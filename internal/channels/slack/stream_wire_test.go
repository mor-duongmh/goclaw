package slack

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
)

// Characterization fixtures for the streaming path (stream.go) plus a guard on
// reasoning delivery. Like send_wire_test.go these pin CURRENT behavior.

// newWireStream wires a slackStream to the harness channel.
func newWireStream(ch *Channel, channelID, msgTS string) *slackStream {
	return &slackStream{
		ch:        ch,
		channelID: channelID,
		msgTS:     msgTS,
	}
}

func TestSlackWireStreamUpdateUsesMrkdwn(t *testing.T) {
	ch, ws := newWireTestChannel(t, nil)
	s := newWireStream(ch, "C123", "1700.9")

	s.Update(context.Background(), "**đậm** và [link](https://example.com)")

	form := firstForm(t, ws, "chat.update", 0)
	text := form.Get("text")

	assertContains(t, text, "*đậm*", "**bold** → *bold*")
	assertContains(t, text, "<https://example.com|link>", "markdown link → mrkdwn link")
	assertNoField(t, form, "blocks")
	assertNoField(t, form, "markdown_text")

	if got := form.Get("ts"); got != "1700.9" {
		t.Errorf("ts = %q, want 1700.9", got)
	}
}

// TestSlackWireStreamUpdateThrottles pins the 1s throttle at stream.go:33-35.
//
// Ordering matters: lastUpdate must stay zero before the FIRST call or the
// first Update returns early and nothing reaches the wire at all — the test
// would then pass while asserting nothing.
func TestSlackWireStreamUpdateThrottles(t *testing.T) {
	ch, ws := newWireTestChannel(t, nil)
	s := newWireStream(ch, "C123", "1700.9")

	s.Update(context.Background(), "một")
	s.Update(context.Background(), "hai")

	requireCallCount(t, ws, "chat.update", 1)

	// Rewinding lastUpdate past the throttle window lets the next call through
	// without a real sleep.
	s.mu.Lock()
	s.lastUpdate = time.Now().Add(-2 * streamThrottleInterval)
	s.mu.Unlock()

	s.Update(context.Background(), "ba")
	requireCallCount(t, ws, "chat.update", 2)
}

// TestSlackWireStreamThrottleHoldsWhenUpdateFails pins that the 1s throttle
// counts every ATTEMPT, not every success.
//
// Before the fix, `s.lastUpdate = time.Now()` sat after the error branch's
// early return, so a persistently failing chat.update never advanced the clock
// and every stream tick fired another request. Harmless today because
// chat.update rarely fails on the mrkdwn path — but once markdown rendering
// makes format errors possible, one bad message turns into a request loop
// hammering Slack for the whole turn.
func TestSlackWireStreamThrottleHoldsWhenUpdateFails(t *testing.T) {
	ch, ws := newWireTestChannel(t, func(method string, _ int) string {
		if method == "chat.update" {
			return errResponse("invalid_blocks")
		}
		return ""
	})
	s := newWireStream(ch, "C123", "1700.9")

	// Three ticks inside one throttle window. Only the first may reach the wire.
	s.Update(context.Background(), "một")
	s.Update(context.Background(), "hai")
	s.Update(context.Background(), "ba")

	requireCallCount(t, ws, "chat.update", 1)
}

// TestSlackWireStreamTruncationIsRuneSafe pins the property that the previous
// byte slice at stream.go:38-39 violated: text put on the wire must always be
// valid UTF-8.
//
// The construction is deterministic, not a corpus tuned until it went red.
// "ế" is 3 bytes; ASCII padding keeps markdownToSlackMrkdwn length-neutral so
// the cut lands exactly where the arithmetic says, and sweeping the pad 0..2
// guarantees at least one case has the limit falling inside the rune.
//
// Measured before the fix: 2 of 3 offsets produced invalid UTF-8.
func TestSlackWireStreamTruncationIsRuneSafe(t *testing.T) {
	for pad := range 3 {
		ch, ws := newWireTestChannel(t, nil)
		s := newWireStream(ch, "C123", "1700.9")

		body := strings.Repeat("a", maxMessageLen-1-pad) + "ế" + strings.Repeat("b", 64)
		s.Update(context.Background(), body)

		calls := ws.callsTo("chat.update")
		// A throttled call captures nothing and would make this test green
		// while asserting nothing at all.
		if len(calls) == 0 {
			t.Fatalf("pad=%d: no chat.update captured — the throttle swallowed the call", pad)
		}

		text := calls[len(calls)-1].Form.Get("text")
		if !utf8.ValidString(text) {
			t.Errorf("pad=%d: wire text is not valid UTF-8 (cut landed mid-rune)", pad)
		}
		if !strings.HasSuffix(text, "...") {
			t.Errorf("pad=%d: truncated text lost the \"...\" marker (decision D10)", pad)
		}
	}
}

