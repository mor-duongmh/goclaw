package slack

import (
	"encoding/json"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/config"
)

func slackChannelWithReasoning(t *testing.T, mode string) *Channel {
	t.Helper()
	ch, err := New(config.SlackConfig{
		Enabled:           true,
		BotToken:          "xoxb-test",
		AppToken:          "xapp-test",
		ReasoningDelivery: mode,
	}, bus.New(), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ch
}

func resolvedDelivery(t *testing.T, ch *Channel) channels.ResolvedReasoningDelivery {
	t.Helper()
	rdc, ok := any(ch).(channels.ReasoningDeliveryChannel)
	if !ok {
		t.Fatal("slack channel does not implement channels.ReasoningDeliveryChannel")
	}
	mode, legacy := rdc.ReasoningDeliveryConfig()
	return channels.ResolveReasoningDelivery(mode, legacy)
}

// Unset config must resolve to off: existing Slack instances keep today's
// behavior and reasoning stays opt-in per instance.
func TestSlackReasoningDeliveryDefaultsOff(t *testing.T) {
	got := resolvedDelivery(t, slackChannelWithReasoning(t, ""))
	if got.Mode != channels.ReasoningDeliveryOff || got.ShowInChannel || got.ForceProviderStream || got.BubbleDelivery {
		t.Fatalf("unset reasoning_delivery resolved to %+v, want off with all flags false", got)
	}
}

func TestSlackReasoningDeliveryAlwaysBubbles(t *testing.T) {
	got := resolvedDelivery(t, slackChannelWithReasoning(t, channels.ReasoningDeliveryAlwaysBubbles))
	if !got.BubbleDelivery {
		t.Fatalf("always_bubbles resolved to %+v, want BubbleDelivery", got)
	}
}

func TestSlackReasoningDeliveryStreamingOnly(t *testing.T) {
	got := resolvedDelivery(t, slackChannelWithReasoning(t, channels.ReasoningDeliveryStreamingOnly))
	if got.Mode != channels.ReasoningDeliveryStreamingOnly || !got.ShowInChannel || got.BubbleDelivery {
		t.Fatalf("streaming_only resolved to %+v", got)
	}
}

// An unrecognizable value must not fall open to streaming_only.
func TestSlackReasoningDeliveryUnknownValueIsOff(t *testing.T) {
	for _, mode := range []string{"bogus", "  ", "bubbles"} {
		got := resolvedDelivery(t, slackChannelWithReasoning(t, mode))
		if got.Mode != channels.ReasoningDeliveryOff || got.ShowInChannel {
			t.Fatalf("mode %q resolved to %+v, want off", mode, got)
		}
	}
}

// The streaming reasoning lane stays shut for Slack: thread replies have
// different UX from Telegram's in-place edit. always_bubbles does not open it.
func TestSlackReasoningStreamEnabledStaysFalse(t *testing.T) {
	for _, mode := range []string{"", channels.ReasoningDeliveryStreamingOnly, channels.ReasoningDeliveryAlwaysBubbles} {
		if slackChannelWithReasoning(t, mode).ReasoningStreamEnabled() {
			t.Fatalf("ReasoningStreamEnabled() = true for mode %q", mode)
		}
	}
}

// The registered constructor path (cmd/gateway.go).
func TestSlackFactoryWithPendingStoreMapsReasoningDelivery(t *testing.T) {
	creds := json.RawMessage(`{"bot_token":"xoxb-test","app_token":"xapp-test"}`)
	cfg := json.RawMessage(`{"reasoning_delivery":"always_bubbles"}`)

	ch, err := FactoryWithPendingStore(nil)("slack-1", creds, cfg, bus.New(), nil)
	if err != nil {
		t.Fatalf("FactoryWithPendingStore: %v", err)
	}
	rdc, ok := ch.(channels.ReasoningDeliveryChannel)
	if !ok {
		t.Fatal("factory-built channel does not implement ReasoningDeliveryChannel")
	}
	mode, _ := rdc.ReasoningDeliveryConfig()
	if mode != channels.ReasoningDeliveryAlwaysBubbles {
		t.Fatalf("mode = %q, want always_bubbles — instance config is not mapped", mode)
	}
}

// The static config.json path (cmd/gateway_channels_setup.go).
func TestSlackNewFromStaticConfigMapsReasoningDelivery(t *testing.T) {
	mode, _ := slackChannelWithReasoning(t, channels.ReasoningDeliveryAlwaysBubbles).ReasoningDeliveryConfig()
	if mode != channels.ReasoningDeliveryAlwaysBubbles {
		t.Fatalf("mode = %q, want always_bubbles", mode)
	}
}
