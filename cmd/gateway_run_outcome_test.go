package cmd

import (
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

// The run metadata map is shared by every outbound message of a turn, so
// tagging the terminal one must not write through to the others.
func TestWithRunOutcomeDoesNotMutateSharedMetadata(t *testing.T) {
	shared := map[string]string{"local_key": "C1:thread:1", "placeholder_key": "C1:thread:1"}

	tagged := withRunOutcome(shared, bus.RunOutcomeFailed)

	if _, leaked := shared[bus.MetaRunOutcome]; leaked {
		t.Fatal("withRunOutcome wrote the outcome into the shared run metadata")
	}
	if tagged[bus.MetaRunOutcome] != bus.RunOutcomeFailed {
		t.Fatalf("outcome = %q, want %q", tagged[bus.MetaRunOutcome], bus.RunOutcomeFailed)
	}
	for k, v := range shared {
		if tagged[k] != v {
			t.Fatalf("routing key %q lost: got %q, want %q — the failure notice needs placeholder_key", k, tagged[k], v)
		}
	}
}

func TestWithRunOutcomeHandlesNilMetadata(t *testing.T) {
	tagged := withRunOutcome(nil, bus.RunOutcomeCancelled)
	if tagged[bus.MetaRunOutcome] != bus.RunOutcomeCancelled {
		t.Fatalf("outcome = %q", tagged[bus.MetaRunOutcome])
	}
}
