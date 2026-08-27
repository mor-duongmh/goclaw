package slack

import (
	"context"
	"fmt"
	"log/slog"
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

	channelID := msg.ChatID
	if channelID == "" {
		return fmt.Errorf("empty chat ID for slack send")
	}

	placeholderKey := channelID
	if pk := msg.Metadata["placeholder_key"]; pk != "" {
		placeholderKey = pk
	}
	threadTS := msg.Metadata["message_thread_id"]

	// Placeholder update (LLM retry notification).
	//
	// The retry notice is a fixed string with no markdown syntax, so it goes
	// straight out as text on both flag paths. chat.update with text and no
	// blocks clears any existing blocks and renders the text — which is exactly
	// what is wanted when overwriting a message that was rendered as markdown.
	// See docs.slack.dev/reference/methods/chat.update.
	if msg.Metadata["placeholder_update"] == "true" {
		if pTS, ok := c.placeholders.Load(placeholderKey); ok {
			ts := pTS.(string)
			_, _, _, _ = c.api.UpdateMessage(channelID, ts,
				slackapi.MsgOptionText(msg.Content, false))
		}
		return nil
	}

	content := msg.Content

	// NO_REPLY: delete placeholder, return
	if content == "" {
		if pTS, ok := c.placeholders.Load(placeholderKey); ok {
			c.placeholders.Delete(placeholderKey)
			ts := pTS.(string)
			_, _, _ = c.api.DeleteMessage(channelID, ts)
		}
		return nil
	}

	// Convert to legacy mrkdwn only when markdown-native rendering is off, and
	// convert HERE rather than inside renderOptions: sendChunked splits before
	// building options, so converting per-chunk would move the split boundaries
	// and run convertTablesToCodeBlocks on half a table. Leaving it ungated
	// would be worse still — **bold** becomes *bold*, which a markdown block
	// then renders as italic.
	if !c.markdownNativeEnabled() {
		content = markdownToSlackMrkdwn(content)
	}

	// Edit placeholder with first chunk, send rest as follow-ups
	if pTS, ok := c.placeholders.Load(placeholderKey); ok {
		c.placeholders.Delete(placeholderKey)
		ts := pTS.(string)

		editContent, remaining := c.splitFirstPayload(content)

		editErr := c.sendRendered(renderTarget{
			method:    methodUpdate,
			channelID: channelID,
			msgTS:     ts,
			threadTS:  threadTS,
		}, editContent)

		if editErr == nil {
			if remaining != "" {
				return c.sendChunked(channelID, remaining, threadTS)
			}
			return nil
		}
		slog.Warn("slack placeholder edit failed, sending new message",
			"channel_id", channelID, "error", editErr)
	}

	// Handle media attachments
	for _, media := range msg.Media {
		if err := c.uploadFile(channelID, threadTS, media); err != nil {
			slog.Warn("slack: file upload failed",
				"file", media.URL, "error", err)
			c.sendChunked(channelID, fmt.Sprintf("[File upload failed: %s]", media.URL), threadTS)
		}
	}

	return c.sendChunked(channelID, content, threadTS)
}

// sendChunked sends message chunks using markdown-aware splitting.
func (c *Channel) sendChunked(channelID, content, threadTS string) error {
	for _, chunk := range c.payloadsFor(content) {
		err := c.sendRendered(renderTarget{
			method:    methodPost,
			channelID: channelID,
			threadTS:  threadTS,
		}, chunk)
		if err != nil {
			return fmt.Errorf("send slack message: %w", err)
		}
	}
	return nil
}

// payloadsFor splits content into the payloads that go on the wire: the Block
// Kit structure budget first (markdown-native only), then markdown-aware length
// chunking. Both split, neither truncates.
func (c *Channel) payloadsFor(content string) []string {
	if !c.markdownNativeEnabled() {
		return channels.ChunkMarkdown(content, maxMessageLen)
	}

	var payloads []string
	for _, part := range splitByStructureBudget(content) {
		payloads = append(payloads, channels.ChunkMarkdown(part, maxMessageLen)...)
	}
	return payloads
}

// splitFirstPayload returns the first wire payload plus everything left over,
// which the caller hands to sendChunked. On the markdown-native path the
// structure budget applies to the placeholder edit too, otherwise a
// divider-heavy answer would put its whole block count into one edit.
func (c *Channel) splitFirstPayload(content string) (first, remaining string) {
	if !c.markdownNativeEnabled() {
		return splitAtLimit(content, maxMessageLen)
	}

	parts := splitByStructureBudget(content)
	first, rest := splitAtLimit(parts[0], maxMessageLen)

	tail := parts[1:]
	if rest != "" {
		tail = append([]string{rest}, tail...)
	}
	return first, strings.Join(tail, "\n")
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