// TestSlackWireStreamShortTextIsNotMarkedTruncated guards the other half of
// decision D10: the "..." marker must appear only when content was actually
// cut, otherwise every short streaming update would look truncated.
func TestSlackWireStreamShortTextIsNotMarkedTruncated(t *testing.T) {
	ch, ws := newWireTestChannel(t, nil)
	s := newWireStream(ch, "C123", "1700.9")

	s.Update(context.Background(), "câu trả lời ngắn")

	text := firstForm(t, ws, "chat.update", 0).Get("text")
	if strings.HasSuffix(text, "...") {
		t.Errorf("short text was marked truncated: %q", text)
	}
	if text != "câu trả lời ngắn" {
		t.Errorf("text = %q, want the input unchanged", text)
	}
}

// TestSlackReasoningDeliveryResolvesOff guards the reasoning path at the level
// that actually routes, not at the constant.
//
// Asserting ReasoningStreamEnabled() == false alone would be a tautology — the
// method returns a literal false. What matters is that Slack is NOT a
// ReasoningDeliveryChannel, because that is the type assertion
// Manager.ResolveReasoningDelivery (runs.go:135-142) checks first. If someone
// implements that interface on Slack, raw italic-wrapped chain-of-thought
// starts flowing into Send() with no other signal.
func TestSlackReasoningDeliveryResolvesOff(t *testing.T) {
	ch, _ := newWireTestChannel(t, nil)

	if _, isReasoningCh := any(ch).(channels.ReasoningDeliveryChannel); isReasoningCh {
		t.Fatal("Slack now implements ReasoningDeliveryChannel — reasoning delivery routing changed; " +
			"re-check that raw reasoning is not being sent to Slack")
	}

	sc, ok := any(ch).(channels.StreamingChannel)
	if !ok {
		t.Fatal("Slack no longer implements StreamingChannel — CreateStream/FinalizeStream routing changed")
	}

	legacy := sc.ReasoningStreamEnabled()
	got := channels.ResolveReasoningDelivery("", &legacy)

	if got.Mode != channels.ReasoningDeliveryOff {
		t.Errorf("Mode = %q, want %q", got.Mode, channels.ReasoningDeliveryOff)
	}
	if got.ShowInChannel {
		t.Error("ShowInChannel = true, want false — reasoning must not reach Slack")
	}
	if got.BubbleDelivery {
		t.Error("BubbleDelivery = true, want false")
	}
}

// TestSplitAtLimitIsRuneSafe covers splitAtLimit (send.go:104-115) on the
// corpora most likely to break it. maxLen is swept from 4 upward: at maxLen 3
// ChunkMarkdown genuinely emits a partial rune for 4-byte runes (the
// `cutAt == 0` guard at chunking.go:48-49), which is a documented limitation
// well below any production limit.
func TestSplitAtLimitIsRuneSafe(t *testing.T) {
	corpora := map[string]string{
		"vietnamese": strings.Repeat("Cửa hàng đã nhận đủ đơn hàng ệ ỗ ự ỹ. ", 60),
		"cjk":        strings.Repeat("你好世界これはテストです ", 60),
		"emoji":      strings.Repeat("😀🎉🚀 ", 120),
		"mixed":      strings.Repeat("Xin chào 中文 😀 ", 80),
	}

	for name, corpus := range corpora {
		t.Run(name, func(t *testing.T) {
			for maxLen := 4; maxLen <= 300; maxLen++ {
				chunk, remaining := splitAtLimit(corpus, maxLen)
				if !utf8.ValidString(chunk) {
					t.Fatalf("maxLen=%d: first chunk is not valid UTF-8: %q", maxLen, chunk)
				}
				if !utf8.ValidString(remaining) {
					t.Fatalf("maxLen=%d: remainder is not valid UTF-8", maxLen)
				}
			}
			for _, maxLen := range []int{2000, 4000, 11000} {
				chunk, remaining := splitAtLimit(corpus, maxLen)
				if !utf8.ValidString(chunk) || !utf8.ValidString(remaining) {
					t.Fatalf("maxLen=%d: invalid UTF-8", maxLen)
				}
			}
		})
	}
}

