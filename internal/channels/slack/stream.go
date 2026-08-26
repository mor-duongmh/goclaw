package slack

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
)

const (
	streamThrottleInterval = 1000 * time.Millisecond

	// streamUpdateTimeout bounds one chat.update call group, retries included.
	// Update runs inline on the bus broadcast path (internal/bus/bus.go:120-135
	// invokes every subscriber synchronously while holding subMu.RLock), so an
	// uncapped Retry-After sleep here stalls the goroutine that emitted the chunk
	// plus every subscriber queued behind it. The caller hands us
	// context.Background() (internal/channels/events.go:36), so this deadline is
	// the only bound that exists.
	streamUpdateTimeout = 10 * time.Second

	// streamMaxFailures stops editing after this many consecutive failures. Each
	// failing edit can burn a whole streamUpdateTimeout, once per remaining
	// chunk, and the preview is cosmetic: Send() still writes the final answer
	// into the same placeholder.
	streamMaxFailures = 3
)

// slackStream implements channels.ChannelStream for Slack.
// It edits the placeholder "Thinking..." message as chunks arrive.
type slackStream struct {
	api        *slackapi.Client
	channelID  string
	threadTS   string
	msgTS      string    // placeholder message timestamp
	lastUpdate time.Time // end of the last chat.update attempt, success or not
	failures   int       // consecutive failed attempts; >= streamMaxFailures stops editing
	mu         sync.Mutex
}

// Update edits the placeholder with accumulated text, throttled to avoid rate limits.
func (s *slackStream) Update(ctx context.Context, fullText string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failures >= streamMaxFailures {
		return
	}
	if time.Since(s.lastUpdate) < streamThrottleInterval {
		return
	}

	formatted := markdownToSlackMrkdwn(fullText)
	if len(formatted) > maxMessageLen {
		formatted = formatted[:maxMessageLen] + "..."
	}

	opts := []slackapi.MsgOption{slackapi.MsgOptionText(formatted, false)}
	callCtx, cancel := context.WithTimeout(ctx, streamUpdateTimeout)
	_, _, _, err := s.api.UpdateMessageContext(callCtx, s.channelID, s.msgTS, opts...)
	cancel()

	// The throttle window reopens from when the attempt RETURNED, failures
	// included. Leaving lastUpdate stale on error let every later chunk issue its
	// own call, so one broken edit cost an API round trip per chunk instead of one
	// per second. Telegram arms its throttle the same way
	// (internal/channels/telegram/stream.go:236).
	s.lastUpdate = time.Now()
	if err != nil {
		s.failures++
		slog.Debug("slack stream chunk update failed", "error", err, "failures", s.failures)
		return
	}
	s.failures = 0
}

// Stop finalizes the stream. For Slack, Send() handles the final edit via the placeholder map,
// so Stop() is a no-op here — FinalizeStream stores the msgTS into c.placeholders.
func (s *slackStream) Stop(_ context.Context) error {
	return nil
}

// MessageID returns 0 — Slack uses string timestamps, not int message IDs.
// FinalizeStream handles the Slack-specific placeholder handoff via type assertion.
func (s *slackStream) MessageID() int {
	return 0
}

// MsgTS returns the Slack message timestamp (placeholder TS) for FinalizeStream handoff.
func (s *slackStream) MsgTS() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msgTS
}

// StreamEnabled reports whether streaming is active for DMs or groups.
func (c *Channel) StreamEnabled(isGroup bool) bool {
	if isGroup {
		return c.config.GroupStream != nil && *c.config.GroupStream
	}
	return c.config.DMStream != nil && *c.config.DMStream
}

// CreateStream creates a per-run streaming handle for the given chatID.
// Implements channels.StreamingChannel.
// The placeholder "Thinking..." was already sent in handleMessage.
func (c *Channel) CreateStream(_ context.Context, chatID string, _ bool) (channels.ChannelStream, error) {
	pTS, pOK := c.placeholders.Load(chatID)
	if !pOK {
		// No placeholder — stream will be a no-op
		return &slackStream{
			api:       c.api,
			channelID: extractChannelID(chatID),
			threadTS:  extractThreadTS(chatID),
		}, nil
	}

	return &slackStream{
		api:       c.api,
		channelID: extractChannelID(chatID),
		threadTS:  extractThreadTS(chatID),
		msgTS:     pTS.(string),
	}, nil
}

// FinalizeStream stores the stream's placeholder TS back into c.placeholders so that
// Send() can edit it with the properly formatted final response.
// Implements channels.StreamingChannel.
// ReasoningStreamEnabled returns false — Slack lane support is deferred to a separate PR.
// Slack streaming uses thread replies which have different UX from Telegram in-place edit.
func (c *Channel) ReasoningStreamEnabled() bool { return false }

// ReasoningDeliveryConfig implements channels.ReasoningDeliveryChannel.
// Unset or unrecognized config resolves to off so existing Slack instances
// keep their current behavior; reasoning delivery is opt-in per instance.
// The legacy reasoning_stream bool is never returned — Slack never shipped it.
func (c *Channel) ReasoningDeliveryConfig() (string, *bool) {
	switch mode := channels.NormalizeReasoningDeliveryMode(c.config.ReasoningDelivery); mode {
	case channels.ReasoningDeliveryStreamingOnly, channels.ReasoningDeliveryAlwaysBubbles:
		return mode, nil
	default:
		return channels.ReasoningDeliveryOff, nil
	}
}

func (c *Channel) FinalizeStream(_ context.Context, chatID string, stream channels.ChannelStream) {
	ss, ok := stream.(*slackStream)
	if !ok || ss.msgTS == "" {
		return
	}
	c.placeholders.Store(chatID, ss.msgTS)
}

// extractChannelID gets the channel ID from a local_key.
func extractChannelID(localKey string) string {
	if idx := strings.Index(localKey, ":thread:"); idx > 0 {
		return localKey[:idx]
	}
	return localKey
}

// extractThreadTS gets the thread_ts from a local_key, or "" if not threaded.
func extractThreadTS(localKey string) string {
	const prefix = ":thread:"
	if idx := strings.Index(localKey, prefix); idx > 0 {
		return localKey[idx+len(prefix):]
	}
	return ""
}
