package slack

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
	slackapi "github.com/slack-go/slack"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

// Send delivers an outbound message to Slack.
func (c *Channel) Send(_ context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return fmt.Errorf("slack bot not running")
	}

	if msg.ChatID == "" {
		return fmt.Errorf("empty chat ID for slack send")
	}
	// Intermediate sends (quick ack, progress bubbles, retry notices) are
	// published with the run's composite local key as ChatID, so the Slack
	// channel has to be recovered from it — "C123:thread:1700000000.000100"
	// is not a channel the API accepts.
	channelID := extractChannelID(msg.ChatID)

	// Only a reply that names the placeholder may edit or delete it. Progress
	// bubbles and quick acks deliberately travel without placeholder_key
	// (internal/channels/routing_metadata.go:28) so they arrive as their own
	// messages — and their ChatID is the run's local key, which is exactly the
	// key the placeholder is stored under. Falling back to it would let the
	// first progress message consume the "Thinking..." bubble that the final
	// answer is supposed to become.
	placeholderKey := msg.Metadata["placeholder_key"]
	threadTS := c.resolveThreadTS(msg, placeholderKey)

	// Placeholder update (LLM retry notification) is the one interim message
	// that belongs IN the placeholder, and it carries no placeholder_key — its
	// ChatID is the local key, so it addresses the placeholder directly.
	if msg.Metadata["placeholder_update"] == "true" {
		retryKey := placeholderKey
		if retryKey == "" {
			retryKey = msg.ChatID
		}
		if pTS, ok := c.placeholders.Load(retryKey); ok {
			ts := pTS.(string)
			_, _, _, _ = c.api.UpdateMessage(channelID, ts,
				slackapi.MsgOptionText(msg.Content, false))
		}
		return nil
	}

	content := msg.Content

	// NO_REPLY: delete placeholder, return.
	// Media-only replies (attachment without caption) must not take this path.
	if content == "" && len(msg.Media) == 0 {
		if pTS, ok := c.placeholders.LoadAndDelete(placeholderKey); ok {
			_, _, _ = c.api.DeleteMessage(channelID, pTS.(string))
		}
		return nil
	}

	if content != "" {
		content = markdownToSlackMrkdwn(content)
	}

	// Attachments are uploaded before the placeholder-edit branch below: that
	// branch returns as soon as the edit succeeds, so any media handled after it
	// would never be delivered. Uploads post their own messages, so the
	// placeholder is deleted rather than edited.
	if len(msg.Media) > 0 {
		slog.Info("slack.media_send",
			"chat_id", msg.ChatID,
			"channel_id", channelID,
			"thread_ts", threadTS,
			"meta_thread", msg.Metadata["message_thread_id"],
			"meta_local_key", msg.Metadata["local_key"],
			"meta_placeholder_key", msg.Metadata["placeholder_key"],
			"count", len(msg.Media))

		if pTS, ok := c.placeholders.LoadAndDelete(placeholderKey); ok {
			_, _, _ = c.api.DeleteMessage(channelID, pTS.(string))
		}

		for _, att := range msg.Media {
			if err := c.uploadFile(channelID, threadTS, att); err != nil {
				slog.Warn("slack: file upload failed",
					"file", att.URL, "error", err)
				_ = c.sendChunked(channelID,
					fmt.Sprintf("[File upload failed: %s]", filepath.Base(att.URL)), threadTS)
			}
		}

		if content == "" {
			return nil
		}
		return c.sendChunked(channelID, content, threadTS)
	}

	// Edit placeholder with first chunk, send rest as follow-ups
	if pTS, ok := c.placeholders.Load(placeholderKey); ok {
		c.placeholders.Delete(placeholderKey)
		ts := pTS.(string)

		editContent, remaining := splitAtLimit(content, maxMessageLen)

		opts := []slackapi.MsgOption{slackapi.MsgOptionText(editContent, false)}
		if threadTS != "" {
			opts = append(opts, slackapi.MsgOptionTS(threadTS))
		}

		if _, _, _, editErr := c.api.UpdateMessage(channelID, ts, opts...); editErr == nil {
			if remaining != "" {
				return c.sendChunked(channelID, remaining, threadTS)
			}
			return nil
		} else {
			slog.Warn("slack placeholder edit failed, sending new message",
				"channel_id", channelID, "error", editErr)
		}
	}

	return c.sendChunked(channelID, content, threadTS)
}

// resolveThreadTS picks the thread a reply belongs to. Explicit routing metadata
// wins; otherwise the thread is recovered from the composite local key
// ("C123:thread:1700000000.000100"), which is how tool-initiated sends
// (message/send_file) reach this channel — they carry the key but not the
// thread field. Mirrors the same fallback in the Telegram adapter.
func (c *Channel) resolveThreadTS(msg bus.OutboundMessage, placeholderKey string) string {
	if ts := msg.Metadata["message_thread_id"]; ts != "" {
		return ts
	}
	for _, candidate := range []string{msg.Metadata["local_key"], placeholderKey, msg.ChatID} {
		if ts := extractThreadTS(candidate); ts != "" {
			return ts
		}
	}
	return ""
}

// postPlaceholder posts the "Thinking..." message and remembers its timestamp
// so the final reply can edit it in place.
func (c *Channel) postPlaceholder(channelID, localKey, threadTS string) {
	opts := []slackapi.MsgOption{slackapi.MsgOptionText("Thinking...", false)}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}

	if _, ts, err := c.api.PostMessage(channelID, opts...); err == nil {
		c.placeholders.Store(localKey, ts)
	} else {
		slog.Debug("slack: placeholder post failed", "channel_id", channelID, "error", err)
	}
}

// sendChunked sends message chunks using markdown-aware splitting.
func (c *Channel) sendChunked(channelID, content, threadTS string) error {
	for _, chunk := range channels.ChunkMarkdown(content, maxMessageLen) {
		opts := []slackapi.MsgOption{slackapi.MsgOptionText(chunk, false)}
		if threadTS != "" {
			opts = append(opts, slackapi.MsgOptionTS(threadTS))
		}

		if _, _, err := c.api.PostMessage(channelID, opts...); err != nil {
			return fmt.Errorf("send slack message: %w", err)
		}
	}
	return nil
}

// splitAtLimit splits content into first chunk + remaining using markdown-aware chunking.
func splitAtLimit(content string, maxLen int) (chunk, remaining string) {
	chunks := channels.ChunkMarkdown(content, maxLen)
	if len(chunks) == 0 {
		return "", ""
	}
	if len(chunks) == 1 {
		return chunks[0], ""
	}
	return chunks[0], strings.Join(chunks[1:], "\n")
}