// --- markdown_native ON branch -------------------------------------------

func TestSlackWireNativeStreamUpdateSendsMarkdownBlock(t *testing.T) {
	ch, ws := newWireTestChannel(t, nil)
	enableMarkdownNative(ch)
	s := newWireStream(ch, "C123", "1700.9")

	s.Update(context.Background(), "**đậm** và [link](https://example.com)")

	form := firstForm(t, ws, "chat.update", 0)
	blocks := blocksOf(t, form.Get("blocks"))
	if len(blocks) != 1 {
		t.Fatalf("blocks has %d elements, want 1", len(blocks))
	}

	blockText, _ := blocks[0]["text"].(string)
	if blockText != "**đậm** và [link](https://example.com)" {
		t.Errorf("block text = %q, want the raw markdown unchanged", blockText)
	}
	if form.Get("text") == "" {
		t.Error("no top-level text fallback on the streaming edit")
	}
	if got := form.Get("ts"); got != "1700.9" {
		t.Errorf("ts = %q, want 1700.9", got)
	}
}

// TestSlackWireNativeStreamDegrades covers the second chat.update path. The
// retry happens inside one Update() call, so the throttle is not involved.
func TestSlackWireNativeStreamDegrades(t *testing.T) {
	ch, ws := newWireTestChannel(t, func(method string, n int) string {
		if method == "chat.update" && n == 1 {
			return errResponse("block_mismatch")
		}
		return ""
	})
	enableMarkdownNative(ch)
	s := newWireStream(ch, "C123", "1700.9")

	s.Update(context.Background(), "**đậm**")

	updates := requireCallCount(t, ws, "chat.update", 2)
	retry := updates[1].Form
	if got := retry.Get("text"); got != "*đậm*" {
		t.Errorf("degraded text = %q, want %q", got, "*đậm*")
	}
	if got := retry.Get("blocks"); got != "[]" {
		t.Errorf("degraded update blocks = %q, want %q", got, "[]")
	}
}

// TestSlackWireNativeStreamTruncationIsRuneSafe repeats the Phase 3 property on
// the ON path, where the text put on the wire is raw markdown instead of mrkdwn.
func TestSlackWireNativeStreamTruncationIsRuneSafe(t *testing.T) {
	for pad := range 3 {
		ch, ws := newWireTestChannel(t, nil)
		enableMarkdownNative(ch)
		s := newWireStream(ch, "C123", "1700.9")

		body := strings.Repeat("a", maxMessageLen-1-pad) + "ế" + strings.Repeat("b", 64)
		s.Update(context.Background(), body)

		calls := ws.callsTo("chat.update")
		if len(calls) == 0 {
			t.Fatalf("pad=%d: no chat.update captured", pad)
		}

		blocks := blocksOf(t, calls[len(calls)-1].Form.Get("blocks"))
		blockText, _ := blocks[0]["text"].(string)
		if !utf8.ValidString(blockText) {
			t.Errorf("pad=%d: block text is not valid UTF-8", pad)
		}
		if !strings.HasSuffix(blockText, "...") {
			t.Errorf("pad=%d: truncated block text lost the \"...\" marker", pad)
		}
	}
}

// TestSlackWireNativeStreamOverBudgetSendsMrkdwn covers the one place the
// structure budget cannot split: a streaming edit is a single message. Left
// alone, an over-budget payload is rejected and degraded on EVERY tick — two
// chat.update calls per second plus a warning each, for the whole turn, which
// is exactly the traffic the 1s throttle exists to bound.
func TestSlackWireNativeStreamOverBudgetSendsMrkdwn(t *testing.T) {
	ch, ws := newWireTestChannel(t, nil)
	enableMarkdownNative(ch)
	s := newWireStream(ch, "C123", "1700.9")

	// Over budget in units, comfortably under the byte limit.
	body := strings.Repeat("---\nđoạn văn\n", slackStructureBudget+5)
	if structuralUnits(body) <= slackStructureBudget {
		t.Fatalf("fixture is not over budget: %d units", structuralUnits(body))
	}

	s.Update(context.Background(), body)

	// One call, not a rejection plus a degrade.
	form := firstForm(t, ws, "chat.update", 0)
	requireCallCount(t, ws, "chat.update", 1)

	if got := form.Get("blocks"); got != "[]" {
		t.Errorf("blocks = %q, want %q — an over-budget tick must clear any block from an earlier tick", got, "[]")
	}
	if form.Get("text") == "" {
		t.Error("over-budget tick sent no text")
	}
}
