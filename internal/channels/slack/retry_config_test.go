package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/config"
)

// scriptedResponse is one canned reply from the fake Slack API.
type scriptedResponse struct {
	status     int
	retryAfter string         // Retry-After header, set only when non-empty
	body       map[string]any // JSON body; defaults to a generic ok:true
	hijackKill bool           // drop the connection to simulate a transport failure
}

// scriptedSlack is a fake Slack API that returns a preset sequence per endpoint
// and counts attempts, so retry behaviour can be asserted without a workspace.
// The last scripted response repeats once the script runs out.
type scriptedSlack struct {
	mu       sync.Mutex
	attempts map[string]int
	script   map[string][]scriptedResponse
	server   *httptest.Server
}

func newScriptedSlack(script map[string][]scriptedResponse) *scriptedSlack {
	s := &scriptedSlack{attempts: map[string]int{}, script: script}
	mux := http.NewServeMux()

	handle := func(endpoint string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			n := s.attempts[endpoint]
			s.attempts[endpoint] = n + 1
			seq := s.script[endpoint]
			s.mu.Unlock()

			resp := scriptedResponse{status: http.StatusOK}
			if len(seq) > 0 {
				if n < len(seq) {
					resp = seq[n]
				} else {
					resp = seq[len(seq)-1]
				}
			}

			if resp.hijackKill {
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, err := hj.Hijack()
					if err == nil {
						_ = conn.Close()
						return
					}
				}
			}

			if resp.retryAfter != "" {
				w.Header().Set("Retry-After", resp.retryAfter)
			}
			w.Header().Set("Content-Type", "application/json")
			if resp.status != 0 && resp.status != http.StatusOK {
				w.WriteHeader(resp.status)
			}
			body := resp.body
			if body == nil {
				body = s.defaultBody(endpoint)
			}
			_ = json.NewEncoder(w).Encode(body)
		}
	}

	for _, ep := range []string{
		"/chat.postMessage", "/chat.update", "/chat.delete",
		"/files.getUploadURLExternal", "/upload-target", "/files.completeUploadExternal",
		"/auth.test", "/reactions.add", "/reactions.remove",
	} {
		mux.HandleFunc(ep, handle(ep))
	}

	s.server = httptest.NewServer(mux)
	return s
}

// defaultBody is the success payload for an endpoint when the script does not
// override it. A generic ok:true is not enough for the three-step v2 upload:
// UploadFileContext reads upload_url and file_id out of the first response and
// aborts before reaching files.completeUploadExternal without them. The upload
// URL has to point back at this server, so it is resolved per request rather
// than baked into a literal.
func (s *scriptedSlack) defaultBody(endpoint string) map[string]any {
	switch endpoint {
	case "/files.getUploadURLExternal":
		return map[string]any{"ok": true, "upload_url": s.server.URL + "/upload-target", "file_id": "F1"}
	case "/files.completeUploadExternal":
		return map[string]any{"ok": true, "files": []map[string]any{{"id": "F1", "title": "f.txt"}}}
	case "/auth.test":
		return map[string]any{"ok": true, "url": "https://t.slack.com/", "team": "T", "user": "bot",
			"team_id": "T1", "user_id": "U1", "bot_id": "B1"}
	case "/reactions.add", "/reactions.remove":
		return map[string]any{"ok": true}
	default:
		return map[string]any{"ok": true, "channel": "C1", "ts": "1700000001.0"}
	}
}

func (s *scriptedSlack) attemptsFor(endpoint string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[endpoint]
}

