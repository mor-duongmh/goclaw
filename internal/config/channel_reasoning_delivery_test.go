package config

import (
	"encoding/json"
	"testing"
)

func TestNormalizeChannelInstanceConfigRaw_NormalizesTelegramReasoningDelivery(t *testing.T) {
	raw := json.RawMessage(`{"reasoning_delivery":"always_bubbles","reasoning_stream":false,"dm_stream":false}`)

	got := NormalizeChannelInstanceConfigRaw("telegram", raw)

	var cfg map[string]any
	if err := json.Unmarshal(got, &cfg); err != nil {
		t.Fatalf("normalized config is invalid JSON: %v", err)
	}
	if cfg["reasoning_delivery"] != reasoningDeliveryAlwaysBubbles {
		t.Fatalf("reasoning_delivery = %v", cfg["reasoning_delivery"])
	}
	if _, ok := cfg["reasoning_stream"]; ok {
		t.Fatalf("legacy reasoning_stream survived normalization: %s", got)
	}
}

func TestNormalizeChannelInstanceConfigValue_LegacyFalseMapsToOff(t *testing.T) {
	got := NormalizeChannelInstanceConfigValue("telegram", map[string]any{
		"reasoning_stream": false,
	})

	cfg := got.(map[string]any)
	if cfg["reasoning_delivery"] != reasoningDeliveryOff {
		t.Fatalf("reasoning_delivery = %v, want off", cfg["reasoning_delivery"])
	}
	if _, ok := cfg["reasoning_stream"]; ok {
		t.Fatal("legacy reasoning_stream survived normalization")
	}
}

// Slack joined telegram in the normalization set; every other channel type is
// still passed through untouched.
func TestNormalizeChannelInstanceConfigValue_IgnoresUnsupportedChannels(t *testing.T) {
	raw := json.RawMessage(`{"reasoning_stream":false}`)
	got := NormalizeChannelInstanceConfigRaw("discord", raw)
	if string(got) != string(raw) {
		t.Fatalf("discord config changed: %s", got)
	}
}

// A Slack payload with no reasoning keys must survive byte-identical, so
// widening the gate cannot perturb existing instances. Keys are deliberately
// out of alphabetical order and the number is a float: an unmarshal/marshal
// round trip would reorder the keys and rewrite 1.5e3, so this fails if the
// pass-through is dropped.
func TestNormalizeChannelInstanceConfigRaw_SlackWithoutReasoningKeysUnchanged(t *testing.T) {
	raw := json.RawMessage(`{"history_limit":50,"dm_policy":"pairing","media_max_bytes":1.5e3}`)
	if got := NormalizeChannelInstanceConfigRaw("slack", raw); string(got) != string(raw) {
		t.Fatalf("slack config without reasoning keys changed:\n got %s\nwant %s", got, raw)
	}
}

func TestNormalizeChannelInstanceConfigRaw_SlackCanonicalModesPreserved(t *testing.T) {
	for _, mode := range []string{reasoningDeliveryOff, reasoningDeliveryStreamingOnly, reasoningDeliveryAlwaysBubbles} {
		raw := json.RawMessage(`{"reasoning_delivery":"` + mode + `"}`)
		var cfg map[string]any
		if err := json.Unmarshal(NormalizeChannelInstanceConfigRaw("slack", raw), &cfg); err != nil {
			t.Fatalf("mode %q: invalid JSON: %v", mode, err)
		}
		if cfg["reasoning_delivery"] != mode {
			t.Fatalf("mode %q rewritten to %v", mode, cfg["reasoning_delivery"])
		}
	}
}

// Case and surrounding whitespace are operator typos, not a different intent:
// canonicalize instead of discarding the choice. The read path
// (channels.NormalizeReasoningDeliveryMode) already trims and lowercases, so
// normalization has to agree with it or stored config and runtime disagree.
func TestNormalizeChannelInstanceConfigRaw_SlackCanonicalizesCaseAndWhitespace(t *testing.T) {
	cases := map[string]string{
		`"Always_Bubbles"`:   reasoningDeliveryAlwaysBubbles,
		`" always_bubbles "`: reasoningDeliveryAlwaysBubbles,
		`"OFF"`:              reasoningDeliveryOff,
		`"Streaming_Only"`:   reasoningDeliveryStreamingOnly,
	}
	for in, want := range cases {
		raw := json.RawMessage(`{"reasoning_delivery":` + in + `}`)
		var cfg map[string]any
		if err := json.Unmarshal(NormalizeChannelInstanceConfigRaw("slack", raw), &cfg); err != nil {
			t.Fatalf("input %s: invalid JSON: %v", in, err)
		}
		if cfg["reasoning_delivery"] != want {
			t.Fatalf("input %s → %v, want %s", in, cfg["reasoning_delivery"], want)
		}
	}
}

// Slack reasoning is opt-in, so an unrecognizable value must land on off.
// Falling back to streaming_only would turn a typo into an enabled feature.
func TestNormalizeChannelInstanceConfigRaw_SlackUnknownModeResolvesOff(t *testing.T) {
	for _, in := range []string{`"bogus"`, `""`, `123`, `null`} {
		raw := json.RawMessage(`{"reasoning_delivery":` + in + `}`)
		var cfg map[string]any
		if err := json.Unmarshal(NormalizeChannelInstanceConfigRaw("slack", raw), &cfg); err != nil {
			t.Fatalf("input %s: invalid JSON: %v", in, err)
		}
		if cfg["reasoning_delivery"] != reasoningDeliveryOff {
			t.Fatalf("input %s → %v, want off", in, cfg["reasoning_delivery"])
		}
	}
}

// Telegram shipped reasoning streaming before the three-mode config existed,
// so its unrecognized-value fallback stays streaming_only. Characterized here
// so widening the gate cannot silently change it.
func TestNormalizeChannelInstanceConfigRaw_TelegramUnknownModeStaysStreamingOnly(t *testing.T) {
	raw := json.RawMessage(`{"reasoning_delivery":"bogus"}`)
	var cfg map[string]any
	if err := json.Unmarshal(NormalizeChannelInstanceConfigRaw("telegram", raw), &cfg); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if cfg["reasoning_delivery"] != reasoningDeliveryStreamingOnly {
		t.Fatalf("telegram fallback = %v, want streaming_only", cfg["reasoning_delivery"])
	}
}

func TestNormalizeChannelInstanceConfigValue_SlackLegacyStreamMapsToOff(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		got := NormalizeChannelInstanceConfigValue("slack", map[string]any{"reasoning_stream": legacy})
		cfg := got.(map[string]any)
		if cfg["reasoning_delivery"] != reasoningDeliveryOff {
			t.Fatalf("legacy reasoning_stream=%v → %v, want off", legacy, cfg["reasoning_delivery"])
		}
		if _, ok := cfg["reasoning_stream"]; ok {
			t.Fatalf("legacy reasoning_stream survived normalization for slack")
		}
	}
}
