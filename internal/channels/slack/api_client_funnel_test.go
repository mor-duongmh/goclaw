package slack

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/config"
)

// wireCall is one request as the CLIENT side saw it. A deadline lives only in
// the request context, so no fake server can observe it -- only a transport
// installed on the Slack client can, which is why the funnel exists.
type wireCall struct {
	path        string
	budget      time.Duration
	hasDeadline bool
}

type wireLog struct {
	mu    sync.Mutex
	calls []wireCall
}

func (w *wireLog) record(req *http.Request) {
	call := wireCall{path: req.URL.Path}
	if dl, ok := req.Context().Deadline(); ok {
		call.hasDeadline = true
		call.budget = time.Until(dl)
	}
	w.mu.Lock()
	w.calls = append(w.calls, call)
	w.mu.Unlock()
}

func (w *wireLog) find(path string) (wireCall, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.calls {
		if c.path == path {
			return c, true
		}
	}
	return wireCall{}, false
}

func (w *wireLog) paths() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.calls))
	for _, c := range w.calls {
		out = append(out, c.path)
	}
	return out
}

// okSlack answers every endpoint the bounded send paths touch, plus the
// socket-mode connect call, always successfully.
func okSlack(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		switch r.URL.Path {
		case "/files.getUploadURLExternal":
			body = map[string]any{"ok": true, "upload_url": srv.URL + "/upload-target", "file_id": "F1"}
		case "/files.completeUploadExternal":
			body = map[string]any{"ok": true, "files": []map[string]any{{"id": "F1", "title": "f.txt"}}}
		case "/apps.connections.open":
			body = map[string]any{"ok": true, "url": "wss://127.0.0.1:1/link"}
		default:
			body = map[string]any{"ok": true, "channel": "C1", "ts": "1700000001.0"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func funnelChannel(t *testing.T, srv *httptest.Server, log *wireLog) *Channel {
	t.Helper()
	ch, err := New(config.SlackConfig{BotToken: "xoxb-test", AppToken: "xapp-test"}, bus.New(), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch.api = newSlackAPIClientWithPolicy("xoxb-test", slackRetryConfig(), log.record,
		slackapi.OptionAPIURL(srv.URL+"/"))
	ch.SetRunning(true)
	return ch
}

// TestSlackBoundedSendsCarryDeadline is the guard the timeout constants have
// been missing: it fails both when a context.WithTimeout wrapper is deleted (no
// deadline reaches the wire) and when a budget is inflated past the ceilings
// below. The ceilings are literals on purpose -- comparing against the constant
// under test would move with the mutation and assert nothing.
func TestSlackBoundedSendsCarryDeadline(t *testing.T) {
	const (
		textCeiling  = 90 * time.Second
		mediaCeiling = 20 * time.Minute
	)

	tmp := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(tmp, []byte("payload"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	cases := []struct {
		name   string
		seed   bool // pre-seed a placeholder for "C1"
		msg    bus.OutboundMessage
		expect map[string]time.Duration // endpoint -> ceiling
	}{
		{
			name:   "text",
			msg:    bus.OutboundMessage{ChatID: "C1", Content: "hello"},
			expect: map[string]time.Duration{"/chat.postMessage": textCeiling},
		},
		{
			name: "placeholder edit",
			seed: true,
			msg: bus.OutboundMessage{ChatID: "C1", Content: "hello",
				Metadata: map[string]string{"placeholder_key": "C1"}},
			expect: map[string]time.Duration{"/chat.update": textCeiling},
		},
		{
			name: "placeholder retry notice",
			seed: true,
			msg: bus.OutboundMessage{ChatID: "C1", Content: "retrying",
				Metadata: map[string]string{"placeholder_update": "true"}},
			expect: map[string]time.Duration{"/chat.update": textCeiling},
		},
		{
			name: "no reply deletes placeholder",
			seed: true,
			msg: bus.OutboundMessage{ChatID: "C1",
				Metadata: map[string]string{"placeholder_key": "C1"}},
			expect: map[string]time.Duration{"/chat.delete": textCeiling},
		},
		{
			name: "media upload",
			msg: bus.OutboundMessage{ChatID: "C1",
				Media: []bus.MediaAttachment{{URL: tmp}}},
			expect: map[string]time.Duration{
				"/files.getUploadURLExternal":   mediaCeiling,
				"/upload-target":                mediaCeiling,
				"/files.completeUploadExternal": mediaCeiling,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := okSlack(t)
			log := &wireLog{}
			ch := funnelChannel(t, srv, log)
			if tc.seed {
				ch.placeholders.Store("C1", "1700000000.0")
			}
			if err := ch.Send(context.Background(), tc.msg); err != nil {
				t.Fatalf("Send: %v", err)
			}
			for endpoint, ceiling := range tc.expect {
				call, ok := log.find(endpoint)
				if !ok {
					t.Fatalf("no request to %s; saw %v", endpoint, log.paths())
				}
				if !call.hasDeadline {
					t.Fatalf("%s reached the wire with no deadline: an unbounded "+
						"Retry-After sleep there stalls the whole outbound shard", endpoint)
				}
				if call.budget > ceiling {
					t.Fatalf("%s budget %v exceeds ceiling %v", endpoint, call.budget, ceiling)
				}
			}
		})
	}
}

// TestSlackSocketModeSharesRetryTransport pins the two inheritance paths that no
// source-level check on the receiver expression can see: socketmode.Client
// embeds a COPY of slack.Client by value, and apps.connections.open is issued
// from inside the library. Both must land on the same observed transport, which
// is what makes that transport a complete place to enforce deadlines.
func TestSlackSocketModeSharesRetryTransport(t *testing.T) {
	srv := okSlack(t)
	log := &wireLog{}
	api := newSlackAPIClientWithPolicy("xoxb-test", slackRetryConfig(), log.record,
		slackapi.OptionAppLevelToken("xapp-test"), slackapi.OptionAPIURL(srv.URL+"/"))
	sm := socketmode.New(api, socketmode.OptionDebug(false))

	// Promoted method: names receiver "sm", runs on api's transport.
	if _, _, err := sm.PostMessage("C1", slackapi.MsgOptionText("hi", false)); err != nil {
		t.Fatalf("sm.PostMessage: %v", err)
	}
	// The library's own call: no call site for it exists in this repo.
	if _, _, err := sm.OpenContext(context.Background()); err != nil {
		t.Fatalf("sm.OpenContext: %v", err)
	}

	for _, want := range []string{"/chat.postMessage", "/apps.connections.open"} {
		if _, ok := log.find(want); !ok {
			t.Fatalf("%s bypassed the observed transport; saw %v", want, log.paths())
		}
	}
}

// TestSlackClientsComeFromOneConstructor keeps the enumeration complete by
// construction: if every Slack client in this package is built by
// newSlackAPIClient, then every call on one -- whatever the receiver is named,
// including calls the library makes itself -- inherits the retry policy and the
// observed transport. A check keyed on receiver names (c.api, c.userAPI) cannot
// do this: socketmode's embedded copy and a local alias both defeat it.
func TestSlackClientsComeFromOneConstructor(t *testing.T) {
	const funnelFile = "retry_config.go"

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("parsed no packages")
	}

	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			base := filepath.Base(name)
			// Import alias for slack-go varies per file; resolve it instead of
			// assuming "slackapi", or renaming the alias would evade this.
			alias := ""
			for _, imp := range file.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				if path != "github.com/slack-go/slack" {
					continue
				}
				alias = "slack"
				if imp.Name != nil {
					alias = imp.Name.Name
				}
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				pos := fset.Position(call.Pos())
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					id, ok := fn.X.(*ast.Ident)
					if ok && alias != "" && id.Name == alias && fn.Sel.Name == "New" && base != funnelFile {
						t.Errorf("%s: builds a Slack client directly; use newSlackAPIClient "+
							"so the retry policy and transport cannot be bypassed", pos)
					}
				case *ast.Ident:
					if fn.Name == "newSlackAPIClientWithPolicy" && base != funnelFile {
						t.Errorf("%s: newSlackAPIClientWithPolicy is test-only; production "+
							"must call newSlackAPIClient, which has no policy argument to get wrong", pos)
					}
				}
				return true
			})
		}
	}
}