// newRetryTestChannel builds a running channel whose API client uses the real
// retry policy against the scripted server. No placeholder is pre-seeded, so
// Send takes the plain sendChunked path.
func newRetryTestChannel(t *testing.T, s *scriptedSlack) *Channel {
	t.Helper()

	ch, err := New(config.SlackConfig{BotToken: "xoxb-test", AppToken: "xapp-test"}, bus.New(), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cfg := slackRetryConfig()
	cfg.RetryAfterJitter = 0 // deterministic waits
	cfg.BackoffInitial = 1 * time.Millisecond
	cfg.BackoffMax = 5 * time.Millisecond
	cfg.BackoffJitter = 0
	cfg.Handlers = slackapi.AllBuiltinRetryHandlers(cfg)

	ch.api = slackapi.New("xoxb-test",
		slackapi.OptionAPIURL(s.server.URL+"/"),
		slackapi.OptionRetryConfig(cfg))
	ch.SetRunning(true)
	return ch
}

func textMessage() bus.OutboundMessage {
	return bus.OutboundMessage{ChatID: "C1", Content: "hello"}
}

// --- policy shape ---

func TestSlackRetryConfigExcludesServerErrors(t *testing.T) {
	cfg := slackRetryConfig()
	if cfg.MaxRetries != slackRetryMaxAttempts {
		t.Fatalf("MaxRetries = %d, want %d", cfg.MaxRetries, slackRetryMaxAttempts)
	}
	// AllBuiltinRetryHandlers is connection + 429 only. A 5xx handler would make
	// the non-idempotent chat.postMessage duplicate messages on ambiguous errors.
	if len(cfg.Handlers) != 2 {
		t.Fatalf("handlers = %d, want 2 (connection + rate limit)", len(cfg.Handlers))
	}
}

func TestSlackRetryDisabledByEnv(t *testing.T) {
	t.Setenv(slackRetryDisabledEnv, "true")
	if got := slackRetryConfig().MaxRetries; got != 0 {
		t.Fatalf("MaxRetries = %d, want 0 when %s=true", got, slackRetryDisabledEnv)
	}

	s := newScriptedSlack(map[string][]scriptedResponse{
		"/chat.postMessage": {{status: http.StatusTooManyRequests, retryAfter: "1"}},
	})
	defer s.server.Close()

	ch, err := New(config.SlackConfig{BotToken: "xoxb-test", AppToken: "xapp-test"}, bus.New(), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch.api = slackapi.New("xoxb-test",
		slackapi.OptionAPIURL(s.server.URL+"/"),
		slackapi.OptionRetryConfig(slackRetryConfig()))
	ch.SetRunning(true)

	if err := ch.Send(context.Background(), textMessage()); err == nil {
		t.Fatal("expected error when retries are disabled and Slack returns 429")
	}
	if got := s.attemptsFor("/chat.postMessage"); got != 1 {
		t.Fatalf("attempts = %d, want 1 with retries disabled", got)
	}
}

// TestSlackRetryDisableKnobValues pins which values turn retries off. Anything
// outside this list leaves retries ON, which is the safe default but surprises
// an operator who typed "off" and expected it to take.
func TestSlackRetryDisableKnobValues(t *testing.T) {
	for _, tc := range []struct {
		value    string
		disabled bool
	}{
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"yes", true},
		{" yes ", true},
		{"false", false},
		{"0", false},
		{"off", false},
		{"no", false},
		{"", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(slackRetryDisabledEnv, tc.value)
			got := slackRetryConfig().MaxRetries == 0
			if got != tc.disabled {
				t.Fatalf("%s=%q disabled=%v, want %v", slackRetryDisabledEnv, tc.value, got, tc.disabled)
			}
		})
	}
}

// --- retry behaviour ---

