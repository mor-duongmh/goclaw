package slack

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestExtractChannelID(t *testing.T) {
	tests := []struct {
		name     string
		localKey string
		expected string
	}{
		{
			name:     "plain channel id",
			localKey: "C123456",
			expected: "C123456",
		},
		{
			name:     "threaded message",
			localKey: "C123456:thread:1234.5678",
			expected: "C123456",
		},
		{
			name:     "threaded with different ts format",
			localKey: "C999:thread:999999.999999",
			expected: "C999",
		},
		{
			name:     "no thread marker",
			localKey: "C123456789",
			expected: "C123456789",
		},
		{
			name:     "empty string",
			localKey: "",
			expected: "",
		},
		{
			name:     "only thread marker",
			localKey: ":thread:1234.5678",
			expected: ":thread:1234.5678",
		},
		{
			name:     "colon but no thread text",
			localKey: "C123:thread:",
			expected: "C123",
		},
		{
			name:     "multiple colons",
			localKey: "C123:thread:1234:5678",
			expected: "C123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractChannelID(tt.localKey)
			if got != tt.expected {
				t.Errorf("extractChannelID(%q) = %q, want %q", tt.localKey, got, tt.expected)
			}
		})
	}
}

func TestExtractThreadTS(t *testing.T) {
	tests := []struct {
		name     string
		localKey string
		expected string
	}{
		{
			name:     "threaded message",
			localKey: "C123456:thread:1234.5678",
			expected: "1234.5678",
		},
		{
			name:     "plain channel id",
			localKey: "C123456",
			expected: "",
		},
		{
			name:     "empty string",
			localKey: "",
			expected: "",
		},
		{
			name:     "only thread marker (no channel id)",
			localKey: ":thread:1234.5678",
			expected: "", // idx must be > 0, so this returns ""
		},
		{
			name:     "thread marker but empty ts",
			localKey: "C123:thread:",
			expected: "",
		},
		{
			name:     "multiple colons after thread",
			localKey: "C123:thread:1234:5678:extra",
			expected: "1234:5678:extra",
		},
		{
			name:     "thread marker not found",
			localKey: "C123:other:1234.5678",
			expected: "",
		},
		{
			name:     "thread marker case sensitive",
			localKey: "C123:THREAD:1234.5678",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractThreadTS(tt.localKey)
			if got != tt.expected {
				t.Errorf("extractThreadTS(%q) = %q, want %q", tt.localKey, got, tt.expected)
			}
		})
	}
}

// --- streaming edit bounds ---

// newStreamForTest builds the stream through CreateStream so the production
// construction path is what the assertions run against.
func newStreamForTest(t *testing.T, ch *Channel) *slackStream {
	t.Helper()
	ch.placeholders.Store("C1", "1700000000.000100")
	st, err := ch.CreateStream(context.Background(), "C1", false)
	if err != nil {
		t.Fatalf("CreateStream: %v", err)
	}
	ss, ok := st.(*slackStream)
	if !ok {
		t.Fatalf("CreateStream returned %T, want *slackStream", st)
	}
	return ss
}

// authFailure is a 200-with-ok:false reply: Slack's own shape for a permanent
// error, so no retry handler matches and the attempt returns immediately.
func authFailure() scriptedResponse {
	return scriptedResponse{
		status: http.StatusOK,
		body:   map[string]any{"ok": false, "error": "invalid_auth"},
	}
}

// TestSlackStreamUpdateArmsBoundedDeadline pins that a chunk edit carries a
// deadline at all, and that it stays small. Update is invoked inline from the bus
// broadcast loop with context.Background(), so without a bound here a hostile
// Retry-After would hold that loop for as long as Slack asks.
func TestSlackStreamUpdateArmsBoundedDeadline(t *testing.T) {
	s := newScriptedSlack(nil)
	defer s.server.Close()

	ch, probe := newProbedRetryTestChannel(t, s)
	newStreamForTest(t, ch).Update(context.Background(), "partial answer")

	got := probe.firstDeadline(t)
	if got < 0 {
		t.Fatal("chat.update ran with no deadline: an uncapped Retry-After would stall the bus broadcast loop")
	}
	if got < time.Second || got > 30*time.Second {
		t.Fatalf("stream update deadline = %v, want a bound in [1s, 30s]", got)
	}
}

// TestSlackStreamFailedUpdateArmsThrottle: a failed edit must still consume a
// throttle window. It did not before, so a broken placeholder cost one API round
// trip per chunk rather than one per second.
func TestSlackStreamFailedUpdateArmsThrottle(t *testing.T) {
	s := newScriptedSlack(map[string][]scriptedResponse{
		"/chat.update": {authFailure()},
	})
	defer s.server.Close()

	ss := newStreamForTest(t, newRetryTestChannel(t, s))
	ss.Update(context.Background(), "chunk one")
	ss.Update(context.Background(), "chunk one and two")

	if got := s.attemptsFor("/chat.update"); got != 1 {
		t.Fatalf("attempts = %d, want 1 — the failed edit must arm the throttle for the next chunk", got)
	}
}

// TestSlackStreamStopsAfterRepeatedFailures: throttling alone still lets every
// remaining chunk pay for a doomed edit. After streamMaxFailures the stream stops
// calling; Send() still delivers the final answer into the same placeholder.
func TestSlackStreamStopsAfterRepeatedFailures(t *testing.T) {
	s := newScriptedSlack(map[string][]scriptedResponse{
		"/chat.update": {authFailure()},
	})
	defer s.server.Close()

	ss := newStreamForTest(t, newRetryTestChannel(t, s))
	for i := 0; i < 10; i++ {
		ss.lastUpdate = time.Time{} // stand in for a chunk arriving a second later
		ss.Update(context.Background(), "chunk")
	}

	if got := s.attemptsFor("/chat.update"); got != streamMaxFailures {
		t.Fatalf("attempts = %d over 10 chunks, want %d — editing must stop after repeated failures", got, streamMaxFailures)
	}
}

// TestSlackStreamFailureCounterResetsOnSuccess: the trip counts *consecutive*
// failures. One transient error mid-answer must not silence the rest of the run.
func TestSlackStreamFailureCounterResetsOnSuccess(t *testing.T) {
	s := newScriptedSlack(map[string][]scriptedResponse{
		"/chat.update": {authFailure(), {status: http.StatusOK}, authFailure()},
	})
	defer s.server.Close()

	ss := newStreamForTest(t, newRetryTestChannel(t, s))
	for i := 0; i < 10; i++ {
		ss.lastUpdate = time.Time{}
		ss.Update(context.Background(), "chunk")
	}

	// fail, ok (counter back to zero), then streamMaxFailures more failures.
	want := 2 + streamMaxFailures
	if got := s.attemptsFor("/chat.update"); got != want {
		t.Fatalf("attempts = %d, want %d — a success must clear the failure counter", got, want)
	}
}

