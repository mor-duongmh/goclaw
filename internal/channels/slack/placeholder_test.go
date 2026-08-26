package slack

import (
	"context"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/config"
)

// newConfiguredTestChannel wires a channel with a caller-supplied config to the
// recorder and stores no placeholder, so tests can assert on what the channel
// itself decides to post. newTestChannelWithAPI is kept as-is for the media
// tests, which depend on its pre-seeded placeholder.
func newConfiguredTestChannel(t *testing.T, rec *slackAPIRecorder, cfg config.SlackConfig) *Channel {
	t.Helper()
	cfg.BotToken = "xoxb-test"
	cfg.AppToken = "xapp-test"

	ch, err := New(cfg, bus.New(), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch.api = slackapi.New("xoxb-test", slackapi.OptionAPIURL(rec.server.URL+"/"))
	ch.SetRunning(true)
	return ch
}

func boolPtr(v bool) *bool { return &v }

const testLocalKey = "C1:thread:1700000000.000100"

func TestPlaceholderPostedByDefault(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()
	ch := newConfiguredTestChannel(t, rec, config.SlackConfig{})

	if !ch.shouldPostPlaceholder() {
		t.Fatal("shouldPostPlaceholder() = false with default config")
	}
	ch.postPlaceholder(context.Background(), "C1", testLocalKey, "1700000000.000100")

	if !rec.called("chat.postMessage") {
		t.Fatal("no placeholder posted")
	}
	if texts := rec.postedTexts(); len(texts) != 1 || texts[0] != "Thinking..." {
		t.Fatalf("posted %q, want one \"Thinking...\"", texts)
	}
	if _, ok := ch.placeholders.Load(testLocalKey); !ok {
		t.Fatal("placeholder ts was not remembered")
	}
}

// always_bubbles forces the placeholder off: the answer is an edit of a message
// posted before the reasoning, so in a chronological Slack thread it would
// render above the bubbles it is supposed to conclude.
func TestAlwaysBubblesForcesPlaceholderOff(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()
	ch := newConfiguredTestChannel(t, rec, config.SlackConfig{
		ReasoningDelivery: channels.ReasoningDeliveryAlwaysBubbles,
		ShowPlaceholder:   boolPtr(true), // explicitly on — bubbles still win
	})

	if ch.shouldPostPlaceholder() {
		t.Fatal("shouldPostPlaceholder() = true under always_bubbles")
	}
	ch.postPlaceholder(context.Background(), "C1", testLocalKey, "")
	if rec.called("chat.postMessage") {
		t.Fatalf("placeholder posted under always_bubbles: %q", rec.postedTexts())
	}
}

func TestPlaceholderSkippedWhenDisabled(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()
	ch := newConfiguredTestChannel(t, rec, config.SlackConfig{ShowPlaceholder: boolPtr(false)})

	ch.postPlaceholder(context.Background(), "C1", testLocalKey, "")
	if rec.called("chat.postMessage") {
		t.Fatalf("placeholder posted with show_placeholder=false: %q", rec.postedTexts())
	}

	// The answer then arrives as its own message rather than an edit.
	if err := ch.Send(context.Background(), bus.OutboundMessage{
		Channel:  "slack",
		ChatID:   testLocalKey,
		Content:  "the answer",
		Metadata: map[string]string{"local_key": testLocalKey, "placeholder_key": testLocalKey},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rec.called("chat.update") {
		t.Fatal("answer edited a placeholder that was never posted")
	}
	if texts := rec.postedTexts(); len(texts) != 1 || texts[0] != "the answer" {
		t.Fatalf("posted %q, want the answer as a fresh message", texts)
	}
}

// Only one ts can be remembered per key, so a second post would orphan a
// "Thinking..." message in the thread forever. Rapid messages inside
// debounceDelay reach here with one already live.
func TestPlaceholderNotDuplicatedForSameKey(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()
	ch := newConfiguredTestChannel(t, rec, config.SlackConfig{})

	for range 4 {
		ch.postPlaceholder(context.Background(), "C1", testLocalKey, "")
	}
	if texts := rec.postedTexts(); len(texts) != 1 {
		t.Fatalf("posted %d placeholders for one key: %q", len(texts), texts)
	}
}

// The provider-retry notice is the one interim message that belongs in the
// placeholder. With no placeholder it used to return silently, leaving the user
// to wait out a multi-minute backoff with no signal.
func TestPlaceholderUpdatePostsFreshWhenNoPlaceholder(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()
	ch := newConfiguredTestChannel(t, rec, config.SlackConfig{ShowPlaceholder: boolPtr(false)})

	if err := ch.Send(context.Background(), bus.OutboundMessage{
		Channel:  "slack",
		ChatID:   testLocalKey,
		Content:  "Provider busy, retrying... (1/3)",
		Metadata: map[string]string{"placeholder_update": "true"},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	texts := rec.postedTexts()
	if len(texts) != 1 || !strings.Contains(texts[0], "retrying") {
		t.Fatalf("posted %q, want the retry notice as a fresh message", texts)
	}
}

// The default path must keep folding the retry notice into the placeholder
// rather than posting a second message — the fallback added for the
// placeholder-off case must not fire when there is a placeholder.
func TestPlaceholderUpdateEditsPlaceholderByDefault(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()
	ch := newConfiguredTestChannel(t, rec, config.SlackConfig{})
	ch.placeholders.Store(testLocalKey, "1700000000.0")

	if err := ch.Send(context.Background(), bus.OutboundMessage{
		Channel:  "slack",
		ChatID:   testLocalKey,
		Content:  "Provider busy, retrying... (1/3)",
		Metadata: map[string]string{"placeholder_update": "true", "placeholder_key": testLocalKey},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !rec.called("chat.update") {
		t.Fatal("retry notice did not edit the placeholder")
	}
	if rec.called("chat.postMessage") {
		t.Fatalf("retry notice also posted a fresh message: %q", rec.postedTexts())
	}
	if _, ok := ch.placeholders.Load(testLocalKey); !ok {
		t.Fatal("retry notice consumed the placeholder the answer still needs")
	}
}

// A failed run must not leave reasoning bubbles as the only artifact of the
// turn. The technical error is suppressed for external channels, so the channel
// posts a generic notice instead of nothing.
func TestFailedRunPostsVisibleOutcomeWhenPlaceholderOff(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()
	ch := newConfiguredTestChannel(t, rec, config.SlackConfig{
		ReasoningDelivery: channels.ReasoningDeliveryAlwaysBubbles,
	})

	if err := ch.Send(context.Background(), bus.OutboundMessage{
		Channel:  "slack",
		ChatID:   testLocalKey,
		Content:  "",
		Metadata: map[string]string{"local_key": testLocalKey, bus.MetaRunOutcome: bus.RunOutcomeFailed},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	texts := rec.postedTexts()
	if len(texts) != 1 || texts[0] != channels.GenericFailureNotice {
		t.Fatalf("posted %q, want the generic failure notice", texts)
	}
}

func TestFailedRunReplacesPlaceholderWithNotice(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()
	ch := newConfiguredTestChannel(t, rec, config.SlackConfig{})
	ch.placeholders.Store(testLocalKey, "1700000000.0")

	if err := ch.Send(context.Background(), bus.OutboundMessage{
		Channel:  "slack",
		ChatID:   testLocalKey,
		Content:  "",
		Metadata: map[string]string{"placeholder_key": testLocalKey, bus.MetaRunOutcome: bus.RunOutcomeFailed},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !rec.called("chat.update") {
		t.Fatal("placeholder was not turned into the failure notice")
	}
	if rec.called("chat.delete") {
		t.Fatal("placeholder deleted instead of carrying the failure notice")
	}
}

// NO_REPLY and cancellation keep today's behavior: the placeholder is removed
// and nothing is said.
func TestEmptyContentWithoutFailureStaysSilent(t *testing.T) {
	for _, meta := range []map[string]string{
		{"placeholder_key": testLocalKey},
		{"placeholder_key": testLocalKey, bus.MetaRunOutcome: bus.RunOutcomeCancelled},
	} {
		rec := newSlackAPIRecorder()
		ch := newConfiguredTestChannel(t, rec, config.SlackConfig{})
		ch.placeholders.Store(testLocalKey, "1700000000.0")

		if err := ch.Send(context.Background(), bus.OutboundMessage{
			Channel: "slack", ChatID: testLocalKey, Content: "", Metadata: meta,
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if rec.called("chat.postMessage") || rec.called("chat.update") {
			t.Fatalf("meta %v produced a message: %q", meta, rec.postedTexts())
		}
		if !rec.called("chat.delete") {
			t.Fatalf("meta %v did not clean up the placeholder", meta)
		}
		rec.server.Close()
	}
}

// A provider that emits no reasoning at all must still deliver the answer:
// always_bubbles turns the placeholder off, so with nothing to show the turn
// would otherwise be entirely silent.
func TestAnswerDeliveredWithoutPlaceholderOrReasoning(t *testing.T) {
	rec := newSlackAPIRecorder()
	defer rec.server.Close()
	ch := newConfiguredTestChannel(t, rec, config.SlackConfig{
		ReasoningDelivery: channels.ReasoningDeliveryAlwaysBubbles,
	})

	ch.postPlaceholder(context.Background(), "C1", testLocalKey, "")
	if err := ch.Send(context.Background(), bus.OutboundMessage{
		Channel:  "slack",
		ChatID:   testLocalKey,
		Content:  "42",
		Metadata: map[string]string{"local_key": testLocalKey, "placeholder_key": testLocalKey},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if texts := rec.postedTexts(); len(texts) != 1 || texts[0] != "42" {
		t.Fatalf("posted %q, want just the answer", texts)
	}
}
