package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// drainOutbound reads all buffered outbound messages with a short per-read
// timeout. A pre-cancelled context cannot be used: select picks randomly when
// both the message and Done are ready.
func drainOutbound(mb *bus.MessageBus) []bus.OutboundMessage {
	var out []bus.OutboundMessage
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		msg, ok := mb.SubscribeOutbound(ctx)
		cancel()
		if !ok {
			return out
		}
		out = append(out, msg)
	}
}

func bridgeCtx(threadedLocalKey string, threadID string) context.Context {
	ctx := context.Background()
	ctx = tools.WithToolChannel(ctx, "slack")
	ctx = tools.WithToolChatID(ctx, "D1")
	ctx = tools.WithToolPeerKind(ctx, "direct")
	if threadedLocalKey != "" {
		ctx = tools.WithToolLocalKey(ctx, threadedLocalKey)
	}
	if threadID != "" {
		ctx = tools.WithToolThreadID(ctx, threadID)
	}
	return ctx
}

// Agents on the Claude CLI provider run their tools through the MCP bridge, so
// send_file attachments are forwarded from here rather than from the message
// tool. The file belongs to the turn being answered and must carry the run's
// routing, otherwise it lands at the chat root while the answer sits in a thread.
func TestForwardMediaCarriesThreadRouting(t *testing.T) {
	mb := bus.New()
	ctx := bridgeCtx("D1:thread:1700000000.000100", "")

	forwardMediaToOutbound(ctx, mb, "send_file", &tools.Result{
		Media: []bus.MediaFile{{Path: "/tmp/report.txt", MimeType: "text/plain"}},
	})

	got := drainOutbound(mb)
	if len(got) != 1 {
		t.Fatalf("outbound count: got %d want 1 (%+v)", len(got), got)
	}
	if got[0].Metadata["local_key"] != "D1:thread:1700000000.000100" {
		t.Fatalf("local_key = %q, want the run's local key", got[0].Metadata["local_key"])
	}
}

// A top-level channel message has a thread-less local key while the reply opens
// the thread, so the thread ID is the only routing that knows where it goes.
func TestForwardMediaCarriesReplyThreadOpenedByTheReply(t *testing.T) {
	mb := bus.New()
	ctx := bridgeCtx("D1", "1700000000.000100")

	forwardMediaToOutbound(ctx, mb, "send_file", &tools.Result{
		Media: []bus.MediaFile{{Path: "/tmp/report.txt", MimeType: "text/plain"}},
	})

	got := drainOutbound(mb)
	if len(got) != 1 {
		t.Fatalf("outbound count: got %d want 1 (%+v)", len(got), got)
	}
	if got[0].Metadata["message_thread_id"] != "1700000000.000100" {
		t.Fatalf("message_thread_id = %q, want the reply thread", got[0].Metadata["message_thread_id"])
	}
}

// Group sends keep the group_id the Zalo adapter needs alongside the routing.
func TestForwardMediaKeepsGroupIDAlongsideRouting(t *testing.T) {
	mb := bus.New()
	ctx := context.Background()
	ctx = tools.WithToolChannel(ctx, "slack")
	ctx = tools.WithToolChatID(ctx, "C1")
	ctx = tools.WithToolPeerKind(ctx, "group")
	ctx = tools.WithToolLocalKey(ctx, "C1:thread:1700000000.000100")

	forwardMediaToOutbound(ctx, mb, "send_file", &tools.Result{
		Media: []bus.MediaFile{{Path: "/tmp/report.txt", MimeType: "text/plain"}},
	})

	got := drainOutbound(mb)
	if len(got) != 1 {
		t.Fatalf("outbound count: got %d want 1 (%+v)", len(got), got)
	}
	if got[0].Metadata["group_id"] != "C1" {
		t.Fatalf("group_id = %q, want the chat ID", got[0].Metadata["group_id"])
	}
	if got[0].Metadata["local_key"] != "C1:thread:1700000000.000100" {
		t.Fatalf("local_key = %q, want the run's local key", got[0].Metadata["local_key"])
	}
}