func TestSlackSucceedsFirstTryNoRetry(t *testing.T) {
	s := newScriptedSlack(nil)
	defer s.server.Close()

	ch := newRetryTestChannel(t, s)
	if err := ch.Send(context.Background(), textMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := s.attemptsFor("/chat.postMessage"); got != 1 {
		t.Fatalf("attempts = %d, want exactly 1 on the happy path", got)
	}
}

func TestSlackRetriesOn429AndHonorsRetryAfter(t *testing.T) {
	s := newScriptedSlack(map[string][]scriptedResponse{
		"/chat.postMessage": {
			{status: http.StatusTooManyRequests, retryAfter: "1"},
			{status: http.StatusOK},
		},
	})
	defer s.server.Close()

	ch := newRetryTestChannel(t, s)
	start := time.Now()
	if err := ch.Send(context.Background(), textMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	elapsed := time.Since(start)

	if got := s.attemptsFor("/chat.postMessage"); got != 2 {
		t.Fatalf("attempts = %d, want 2 (429 then success)", got)
	}
	// slack-go floors the 429 wait at one second, so the advertised Retry-After
	// must actually have been observed rather than skipped.
	if elapsed < time.Second {
		t.Fatalf("elapsed = %v, want >= 1s to honor Retry-After", elapsed)
	}
}

// TestSlackCallerDeadlineCapsRetryAfter covers only half the gap: it supplies
// its own 200ms parent, so it proves the parent deadline reaches slack-go's
// retry sleep — not that the channel arms a bound of its own when the caller
// has none. That second half is TestSlackSendArmsBoundedDeadline.
func TestSlackCallerDeadlineCapsRetryAfter(t *testing.T) {
	s := newScriptedSlack(map[string][]scriptedResponse{
		"/chat.postMessage": {{status: http.StatusTooManyRequests, retryAfter: "600"}},
	})
	defer s.server.Close()

	ch := newRetryTestChannel(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := ch.Send(ctx, textMessage())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error once the deadline cut the retry wait short")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("elapsed = %v; the deadline did not cap a 600s Retry-After", elapsed)
	}
}

func TestSlackDoesNotRetryOn5xx(t *testing.T) {
	s := newScriptedSlack(map[string][]scriptedResponse{
		"/chat.postMessage": {{status: http.StatusInternalServerError}},
	})
	defer s.server.Close()

	ch := newRetryTestChannel(t, s)
	if err := ch.Send(context.Background(), textMessage()); err == nil {
		t.Fatal("expected an error on 500")
	}
	// chat.postMessage is not idempotent: a 5xx may mean the message landed, so
	// resending would duplicate it.
	if got := s.attemptsFor("/chat.postMessage"); got != 1 {
		t.Fatalf("attempts = %d, want 1 — 5xx must not be retried", got)
	}
}

func TestSlackDoesNotRetryOnAuthError(t *testing.T) {
	s := newScriptedSlack(map[string][]scriptedResponse{
		"/chat.postMessage": {{
			status: http.StatusOK,
			body:   map[string]any{"ok": false, "error": "invalid_auth"},
		}},
	})
	defer s.server.Close()

	ch := newRetryTestChannel(t, s)
	if err := ch.Send(context.Background(), textMessage()); err == nil {
		t.Fatal("expected an error on invalid_auth")
	}
	// Slack reports auth failures as HTTP 200 with ok:false, so no retry handler
	// matches and the call must not be repeated.
	if got := s.attemptsFor("/chat.postMessage"); got != 1 {
		t.Fatalf("attempts = %d, want 1 — auth failures must not be retried", got)
	}
}

func TestSlackRetriesOnConnectionError(t *testing.T) {
	s := newScriptedSlack(map[string][]scriptedResponse{
		"/chat.postMessage": {
			{hijackKill: true},
			{status: http.StatusOK},
		},
	})
	defer s.server.Close()

	ch := newRetryTestChannel(t, s)
	if err := ch.Send(context.Background(), textMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := s.attemptsFor("/chat.postMessage"); got != 2 {
		t.Fatalf("attempts = %d, want 2 (dropped connection then success)", got)
	}
}

func TestSlackRespectsContextCancel(t *testing.T) {
	s := newScriptedSlack(map[string][]scriptedResponse{
		"/chat.postMessage": {{status: http.StatusTooManyRequests, retryAfter: "30"}},
	})
	defer s.server.Close()

	ch := newRetryTestChannel(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if err := ch.Send(ctx, textMessage()); err == nil {
		t.Fatal("expected an error after cancellation")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("elapsed = %v; cancellation did not interrupt the retry wait", elapsed)
	}
}

// --- deadline observability ---

type slackHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// ctxProbe sits between slack-go's retry wrapper and the network and records the
// deadline carried by every request. It lets a test assert that a send budget
// exists and how large it is without waiting the budget out.
type ctxProbe struct {
	inner slackHTTPDoer
	mu    sync.Mutex
	seen  []time.Duration // time remaining on each request's deadline; -1 = none
}

func (p *ctxProbe) Do(r *http.Request) (*http.Response, error) {
	remaining := time.Duration(-1)
	if dl, ok := r.Context().Deadline(); ok {
		remaining = time.Until(dl)
	}
	p.mu.Lock()
	p.seen = append(p.seen, remaining)
	p.mu.Unlock()
	return p.inner.Do(r)
}

func (p *ctxProbe) firstDeadline(t *testing.T) time.Duration {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.seen) == 0 {
		t.Fatal("no request reached the HTTP client")
	}
	return p.seen[0]
}

// newProbedRetryTestChannel keeps the channel's real budgets. Overriding them
// here is what made the cancellation test vacuous, so the seam is the probe,
// not the budget: tests that need a short deadline pass their own parent ctx.
func newProbedRetryTestChannel(t *testing.T, s *scriptedSlack) (*Channel, *ctxProbe) {
	t.Helper()

	ch, err := New(config.SlackConfig{BotToken: "xoxb-test", AppToken: "xapp-test"}, bus.New(), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	probe := &ctxProbe{inner: &http.Client{}}
	ch.api = slackapi.New("xoxb-test",
		slackapi.OptionAPIURL(s.server.URL+"/"),
		slackapi.OptionHTTPClient(probe),
		slackapi.OptionRetryConfig(slackRetryConfig()))
	ch.SetRunning(true)
	return ch, probe
}

func TestSlackSendArmsBoundedDeadline(t *testing.T) {
	s := newScriptedSlack(nil)
	defer s.server.Close()

	ch, probe := newProbedRetryTestChannel(t, s)
	// context.Background() on purpose: the bound must come from the channel, not
	// the caller. Real sends arrive from the outbound shard without a deadline.
	if err := ch.Send(context.Background(), textMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := probe.firstDeadline(t)
	if got < 0 {
		t.Fatal("chat.postMessage ran with no deadline: an uncapped Retry-After would stall the outbound shard indefinitely")
	}
	if got < 10*time.Second || got > 2*time.Minute {
		t.Fatalf("send deadline = %v, want a bound in [10s, 2m]", got)
	}
}

func TestSlackUploadArmsBoundedDeadline(t *testing.T) {
	s := newScriptedSlack(nil)
	defer s.server.Close()

	dir := t.TempDir()
	path := dir + "/f.txt"
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	ch, probe := newProbedRetryTestChannel(t, s)
	if err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID: "C1",
		Media:  []bus.MediaAttachment{{URL: path}},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := s.attemptsFor("/files.completeUploadExternal"); got != 1 {
		t.Fatalf("completeUploadExternal attempts = %d, want 1 — the fake must carry the whole v2 upload", got)
	}

	got := probe.firstDeadline(t)
	if got < 0 {
		t.Fatal("upload ran with no deadline")
	}
	if got < 10*time.Second || got > 20*time.Minute {
		t.Fatalf("upload deadline = %v, want a bound in [10s, 20m]", got)
	}
}
