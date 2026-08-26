package config

import (
	"encoding/json"
	"strings"
)

const (
	channelTypeTelegram            = "telegram"
	channelTypeSlack               = "slack"
	reasoningDeliveryKey           = "reasoning_delivery"
	legacyReasoningStreamKey       = "reasoning_stream"
	reasoningDeliveryOff           = "off"
	reasoningDeliveryStreamingOnly = "streaming_only"
	reasoningDeliveryAlwaysBubbles = "always_bubbles"
)

// normalizesReasoningDelivery reports whether a channel type participates in
// reasoning-delivery normalization. Every other type is passed through
// untouched so its config keys keep whatever shape they were stored with.
func normalizesReasoningDelivery(channelType string) bool {
	switch channelType {
	case channelTypeTelegram, channelTypeSlack:
		return true
	default:
		return false
	}
}

// NormalizeChannelInstanceConfigValue applies compatibility rewrites for
// channel-instance config payloads before they are persisted.
func NormalizeChannelInstanceConfigValue(channelType string, value any) any {
	if !normalizesReasoningDelivery(channelType) || value == nil {
		return value
	}
	switch cfg := value.(type) {
	case json.RawMessage:
		return normalizeChannelConfigRaw(channelType, cfg)
	case []byte:
		return []byte(normalizeChannelConfigRaw(channelType, cfg))
	case map[string]any:
		return normalizeReasoningDeliveryMap(channelType, cfg)
	default:
		return value
	}
}

func NormalizeChannelInstanceConfigRaw(channelType string, raw json.RawMessage) json.RawMessage {
	if !normalizesReasoningDelivery(channelType) {
		return raw
	}
	return normalizeChannelConfigRaw(channelType, raw)
}

func normalizeChannelConfigRaw(channelType string, raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return raw
	}
	_, hasMode := cfg[reasoningDeliveryKey]
	_, hasLegacy := cfg[legacyReasoningStreamKey]
	if !hasMode && !hasLegacy {
		// No reasoning keys to rewrite: return the stored bytes untouched
		// instead of re-marshalling and reordering unrelated config.
		return raw
	}
	cfg = normalizeReasoningDeliveryMap(channelType, cfg)
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return raw
	}
	return encoded
}

func normalizeReasoningDeliveryMap(channelType string, cfg map[string]any) map[string]any {
	if cfg == nil {
		return nil
	}
	_, hasMode := cfg[reasoningDeliveryKey]
	_, hasLegacy := cfg[legacyReasoningStreamKey]
	if !hasMode && !hasLegacy {
		return cfg
	}
	cfg[reasoningDeliveryKey] = resolveReasoningDeliveryMode(channelType, cfg)
	delete(cfg, legacyReasoningStreamKey)
	return cfg
}

func resolveReasoningDeliveryMode(channelType string, cfg map[string]any) string {
	if mode, ok := cfg[reasoningDeliveryKey].(string); ok {
		if canonical := canonicalReasoningDeliveryMode(mode); canonical != "" {
			return canonical
		}
	}
	if legacy, ok := cfg[legacyReasoningStreamKey].(bool); ok && !legacy {
		return reasoningDeliveryOff
	}
	return unrecognizedReasoningDeliveryMode(channelType)
}

// canonicalReasoningDeliveryMode accepts the operator's value with stray case
// or whitespace and returns the canonical spelling, or "" when the value names
// no known mode. It must agree with channels.NormalizeReasoningDeliveryMode:
// if normalization discarded a value the read path would have accepted, stored
// config and runtime behavior would disagree.
func canonicalReasoningDeliveryMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case reasoningDeliveryOff:
		return reasoningDeliveryOff
	case reasoningDeliveryStreamingOnly:
		return reasoningDeliveryStreamingOnly
	case reasoningDeliveryAlwaysBubbles:
		return reasoningDeliveryAlwaysBubbles
	default:
		return ""
	}
}

// unrecognizedReasoningDeliveryMode is the fallback for a value that names no
// known mode. Telegram shipped reasoning streaming before the three-mode
// config existed, so it keeps its historical meaning. Slack reasoning is
// opt-in from the start: an unrecognizable value must stay silent rather than
// turn a typo into an enabled feature.
func unrecognizedReasoningDeliveryMode(channelType string) string {
	if channelType == channelTypeSlack {
		return reasoningDeliveryOff
	}
	return reasoningDeliveryStreamingOnly
}
