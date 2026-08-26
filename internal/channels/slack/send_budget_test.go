package slack

import (
	"context"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/config"
)

// budgetProbe sits between slack-go and the network and records the deadline
// every request carried. Absolute deadlines, not remaining time: two calls can
// then be compared for "same budget" without the test having to run long enough
// to measure the decay.
type budgetProbe struct {
	inner interface {
		Do(*http.Request) (*http.Response, error)
	}
	mu   sync.Mutex
	reqs []probedRequest
}

type probedRequest struct {
	path        string
	deadline    time.Time
	hasDeadline bool
}

func (p *budgetProbe) Do(r *http.Request) (*http.Response, error) {
	rec := probedRequest{path: r.URL.Path}
	if dl, ok := r.Context().Deadline(); ok {
		rec.deadline, rec.hasDeadline = dl, true
	}
	p.mu.Lock()
	p.reqs = append(p.reqs, rec)
	p.mu.Unlock()
	return p.inner.Do(r)
}

// requests returns the calls seen on one endpoint in order; "" returns all.
func (p *budgetProbe) requests(path string) []probedRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]probedRequest, 0, len(p.reqs))
	for _, r := range p.reqs {
		if path == "" || r.path == path {
			out = append(out, r)
		}
	}
	return out
}

func newBudgetTestChannel(t *testing.T, s *scriptedSlack) (*Channel, *budgetProbe) {
	t.Helper()

	ch, err := New(config.SlackConfig{BotToken: "xoxb-test", AppToken: "xapp-test"}, bus.New(), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	probe := &budgetProbe{inner: &http.Client{}}
	ch.api = slackapi.New("xoxb-test",
		slackapi.OptionAPIURL(s.server.URL+"/"),
		slackapi.OptionHTTPClient(probe),
		slackapi.OptionRetryConfig(slackRetryConfig()))
	ch.SetRunning(true)
	return ch, probe
}

func tempAttachments(t *testing.T, n int) []bus.MediaAttachment {
	t.Helper()
	dir := t.TempDir()
	out := make([]bus.MediaAttachment, 0, n)
	for i := 0; i < n; i++ {
		path := dir + "/f" + string(rune('a'+i)) + ".txt"
		if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		out = append(out, bus.MediaAttachment{URL: path})
	}
	return out
}

// scriptUploadSuccess makes the fake carry the whole three-step v2 upload. A
// generic ok:true is not enough: UploadFileContext reads upload_url and file_id
// out of the first response and aborts before files.completeUploadExternal
// without them, and the upload URL has to point back at this server, which is
// only known once it is listening.
func scriptUploadSuccess(s *scriptedSlack) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script["/files.getUploadURLExternal"] = []scriptedResponse{{body: map[string]any{
		"ok": true, "upload_url": s.server.URL + "/upload-target", "file_id": "F1"}}}
	s.script["/files.completeUploadExternal"] = []scriptedResponse{{body: map[string]any{
		"ok": true, "files": []map[string]any{{"id": "F1", "title": "f.txt"}}}}}
}

// A reply with several attachments costs ONE budget, not one per attachment, and
// that budget is armed even when the caller hands over no deadline at all. Every
// request the send makes therefore carries the same media budget; re-wrapping
// any API group in a deadline of its own shows up here as a much smaller
// remaining time, and dropping the budget shows up as no deadline.
func TestSlackMediaSendSharesOneBudgetAcrossAttachments(t *testing.T) {
	s := newScriptedSlack(map[string][]scriptedResponse{})
	defer s.server.Close()
	scriptUploadSuccess(s)

	ch, probe := newBudgetTestChannel(t, s)
	// context.Background() on purpose: real sends arrive from the outbound shard
	// without a deadline, so the bound has to come from the channel.
	if err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "C1",
		Content: "caption travels on the same budget",
		Media:   tempAttachments(t, 3),
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := s.attemptsFor("/files.completeUploadExternal"); got != 3 {
		t.Fatalf("completed uploads = %d, want 3", got)
	}

	reqs := probe.requests("")
	if len(reqs) == 0 {
		t.Fatal("no request reached the HTTP client")
	}
	for _, r := range reqs {
		if !r.hasDeadline {
			t.Fatalf("%s ran with no deadline: an uncapped Retry-After would stall the outbound shard", r.path)
		}
		left := time.Until(r.deadline)
		if left < slackMediaSendBudget-time.Minute {
			t.Fatalf("%s had %v left, want the shared media budget (~%v): a per-call budget restarts the clock",
				r.path, left, slackMediaSendBudget)
		}
		// Deliberately a literal, not slackMediaSendBudget: an assertion written
		// in terms of the constant grows with it, so inflating the constant to
		// something that no longer bounds the shard at all would pass.
		if left > 20*time.Minute {
			t.Fatalf("%s had %v left; a media send must stay bounded well under 20m", r.path, left)
		}
	}
}

