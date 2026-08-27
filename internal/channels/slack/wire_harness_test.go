package slack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	slackapi "github.com/slack-go/slack"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/config"
)

// Wire-level test harness for the Slack channel.
//
// Why this exists: before this file there was zero coverage of what the Slack
// channel actually puts on the wire — no test called Send(), sendChunked(),
// PostMessage, or UpdateMessage. These fixtures pin the exact form-encoded
// request body so a formatting migration can prove what changed and what did
// not.
//
// The tests live in package slack (internal) on purpose: c.api is unexported
// and New() never assigns it (it is set only inside Start(), which does a real
// auth.test and spawns goroutines). Assigning c.api directly is the only way to
// exercise Send() without a network.

// wireReq is one captured outbound API call.
type wireReq struct {
	Method string // Slack method name, e.g. "chat.postMessage"
	Form   url.Values
}

// wireServer records every request the SDK makes and answers with canned JSON.
type wireServer struct {
	t    *testing.T
	mu   sync.Mutex
	reqs []wireReq

	// respond returns the JSON response body for a given method and 1-based
	// per-method call index. Nil means "always ok".
	respond func(method string, n int) string
}

const wireOKResponse = `{"ok":true,"channel":"C0","ts":"1700000000.000100","message":{"ts":"1700000000.000100"}}`

func (w *wireServer) handler() http.HandlerFunc {
	perMethod := map[string]int{}

	return func(rw http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/")

		if err := r.ParseForm(); err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}

		// The NUL byte is the sentinel markdownToSlackMrkdwn uses for its
		// placeholder tokens (\x00ST0\x00 and friends). A restore step that
		// misses one — or a chunk boundary landing inside a placeholder — leaks
		// it onto the wire. Checked here so every fixture inherits the guard
		// instead of each one having to remember it.
		for field, values := range r.PostForm {
			for _, v := range values {
				if strings.ContainsRune(v, 0) {
					w.t.Errorf("%s: form field %q carries a NUL byte: %q", method, field, v)
				}
			}
		}

		w.mu.Lock()
		perMethod[method]++
		n := perMethod[method]
		form := url.Values{}
		for k, v := range r.PostForm {
			form[k] = append([]string(nil), v...)
		}
		w.reqs = append(w.reqs, wireReq{Method: method, Form: form})
		respond := w.respond
		w.mu.Unlock()

		body := wireOKResponse
		if respond != nil {
			if custom := respond(method, n); custom != "" {
				body = custom
			}
		}

		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(body))
	}
}

// calls returns a copy of everything captured so far.
func (w *wireServer) calls() []wireReq {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]wireReq(nil), w.reqs...)
}

// methods returns just the ordered method names, for coarse flow assertions.
func (w *wireServer) methods() []string {
	out := []string{}
	for _, r := range w.calls() {
		out = append(out, r.Method)
	}
	return out
}

// callsTo returns only the requests for one method, preserving order.
func (w *wireServer) callsTo(method string) []wireReq {
	out := []wireReq{}
	for _, r := range w.calls() {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

// newWireTestChannel builds a *Channel whose API client points at an httptest
// server. Named to avoid colliding with newTestSlackChannel in
// channel_debounce_test.go, which goes through the real New() and therefore
// leaves c.api nil.
//
// respond may be nil for "every call succeeds".
func newWireTestChannel(t *testing.T, respond func(method string, n int) string) (*Channel, *wireServer) {
	t.Helper()

	ws := &wireServer{t: t, respond: respond}
	srv := httptest.NewServer(ws.handler())
	t.Cleanup(srv.Close)

	ch := &Channel{
		BaseChannel:    channels.NewBaseChannel(channels.TypeSlack, nil, nil),
		config:         config.SlackConfig{},
		debounceTimers: make(map[string]*debounceEntry),
		userCache:      make(map[string]cachedUser),
	}

	// The trailing slash matters: the SDK concatenates the method name straight
	// onto this endpoint. Without it every call 404s in a confusing way.
	ch.api = slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/"))
	ch.SetRunning(true)

	return ch, ws
}

// requireCallCount fails with the captured flow when the count is unexpected,
// so a mismatch is diagnosable without re-running under a debugger.
func requireCallCount(t *testing.T, ws *wireServer, method string, want int) []wireReq {
	t.Helper()
	got := ws.callsTo(method)
	if len(got) != want {
		t.Fatalf("%s calls = %d, want %d (flow: %v)", method, len(got), want, ws.methods())
	}
	return got
}

// firstForm returns the form of call index i for a method, failing if absent.
func firstForm(t *testing.T, ws *wireServer, method string, i int) url.Values {
	t.Helper()
	got := ws.callsTo(method)
	if i >= len(got) {
		t.Fatalf("%s call #%d missing (only %d, flow: %v)", method, i, len(got), ws.methods())
	}
	return got[i].Form
}

// assertNoField guards against a field leaking onto the wire. Used to pin that
// today's payloads carry text and nothing else — no blocks, no markdown_text.
func assertNoField(t *testing.T, form url.Values, field string) {
	t.Helper()
	if v, ok := form[field]; ok {
		t.Errorf("form carries %q = %q, want absent", field, v)
	}
}

// assertContains checks a substring and prints the full value on failure. Wire
// bodies are long; a bare "not found" is useless when diagnosing.
func assertContains(t *testing.T, got, want, what string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("%s missing %q\ngot: %q", what, want, got)
	}
}

// errResponse builds a Slack error body for a given code.
func errResponse(code string) string {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": code})
	return string(b)
}
