package slack

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
)

const streamThrottleInterval = 1000 * time.Millisecond

// slackStream implements channels.ChannelStream for Slack.
// It edits the placeholder "Thinking..." message as chunks arrive.
type slackStream struct {
	ch         *Channel // owner; supplies the API client and the render flag
	channelID  string
	threadTS   string
	msgTS      string    // placeholder message timestamp
	lastUpdate time.Time // last chat.update call
	mu         sync.Mutex
}

// Update edits the placeholder with accumulated text, throttled to avoid rate limits.
func (s *slackStream) Update(_ context.Context, fullText string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if time.Since(s.lastUpdate) < streamThrottleInterval {
		return
	}

	// Convert before truncating, and only on the mrkdwn path: with
	// markdown_native on, Slack renders the raw markdown itself.
	formatted := fullText
	if !s.ch.markdownNativeEnabled() {
		formatted = markdownToSlackMrkdwn(formatted)
	}
	formatted = truncateForStream(formatted, maxMessageLen)

	// Stamp the attempt BEFORE checking the result. The throttle limits how
	// often we call Slack, so it has to count every attempt — stamping only on
	// success meant a persistently failing update never advanced the clock and
	// every stream tick fired another request.
	s.lastUpdate = time.Now()

	target := renderTarget{
		method:    methodUpdate,
		channelID: s.channelID,
		msgTS:     s.msgTS,
	}

	// A streaming edit is one message, so the structure budget has no split to
	// fall back on here. Send mrkdwn for this tick instead of letting Slack
	// reject the payload: a rejection degrades, and since the content only
	// grows, every following tick would rejected-then-degrade too — two
	// chat.update calls per second for the rest of the turn, which is the
	// traffic the throttle exists to bound. Sending blocks: [] as part of the
	// mrkdwn payload also clears a block left by an earlier, smaller tick.
	if s.ch.markdownNativeEnabled() && structuralUnits(formatted) > slackStructureBudget {
		slog.Debug("slack stream payload over block budget, sending mrkdwn",
			"channel_id", s.channelID, "units", structuralUnits(formatted))

		if err := s.ch.dispatch(target, degradedOptions(target, formatted)); err != nil {
			slog.Debug("slack stream chunk update failed", "error", err)
		}
		return
	}

	// Through sendRendered so a format rejection on this chat.update degrades to
	// mrkdwn instead of leaving the stream frozen for the rest of the turn.
	if err := s.ch.sendRendered(target, formatted); err != nil {
		slog.Debug("slack stream chunk update failed", "error", err)
	}
}

// truncateForStream cuts text to the wire limit without splitting a UTF-8 rune,
// appending "..." only when content was actually dropped.
//
// The previous implementation sliced bytes directly (text[:maxLen]), which
// produced invalid UTF-8 whenever the limit fell inside a multi-byte character.
// Vietnamese diacritics are 2-3 bytes each, so this was reachable on ordinary
// content, not just exotic input.
//
// ChunkMarkdown already does the rune-boundary walk-back and is covered by its
// own tests, so reuse it instead of re-implementing utf8.RuneStart here. Its
// first chunk may exceed maxLen by a few bytes when it repairs a split code
// fence; that headroom is harmless against Slack's own 40,000-character limit.
func truncateForStream(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}

	chunks := channels.ChunkMarkdown(text, maxLen)
	switch len(chunks) {
	case 0:
		// Unreachable for non-empty input, but never return an empty message
		// just because chunking surprised us.
		return text
	case 1:
		return chunks[0]
	default:
		return chunks[0] + "..."
	}
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
			ch:        c,
			channelID: extractChannelID(chatID),
			threadTS:  extractThreadTS(chatID),
		}, nil
	}

	return &slackStream{
		ch:        c,
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
