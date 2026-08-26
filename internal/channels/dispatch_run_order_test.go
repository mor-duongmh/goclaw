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

// A run addresses its intermediate messages (reasoning bubbles, quick ack,
// progress, retry notice, block replies) by its composite local key, while the
// final answer is published with the bare chat id. Those are two different
// shard keys, so the two streams land on different dispatch workers and race —
// the answer can be delivered before the reasoning that led to it. ShardKey
// pins them to one worker without changing how either message is addressed.
func TestDispatchOutbound_RunIntermediatesShareAnswerShard(t *testing.T) {
	shards := outboundShardCount()
	answerChat, localKey := distinctShardChats(t, "telegram-test", shards)

	mb := bus.New()
	mgr := NewManager(mb)
	ch := newRecorderChannel("telegram-test")
	ch.blockOn = localKey // hold the reasoning bubble hostage
	mgr.channels["telegram-test"] = ch

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.dispatchOutbound(ctx)

	delivery := ResolveReasoningDelivery(ReasoningDeliveryAlwaysBubbles, nil)
	mgr.RegisterRunWithDelivery("run-1", "telegram-test", localKey, answerChat, "msg-1", nil,
		uuid.Nil, false, false, true, ResolvedChatBehavior{}, DeliveryRuntime{}, delivery)

	mgr.HandleAgentEvent(protocol.ChatEventThinking, "run-1", map[string]string{"content": "why 42"})
	mgr.HandleAgentEvent(protocol.AgentEventRunCompleted, "run-1", nil)

	// Give the bubble time to reach its shard worker and block there.
	if !waitFor(t, 2*time.Second, func() bool { return ch.blocked() }) {
		close(ch.release)
		t.Fatal("reasoning bubble never reached a shard worker")
	}

	mb.PublishOutbound(bus.OutboundMessage{
		Channel: "telegram-test",
		ChatID:  answerChat,
		Content: "answer",
	})

	// With one shard key the answer is queued behind the blocked bubble. If it
	// gets delivered while the bubble is still stuck, the two streams are on
	// different workers.
	if waitFor(t, 300*time.Millisecond, func() bool { return len(ch.contentsFor(answerChat)) > 0 }) {
		close(ch.release)
		t.Fatal("answer overtook the run's reasoning bubble — intermediates dispatch on a different shard")
	}

	close(ch.release)
	if !waitFor(t, 5*time.Second, func() bool { return ch.count() >= 2 }) {
		t.Fatalf("only %d messages delivered after release", ch.count())
	}
	if got := ch.allContents(); len(got) < 2 || !strings.Contains(got[0], "why 42") || got[len(got)-1] != "answer" {
		t.Fatalf("delivery order = %q, want reasoning before answer", got)
	}
}

// Every message the Manager publishes for a run must carry the answer's shard
// key, not just reasoning bubbles: quick ack, progress, retry notices and block
// replies all address the run by its local key and all race the answer.
func TestManagerRunPublishesCarryAnswerShardKey(t *testing.T) {
	mb := bus.New()
	mgr := NewManager(mb)
	mgr.RegisterChannel("test", &chatBehaviorTestChannel{name: "test"})

	const localKey = "C1:thread:1700000000.000100"
	const answerChat = "C1"

	delivery := ResolveReasoningDelivery(ReasoningDeliveryAlwaysBubbles, nil)
	mgr.RegisterRunWithDelivery("run-1", "test", localKey, answerChat, "msg-1",
		map[string]string{"local_key": localKey}, uuid.Nil, false, false, true,
		ResolvedChatBehavior{}, DeliveryRuntime{}, delivery)

	mgr.HandleAgentEvent(protocol.ChatEventThinking, "run-1", map[string]string{"content": "thinking"})
	mgr.HandleAgentEvent(protocol.AgentEventRunRetrying, "run-1", map[string]string{"attempt": "1", "maxAttempts": "3"})
	mgr.HandleAgentEvent(protocol.AgentEventRunCompleted, "run-1", nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	seen := 0
	for {
		msg, ok := mb.SubscribeOutbound(ctx)
		if !ok {
			break
		}
		seen++
		if msg.ShardKey != answerChat {
			t.Fatalf("message %d (%q) ShardKey = %q, want %q — it would dispatch on a different worker than the answer",
				seen, msg.Content, msg.ShardKey, answerChat)
		}
		if msg.ChatID != localKey {
			t.Fatalf("message %d ChatID = %q, want the run's local key %q — addressing must not change", seen, msg.ChatID, localKey)
		}
	}
	if seen == 0 {
		t.Fatal("no run messages were published")
	}
}

// A run registered through the plain helpers keeps dispatching on its own
// ChatID, so channels that never split the two keys are unaffected.
func TestRegisterRunDefaultsShardKeyToChatID(t *testing.T) {
	mb := bus.New()
	mgr := NewManager(mb)
	mgr.RegisterChannel("test", &chatBehaviorTestChannel{name: "test"})

	delivery := ResolveReasoningDelivery(ReasoningDeliveryAlwaysBubbles, nil)
	mgr.RegisterRunWithBehavior("run-1", "test", "chat-1", "msg-1", nil, uuid.Nil, false, false, true, ResolvedChatBehavior{}, delivery)

	mgr.HandleAgentEvent(protocol.ChatEventThinking, "run-1", map[string]string{"content": "thinking"})
	mgr.HandleAgentEvent(protocol.AgentEventRunCompleted, "run-1", nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	msg, ok := mb.SubscribeOutbound(ctx)
	if !ok {
		t.Fatal("no bubble published")
	}
	if msg.ShardKey != "chat-1" {
		t.Fatalf("ShardKey = %q, want the run ChatID", msg.ShardKey)
	}
}

// Dispatch prefers ShardKey when set and falls back to ChatID otherwise.
func TestOutboundShardKeySelection(t *testing.T) {
	cases := []struct {
		msg  bus.OutboundMessage
		want string
	}{
		{bus.OutboundMessage{ChatID: "C1"}, "C1"},
		{bus.OutboundMessage{ChatID: "C1:thread:9", ShardKey: "C1"}, "C1"},
		{bus.OutboundMessage{ChatID: "C1", ShardKey: ""}, "C1"},
	}
	for i, tc := range cases {
		if got := dispatchShardKey(tc.msg); got != tc.want {
			t.Fatalf("case %d: dispatchShardKey = %q, want %q", i, got, tc.want)
		}
	}
	// And the key actually decides the shard.
	shards := outboundShardCount()
	a := bus.OutboundMessage{Channel: "x", ChatID: "local-key", ShardKey: "answer"}
	b := bus.OutboundMessage{Channel: "x", ChatID: "answer"}
	if outboundShardIndex("x", dispatchShardKey(a), shards) != outboundShardIndex("x", dispatchShardKey(b), shards) {
		t.Fatal("ShardKey did not place the message on the answer's shard")
	}
}
