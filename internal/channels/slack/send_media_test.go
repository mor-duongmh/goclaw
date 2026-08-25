package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	slackapi "github.com/slack-go/slack"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/config"
)

// slackAPIRecorder stands in for the Slack Web API, recording which methods the
// channel called so media delivery can be asserted without a live workspace.
type slackAPIRecorder struct {
	mu       sync.Mutex
	calls    []string
	posted   []string
	threadTS map[string]string // API method -> thread_ts it was called with
	channel  map[string]string // API method -> channel it was called with
	server   *httptest.Server
}

func newSlackAPIRecorder() *slackAPIRecorder {
	rec := &slackAPIRecorder{threadTS: make(map[string]string), channel: make(map[string]string)}
	mux := http.NewServeMux()

	record := func(method string, r *http.Request) {
		_ = r.ParseForm()
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.calls = append(rec.calls, method)
		if ts := r.Form.Get("thread_ts"); ts != "" {
			rec.threadTS[method] = ts
		}
		for _, field := range []string{"channel", "channel_id"} {
			if ch := r.Form.Get(field); ch != "" {
				rec.channel[method] = ch
				break
			}
		}
		if method == "chat.postMessage" {
			rec.posted = append(rec.posted, r.Form.Get("text"))
		}
	}

	writeJSON := func(w http.ResponseWriter, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}

	mux.HandleFunc("/files.getUploadURLExternal", func(w http.ResponseWriter, r *http.Request) {
		record("files.getUploadURLExternal", r)
		writeJSON(w, map[string]any{
			"ok":         true,
			"upload_url": rec.server.URL + "/upload-target",
			"file_id":    "F1",
		})
	})
	mux.HandleFunc("/upload-target", func(w http.ResponseWriter, r *http.Request) {
		record("upload-target", r)
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("/files.completeUploadExternal", func(w http.ResponseWriter, r *http.Request) {
		record("files.completeUploadExternal", r)
		writeJSON(w, map[string]any{
			"ok":    true,
			"files": []map[string]any{{"id": "F1", "title": "report.txt"}},
		})
	})
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		record("chat.postMessage", r)
		writeJSON(w, map[string]any{"ok": true, "channel": "C1", "ts": "1700000001.0"})
	})
	mux.HandleFunc("/chat.update", func(w http.ResponseWriter, r *http.Request) {
		record("chat.update", r)
		writeJSON(w, map[string]any{"ok": true, "channel": "C1", "ts": "1700000000.0"})
	})
	mux.HandleFunc("/chat.delete", func(w http.ResponseWriter, r *http.Request) {
		record("chat.delete", r)
		writeJSON(w, map[string]any{"ok": true, "channel": "C1", "ts": "1700000000.0"})
	})

	rec.server = httptest.NewServer(mux)
	return rec
}

func (r *slackAPIRecorder) called(method string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c == method {
			return true
		}
	}
	return false
}

func (r *slackAPIRecorder) threadFor(method string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.threadTS[method]
}

func (r *slackAPIRecorder) channelFor(method string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.channel[method]
}

func (r *slackAPIRecorder) postedTexts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.posted...)
}

