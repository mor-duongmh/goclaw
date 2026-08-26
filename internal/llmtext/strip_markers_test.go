package llmtext

import (
	"strings"
	"testing"
)

func TestStripReasoningLeaksRemovesToolCallMarkers(t *testing.T) {
	in := "I should look this up.\n" +
		"[Tool Call: db_query]\n" +
		"Arguments: {\"sql\":\"select token from secrets\"}\n" +
		"[Tool Result db_query]\n" +
		"{\"rows\": 1}\n" +
		"So the answer is 42."

	got := StripReasoningLeaks(in)

	for _, leaked := range []string{"[Tool Call:", "[Tool Result", "select token from secrets", "\"rows\""} {
		if strings.Contains(got, leaked) {
			t.Fatalf("leaked %q in %q", leaked, got)
		}
	}
	if !strings.Contains(got, "So the answer is 42.") {
		t.Fatalf("dropped legitimate reasoning: %q", got)
	}
}

func TestStripReasoningLeaksRemovesHistoricalContextMarker(t *testing.T) {
	in := "[Historical context: earlier turns]\n\nNow I answer."

	got := StripReasoningLeaks(in)

	if strings.Contains(got, "[Historical context:") {
		t.Fatalf("historical context marker leaked: %q", got)
	}
	if !strings.Contains(got, "Now I answer.") {
		t.Fatalf("dropped legitimate reasoning: %q", got)
	}
}

// Characterization test, not an aspiration: the marker line and its JSON /
// Arguments continuation are removed, but an unindented prose-shaped line
// after a marker survives. Reasoning legitimately discusses tool output, so
// this stripper removes structural markers rather than everything the model
// quoted. Bubbles therefore get exactly the sanitization the final answer
// already gets — no stronger policy, and no weaker one.
func TestStripReasoningLeaksKeepsProseShapedLineAfterMarker(t *testing.T) {
	in := "[Tool Result db_query]\napi_key=sk-live-abcdef\ndone."

	got := StripReasoningLeaks(in)

	if strings.Contains(got, "[Tool Result") {
		t.Fatalf("marker survived: %q", got)
	}
	if !strings.Contains(got, "api_key=sk-live-abcdef") {
		t.Fatalf("behavior changed — prose-shaped line after a marker is now stripped: %q", got)
	}
}

func TestStripReasoningLeaksRemovesSystemMessageBlocks(t *testing.T) {
	in := "Let me check.\n[System Message] Stats: tokens=900\nReply in Vietnamese.\n\nThen I conclude."

	got := StripReasoningLeaks(in)

	if strings.Contains(got, "[System Message]") || strings.Contains(got, "Reply in Vietnamese.") {
		t.Fatalf("system message leaked: %q", got)
	}
	if !strings.Contains(got, "Then I conclude.") {
		t.Fatalf("dropped legitimate reasoning: %q", got)
	}
}

func TestStripReasoningLeaksRemovesMediaPaths(t *testing.T) {
	in := "The chart is ready.\nMEDIA:/Users/operator/private/chart.png\nI will describe it."

	got := StripReasoningLeaks(in)

	if strings.Contains(got, "MEDIA:") || strings.Contains(got, "/Users/operator/private") {
		t.Fatalf("media path leaked: %q", got)
	}
	if !strings.Contains(got, "I will describe it.") {
		t.Fatalf("dropped legitimate reasoning: %q", got)
	}
}

// Guards against over-stripping: ordinary reasoning prose must survive
// byte-identical, including think tags, which the assistant-response
// sanitizer removes but reasoning must keep.
func TestStripReasoningLeaksPreservesOrdinaryProse(t *testing.T) {
	cases := []string{
		"The user asks about pricing. Tiers are $10, $20, $50.\n\nI'll compare them.",
		"Consider `json.Marshal` — it returns ([]byte, error).",
		"<think>step one</think> nested tags stay",
		"Brackets like [note] and [1] are not markers.",
		"Code: if x { y() }",
		"  leading and trailing whitespace preserved  ",
		"",
	}
	for _, in := range cases {
		if got := StripReasoningLeaks(in); got != in {
			t.Fatalf("StripReasoningLeaks(%q) = %q, want unchanged", in, got)
		}
	}
}