// TestSlackSingleCallsCarryDeadline covers the calls that are NOT part of a
// logical send and therefore budget themselves: reactions and the display-name
// lookup. They matter because they run inline on the single Socket Mode
// consumer, so an unbounded Retry-After sleep in one of them freezes every
// later inbound event for that channel — the display-name lookup especially,
// since it needs no coincidence to be reached.
//
// The ceiling is a literal on purpose: comparing against slackAPICallTimeout
// would move with a mutation of that constant and assert nothing.
func TestSlackSingleCallsCarryDeadline(t *testing.T) {
	const ceiling = 45 * time.Second

	t.Run("reactions", func(t *testing.T) {
		srv := okSlack(t)
		log := &wireLog{}
		ch := funnelChannel(t, srv, log)
		ch.config.ReactionLevel = "full"

		// Seed a different current emoji so the remove+add pair both fire.
		ch.reactions.Store("C1:1700000000.0", &reactionState{currentEmoji: "eyes"})

		if err := ch.OnReactionEvent(context.Background(), "C1", "1700000000.0", "thinking"); err != nil {
			t.Fatalf("OnReactionEvent: %v", err)
		}
		for _, endpoint := range []string{"/reactions.remove", "/reactions.add"} {
			call, ok := log.find(endpoint)
			if !ok {
				t.Fatalf("no request to %s; saw %v", endpoint, log.paths())
			}
			if !call.hasDeadline {
				t.Fatalf("%s reached the wire with no deadline: it holds st.mu on the "+
					"Socket Mode consumer, so an unbounded retry stalls inbound events", endpoint)
			}
			if call.budget > ceiling {
				t.Fatalf("%s budget %v exceeds ceiling %v", endpoint, call.budget, ceiling)
			}
		}
	})

	t.Run("clear reaction", func(t *testing.T) {
		srv := okSlack(t)
		log := &wireLog{}
		ch := funnelChannel(t, srv, log)
		ch.reactions.Store("C1:1700000000.0", &reactionState{currentEmoji: "eyes"})

		if err := ch.ClearReaction(context.Background(), "C1", "1700000000.0"); err != nil {
			t.Fatalf("ClearReaction: %v", err)
		}
		call, ok := log.find("/reactions.remove")
		if !ok {
			t.Fatalf("no request to /reactions.remove; saw %v", log.paths())
		}
		if !call.hasDeadline {
			t.Fatal("/reactions.remove reached the wire with no deadline")
		}
		if call.budget > ceiling {
			t.Fatalf("/reactions.remove budget %v exceeds ceiling %v", call.budget, ceiling)
		}
	})

	t.Run("display name lookup", func(t *testing.T) {
		srv := okSlack(t)
		log := &wireLog{}
		ch := funnelChannel(t, srv, log)

		// The response shape does not matter; reaching the wire bounded does.
		_ = ch.resolveDisplayName("U0123ABCD")

		call, ok := log.find("/users.info")
		if !ok {
			t.Fatalf("no request to /users.info; saw %v", log.paths())
		}
		if !call.hasDeadline {
			t.Fatal("/users.info reached the wire with no deadline: this is the one " +
				"inbound call whose stall needs no coincidence to be reached")
		}
		if call.budget > ceiling {
			t.Fatalf("/users.info budget %v exceeds ceiling %v", call.budget, ceiling)
		}
	})
}

