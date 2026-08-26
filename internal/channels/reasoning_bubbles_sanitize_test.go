package channels

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

func bubbleRunForSanitize(t *testing.T) (*Manager, *bus.MessageBus) {
	t.Helper()
	mb := bus.New()
	mgr := NewManager(mb)
	mgr.RegisterChannel("test", &chatBehaviorTestChannel{name: "test"})
	delivery := ResolveReasoningDelivery(ReasoningDeliveryAlwaysBubbles, nil)
	mgr.RegisterRunWithBehavior("run-1", "test", "chat-1", "msg-1", nil, uuid.Nil, false, false, true, ResolvedChatBehavior{}, delivery)
	return mgr, mb
}

func firstBubble(t *testing.T, mb *bus.MessageBus) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, ok := mb.SubscribeOutbound(ctx)
	if !ok {
		t.Fatal("expected a reasoning bubble outbound message")
	}
	return got.Content
}

// Markers arrive split across thinking chunks, which is the normal case: with
// always_bubbles the provider streams reasoning token by token. Sanitizing a
// single chunk would never see "[Tool Result" whole, so the guard has to run
// on the accumulated buffer.
func TestReasoningBubblesStripToolMarkersSplitAcrossChunks(t *testing.T) {
	mgr, mb := bubbleRunForSanitize(t)

	for _, chunk := range []string{
		"Checking the database.\n[Tool Res",
		"ult db_query]\nArgum",
		"ents: {\"sql\":\"select token from secrets\"}\n",
		"So the total is 42.",
	} {
		mgr.HandleAgentEvent(protocol.ChatEventThinking, "run-1", map[string]string{"content": chunk})
	}
	mgr.HandleAgentEvent(protocol.AgentEventRunCompleted, "run-1", nil)

	content := firstBubble(t, mb)
	for _, leaked := range []string{"[Tool Result", "select token from secrets"} {
		if strings.Contains(content, leaked) {
			t.Fatalf("bubble leaked %q: %q", leaked, content)
		}
	}
	if !strings.Contains(content, "So the total is 42.") {
		t.Fatalf("bubble dropped legitimate reasoning: %q", content)
	}
}

func TestReasoningBubblesStripSystemMessageAndMediaPaths(t *testing.T) {
	mgr, mb := bubbleRunForSanitize(t)

	mgr.HandleAgentEvent(protocol.ChatEventThinking, "run-1", map[string]string{
		"content": "Plan set.\n[System Message] Stats: tokens=900\nReply in Vietnamese.\n\nMEDIA:/Users/operator/private/chart.png\nRendering next.",
	})
	mgr.HandleAgentEvent(protocol.AgentEventRunCompleted, "run-1", nil)

	content := firstBubble(t, mb)
	for _, leaked := range []string{"[System Message]", "Reply in Vietnamese.", "MEDIA:", "/Users/operator/private"} {
		if strings.Contains(content, leaked) {
			t.Fatalf("bubble leaked %q: %q", leaked, content)
		}
	}
	if !strings.Contains(content, "Rendering next.") {
		t.Fatalf("bubble dropped legitimate reasoning: %q", content)
	}
}

func TestReasoningBubblesPreserveOrdinaryProse(t *testing.T) {
	mgr, mb := bubbleRunForSanitize(t)

	const reasoning = "The user asks about pricing.\n\nTiers are [1] $10 and [2] $20; `json.Marshal` returns ([]byte, error)."
	mgr.HandleAgentEvent(protocol.ChatEventThinking, "run-1", map[string]string{"content": reasoning})
	mgr.HandleAgentEvent(protocol.AgentEventRunCompleted, "run-1", nil)

	content := firstBubble(t, mb)
	if want := "_Reasoning:_\n" + reasoning; content != want {
		t.Fatalf("bubble content = %q, want %q", content, want)
	}
}

// The buffer emits in 1200-rune bubbles; sanitizing must run before the split
// so a marker in a long reasoning stream is stripped rather than sliced into a
// later bubble.
func TestReasoningBubblesStripMarkersInMultiBubblePayload(t *testing.T) {
	mgr, mb := bubbleRunForSanitize(t)

	filler := strings.Repeat("reasoning prose. ", 100) // ~1700 runes
	mgr.HandleAgentEvent(protocol.ChatEventThinking, "run-1", map[string]string{
		"content": filler + "\n[Tool Result db_query]\n{\"secret\":\"sk-live-xyz\"}\n" + filler,
	})
	mgr.HandleAgentEvent(protocol.AgentEventRunCompleted, "run-1", nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	seen := 0
	for {
		got, ok := mb.SubscribeOutbound(ctx)
		if !ok {
			break
		}
		seen++
		for _, leaked := range []string{"[Tool Result", "sk-live-xyz"} {
			if strings.Contains(got.Content, leaked) {
				t.Fatalf("bubble %d leaked %q", seen, leaked)
			}
		}
		if seen >= 4 {
			break
		}
	}
	if seen < 2 {
		t.Fatalf("expected the payload to span multiple bubbles, got %d", seen)
	}
}