// newTestChannelWithAPI builds a running channel wired to the recorder, with a
// pending "Thinking..." placeholder for chatID — the state every real inbound
// message leaves behind.
func newTestChannelWithAPI(t *testing.T, rec *slackAPIRecorder, chatID string) *Channel {
	t.Helper()

	ch, err := New(config.SlackConfig{
		BotToken: "xoxb-test",
		AppToken: "xapp-test",
	}, bus.New(), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ch.api = slackapi.New("xoxb-test", slackapi.OptionAPIURL(rec.server.URL+"/"))
	ch.SetRunning(true)
	ch.placeholders.Store(chatID, "1700000000.0")
	return ch
}

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

// A reply carrying both text and a file must upload the file. The placeholder
// edit path returns as soon as chat.update succeeds, so media handled after it
// would be silently dropped and the user would only see the filename in text.
func TestSendUploadsMediaWhenPlaceholderExists(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()

	ch := newTestChannelWithAPI(t, rec, "C1")
	path := writeTempFile(t, "report.txt", "hello")

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:   "C1",
		Content:  "Here is report.txt",
		Media:    []bus.MediaAttachment{{URL: path, ContentType: "text/plain"}},
		Metadata: map[string]string{"placeholder_key": "C1"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if !rec.called("files.getUploadURLExternal") || !rec.called("files.completeUploadExternal") {
		t.Fatalf("file was not uploaded; calls=%v", rec.calls)
	}
	if !rec.called("chat.delete") {
		t.Fatalf("placeholder was not removed before media send; calls=%v", rec.calls)
	}
	if _, ok := ch.placeholders.Load("C1"); ok {
		t.Fatal("placeholder entry still present after media send")
	}

	texts := rec.postedTexts()
	if len(texts) != 1 || !strings.Contains(texts[0], "Here is report.txt") {
		t.Fatalf("expected the reply text to be posted once, got %v", texts)
	}
}

// A file with no accompanying text must still reach the channel: an empty
// Content alone is not a NO_REPLY signal.
func TestSendDeliversMediaOnlyMessage(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()

	ch := newTestChannelWithAPI(t, rec, "C1")
	path := writeTempFile(t, "chart.png", "png-bytes")

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:   "C1",
		Media:    []bus.MediaAttachment{{URL: path, ContentType: "image/png", Caption: "Monthly chart"}},
		Metadata: map[string]string{"placeholder_key": "C1"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if !rec.called("files.completeUploadExternal") {
		t.Fatalf("media-only message was dropped; calls=%v", rec.calls)
	}
	if texts := rec.postedTexts(); len(texts) != 0 {
		t.Fatalf("expected no extra text message, got %v", texts)
	}
}

// No text and no media stays a NO_REPLY: drop the placeholder, send nothing.
func TestSendNoReplyDeletesPlaceholder(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()

	ch := newTestChannelWithAPI(t, rec, "C1")

	if err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:   "C1",
		Metadata: map[string]string{"placeholder_key": "C1"},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if !rec.called("chat.delete") {
		t.Fatalf("placeholder not deleted; calls=%v", rec.calls)
	}
	if rec.called("chat.postMessage") || rec.called("files.completeUploadExternal") {
		t.Fatalf("NO_REPLY should send nothing; calls=%v", rec.calls)
	}
}

// Text-only replies keep editing the placeholder in place.
func TestSendTextOnlyEditsPlaceholder(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()

	ch := newTestChannelWithAPI(t, rec, "C1")

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:   "C1",
		Content:  "done",
		Metadata: map[string]string{"placeholder_key": "C1"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if !rec.called("chat.update") {
		t.Fatalf("expected placeholder edit; calls=%v", rec.calls)
	}
	if rec.called("chat.postMessage") {
		t.Fatalf("text-only reply should not post a new message; calls=%v", rec.calls)
	}
}

// Tool-initiated sends (message MEDIA:/send_file) carry the composite local key
// but no explicit thread field. The file still belongs in the thread the run
// started from, not at the channel root.
func TestSendDerivesThreadFromLocalKey(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()

	ch := newTestChannelWithAPI(t, rec, "C1:thread:1700000000.000100")
	path := writeTempFile(t, "report.txt", "hello")

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:   "C1",
		Content:  "here it is",
		Media:    []bus.MediaAttachment{{URL: path}},
		Metadata: map[string]string{"local_key": "C1:thread:1700000000.000100", "placeholder_key": "C1"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := rec.threadFor("files.completeUploadExternal"); got != "1700000000.000100" {
		t.Fatalf("file uploaded with thread_ts %q, want the run's thread", got)
	}
	if got := rec.threadFor("chat.postMessage"); got != "1700000000.000100" {
		t.Fatalf("text posted with thread_ts %q, want the run's thread", got)
	}
}

// An explicit thread field still wins over anything derivable from the key.
func TestSendPrefersExplicitThreadMetadata(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()

	ch := newTestChannelWithAPI(t, rec, "C1")
	path := writeTempFile(t, "report.txt", "hello")

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID: "C1",
		Media:  []bus.MediaAttachment{{URL: path}},
		Metadata: map[string]string{
			"message_thread_id": "1700000000.000200",
			"local_key":         "C1:thread:1700000000.000100",
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := rec.threadFor("files.completeUploadExternal"); got != "1700000000.000200" {
		t.Fatalf("thread_ts = %q, want the explicit metadata value", got)
	}
}

// newPlainChannel builds a running channel with no placeholder stored.
func newPlainChannel(t *testing.T, rec *slackAPIRecorder) *Channel {
	t.Helper()

	ch, err := New(config.SlackConfig{BotToken: "xoxb-test", AppToken: "xapp-test"}, bus.New(), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch.api = slackapi.New("xoxb-test", slackapi.OptionAPIURL(rec.server.URL+"/"))
	ch.SetRunning(true)
	return ch
}

// Interim sends (quick ack, progress replies, retry notices) are published with
// the run's composite local key as ChatID. Slack only accepts the bare channel,
// so it has to be recovered from the key.
func TestSendResolvesChannelFromCompositeChatID(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()

	ch := newPlainChannel(t, rec)

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "C1:thread:1700000000.000100",
		Content: "Reading report.txt...",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := rec.channelFor("chat.postMessage"); got != "C1" {
		t.Fatalf("posted to channel %q, want the bare channel ID", got)
	}
	if got := rec.threadFor("chat.postMessage"); got != "1700000000.000100" {
		t.Fatalf("posted with thread_ts %q, want the run's thread", got)
	}
}

// An interim message must not consume the placeholder the final answer becomes.
// Those messages carry the run's local key as ChatID and no placeholder_key, so
// keying the placeholder off ChatID would let the first of them eat it.
func TestInterimMessageDoesNotConsumePlaceholder(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()

	ch := newTestChannelWithAPI(t, rec, "C1:thread:1700000000.000100")

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "C1:thread:1700000000.000100",
		Content: "Reading report.txt...",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if rec.called("chat.update") {
		t.Fatalf("interim message edited the placeholder; calls=%v", rec.calls)
	}
	if _, ok := ch.placeholders.Load("C1:thread:1700000000.000100"); !ok {
		t.Fatal("placeholder was consumed by the interim message")
	}

	// The final answer names the placeholder, so it still edits it in place.
	err = ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:   "C1",
		Content:  "Done.",
		Metadata: map[string]string{"placeholder_key": "C1:thread:1700000000.000100"},
	})
	if err != nil {
		t.Fatalf("Send final: %v", err)
	}
	if !rec.called("chat.update") {
		t.Fatalf("final answer did not edit the placeholder; calls=%v", rec.calls)
	}
}

// A retry notice edits the placeholder when there is one.
func TestRetryNoticeEditsPlaceholderWhenPresent(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()

	ch := newTestChannelWithAPI(t, rec, "C1")

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:   "C1",
		Content:  "Provider busy, retrying... (1/3)",
		Metadata: map[string]string{"placeholder_update": "true"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if !rec.called("chat.update") {
		t.Fatalf("expected a placeholder edit; calls=%v", rec.calls)
	}
	if rec.called("chat.postMessage") {
		t.Fatalf("retry notice should not post a new message when a placeholder exists; calls=%v", rec.calls)
	}
}