// TestSlackAPICallsUseContextVariants is the structural guard the behavioural
// tests cannot be: it fails the moment ANY call on a Slack client uses a
// non-Context method. Those methods substitute context.Background() inside
// slack-go, so the retry loop's Retry-After sleep — which slack-go takes from
// the header UNCAPPED — becomes uninterruptible, and the caller has no deadline
// to bound it with.
//
// Behavioural tests only cover the paths they exercise; a new unbounded call
// site added tomorrow would pass every one of them. This catches it at compile
// time of the test suite instead.
func TestSlackAPICallsUseContextVariants(t *testing.T) {
	// Receiver expressions that hold a Slack client in this package.
	clients := map[string]bool{"api": true, "userAPI": true}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				// Match x.api.Method(...) / x.userAPI.Method(...).
				recv, ok := sel.X.(*ast.SelectorExpr)
				if !ok || !clients[recv.Sel.Name] {
					return true
				}
				if strings.HasSuffix(sel.Sel.Name, "Context") {
					return true
				}
				t.Errorf("%s: %s is a non-Context Slack call; it runs on "+
					"context.Background() inside slack-go, so its uncapped Retry-After "+
					"sleep cannot be bounded or cancelled. Use %sContext.",
					fset.Position(call.Pos()), sel.Sel.Name, sel.Sel.Name)
				return true
			})
		}
	}
}