// The placeholder edit is the only attempt that can fail with the answer still
// undelivered, and the sendChunked below it is the last path that delivers it. It
// must not spend the budget the failed edit was spending, or a slow failure
// leaves the fallback with nothing and the user gets no answer at all. Comparing
// the deadlines the two calls carry proves the split without waiting a budget
// out: the fallback runs on the reserve, which is strictly shorter.
func TestSlackPlaceholderEditFailureGetsSeparateDeliveryBudget(t *testing.T) {
	s := newScriptedSlack(map[string][]scriptedResponse{
		"/chat.update": {{status: http.StatusOK, body: map[string]any{"ok": false, "error": "message_not_found"}}},
	})
	defer s.server.Close()

	ch, probe := newBudgetTestChannel(t, s)
	ch.placeholders.Store("C1", "1700000000.0")

	if err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:   "C1",
		Content:  "the answer",
		Metadata: map[string]string{"placeholder_key": "C1"},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	edits := probe.requests("/chat.update")
	posts := probe.requests("/chat.postMessage")
	if len(edits) != 1 || len(posts) != 1 {
		t.Fatalf("edits = %d, posts = %d; want 1 failed edit then 1 fallback post", len(edits), len(posts))
	}
	if !posts[0].hasDeadline {
		t.Fatal("fallback post ran with no deadline")
	}
	// The 2m ceiling is a literal on purpose: stated in terms of
	// slackSendBudget it would grow with the constant it is meant to bound.
	if left := time.Until(edits[0].deadline); !edits[0].hasDeadline || left <= 0 || left > 2*time.Minute {
		t.Fatalf("placeholder edit had %v left, want a bound well under 2m", left)
	}
	if !posts[0].deadline.Before(edits[0].deadline) {
		t.Fatalf("fallback deadline %v is not earlier than the failed edit's %v: the fallback is still spending the attempt budget it was supposed to be protected from",
			posts[0].deadline, edits[0].deadline)
	}
	if left := time.Until(posts[0].deadline); left <= 0 || left > slackDeliveryReserve {
		t.Fatalf("fallback had %v left, want a fresh reserve of at most %v", left, slackDeliveryReserve)
	}
}

// N failed uploads share ONE delivery reserve. Each failure notice is its own
// postMessage, so a reserve built per notice would let a reply with N
// attachments claim N reserves — exactly the multiplication this budget shape
// exists to remove. Identical deadlines prove one reserve funded them all.
func TestSlackUploadFailureNoticesShareOneReserve(t *testing.T) {
	s := newScriptedSlack(map[string][]scriptedResponse{
		"/files.getUploadURLExternal": {{status: http.StatusOK, body: map[string]any{"ok": false, "error": "invalid_auth"}}},
	})
	defer s.server.Close()

	ch, probe := newBudgetTestChannel(t, s)
	if err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID: "C1",
		Media:  tempAttachments(t, 3),
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	posts := probe.requests("/chat.postMessage")
	if len(posts) != 3 {
		t.Fatalf("failure notices = %d, want one per attachment (3)", len(posts))
	}
	for i, p := range posts {
		if !p.hasDeadline {
			t.Fatalf("notice %d ran with no deadline", i)
		}
		if !p.deadline.Equal(posts[0].deadline) {
			t.Fatalf("notice %d deadline %v != first notice %v: each notice claimed its own reserve",
				i, p.deadline, posts[0].deadline)
		}
	}
}

// The upload body is streamed from disk. slack-go's multipart writer goroutine
// reports on an unbuffered channel that nobody reads once the request itself has
// failed, so an aborted upload leaks that goroutine holding whatever reader it
// was handed: a path leaks a closed file handle, an in-memory reader leaks every
// byte of the attachment.
func TestSlackUploadStreamsFromPath(t *testing.T) {
	att := tempAttachments(t, 1)[0]

	params, err := uploadParams("C1", "1700000000.0", att)
	if err != nil {
		t.Fatalf("uploadParams: %v", err)
	}
	if params.Reader != nil {
		t.Fatal("upload passed an in-memory reader; an aborted upload would leak a goroutine holding the whole file")
	}
	if params.File != att.URL {
		t.Fatalf("File = %q, want the attachment path %q", params.File, att.URL)
	}
	if params.FileSize != len("payload") {
		t.Fatalf("FileSize = %d, want %d", params.FileSize, len("payload"))
	}
}
