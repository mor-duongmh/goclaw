package providers

import "testing"

// Without ThinkingCapable, ResolveReasoningDecision collapses every requested
// effort to "off" and --effort is never passed to the CLI, so the agent's
// Thinking Level setting silently does nothing. Capabilities() already
// advertises Thinking: true, so the two must agree.
func TestClaudeCLIProviderIsThinkingCapable(t *testing.T) {
	var p any = &ClaudeCLIProvider{}
	tc, ok := p.(ThinkingCapable)
	if !ok {
		t.Fatal("ClaudeCLIProvider does not implement ThinkingCapable")
	}
	if !tc.SupportsThinking() {
		t.Fatal("SupportsThinking() = false")
	}
	if caps := (&ClaudeCLIProvider{}).Capabilities(); !caps.Thinking {
		t.Fatal("Capabilities().Thinking = false but SupportsThinking() = true")
	}
}

// An explicit level reaches the CLI. "opus" is not in the reasoning capability
// registry, so the decision passes the requested effort straight through.
func TestClaudeCLIExplicitEffortReachesRequest(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high"} {
		d := ResolveReasoningDecision(&ClaudeCLIProvider{}, "opus", effort, ReasoningFallbackDowngrade, "agent")
		if got := d.RequestEffort(); got != effort {
			t.Fatalf("effort %q: RequestEffort() = %q, want %q (reason: %s)", effort, got, effort, d.Reason)
		}
	}
}

// Characterization, so the surprise is recorded rather than rediscovered:
// "auto" on a model with no registry entry leaves the provider default in
// place, which means no --effort flag. Operators have to pick an explicit
// level for a model GoClaw has no capability metadata for.
func TestClaudeCLIAutoEffortOnUnknownModelSendsNoFlag(t *testing.T) {
	d := ResolveReasoningDecision(&ClaudeCLIProvider{}, "opus", "auto", ReasoningFallbackDowngrade, "agent")
	if !d.UsedProviderDefault {
		t.Fatalf("expected auto on an unknown model to defer to the provider default, got %+v", d)
	}
	if got := d.RequestEffort(); got != "" {
		t.Fatalf("RequestEffort() = %q, want empty", got)
	}
}

func TestClaudeCLIOffStaysOff(t *testing.T) {
	d := ResolveReasoningDecision(&ClaudeCLIProvider{}, "opus", "off", ReasoningFallbackDowngrade, "agent")
	if d.EffectiveEffort != "off" || d.RequestEffort() != "" {
		t.Fatalf("off resolved to %+v", d)
	}
}
