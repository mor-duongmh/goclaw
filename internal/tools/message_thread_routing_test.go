package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

func newMediaToolFixture(t *testing.T) (*MessageTool, *bus.MessageBus, string) {
	t.Helper()

	workspace := t.TempDir()
	mediaPath := filepath.Join(workspace, "report.txt")
	if err := os.WriteFile(mediaPath, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	tool := NewMessageTool(workspace, false)
	mb := bus.New()
	tool.SetMessageBus(mb)
	return tool, mb, mediaPath
}

// A channel message that arrives at top level produces a thread-less local key
// while the bot's reply opens a new thread on it. Only message_thread_id knows
// about that thread, so a file sent back must carry it — otherwise it lands at
// the channel root, outside the thread the answer is in.
func TestSendMediaCarriesReplyThreadOpenedByTheReply(t *testing.T) {
	tool, mb, mediaPath := newMediaToolFixture(t)

	ctx := context.Background()
	ctx = WithToolSessionKey(ctx, "agent:a:slack:group:C1")
	ctx = WithToolChannel(ctx, "slack")
	ctx = WithToolChatID(ctx, "C1")
	ctx = WithToolPeerKind(ctx, "group")
	ctx = WithToolLocalKey(ctx, "C1") // top-level message: no thread in the key
	ctx = WithToolThreadID(ctx, "1700000000.000100")

	res := tool.Execute(ctx, map[string]any{
		"action":  "send",
		"channel": "slack",
		"target":  "C1",
		"message": "MEDIA:" + mediaPath,
	})
	if res != nil && res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}

	got := drainBusNow(mb)
	if len(got) != 1 {
		t.Fatalf("outbound count: got %d want 1 (%+v)", len(got), got)
	}
	if got[0].Metadata["message_thread_id"] != "1700000000.000100" {
		t.Fatalf("message_thread_id = %q, want the reply thread", got[0].Metadata["message_thread_id"])
	}
}

// Thread routing describes the current conversation only.
func TestSendMediaOmitsReplyThreadOnCrossTargetForward(t *testing.T) {
	tool, mb, mediaPath := newMediaToolFixture(t)

	ctx := context.Background()
	ctx = WithToolSessionKey(ctx, "agent:a:slack:group:C1")
	ctx = WithToolChannel(ctx, "slack")
	ctx = WithToolChatID(ctx, "C1")
	ctx = WithToolPeerKind(ctx, "group")
	ctx = WithToolThreadID(ctx, "1700000000.000100")

	res := tool.Execute(ctx, map[string]any{
		"action":         "send",
		"channel":        "slack",
		"target":         "C2",
		"message":        "MEDIA:" + mediaPath,
		"forward":        true,
		"forward_reason": "user asked to forward this file to C2",
	})
	if res != nil && res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}

	got := drainBusNow(mb)
	if len(got) == 0 {
		t.Fatal("expected an outbound message")
	}
	if tid := got[0].Metadata["message_thread_id"]; tid != "" {
		t.Fatalf("message_thread_id = %q, want empty for a cross-target forward", tid)
	}
}
