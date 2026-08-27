package slack

import (
	"errors"
	"log/slog"
	"regexp"
	"strings"

	slackapi "github.com/slack-go/slack"
)

// Markdown-native render path.
//
// Slack's chat.postMessage `text` field only understands legacy mrkdwn, which is
// why markdownToSlackMrkdwn exists. Block Kit's `markdown` block understands
// standard markdown instead, and the two dialects disagree on the most common
// characters (`*one star*` is bold in mrkdwn, italic in standard markdown), so no
// single string is correct for both. This file carries the standard-markdown
// path; format.go keeps the mrkdwn path, which stays the default.
//
// Everything here sits behind the markdown_native flag, default OFF. With the
// flag off the wire body is byte-identical to what shipped before.

const (
	// slackStructureBudget caps how many structural markers ride in one payload.
	//
	// Slack allows 50 blocks per message and a single markdown block "may result
	// in multiple blocks after translation". Dividers, tables, headings and code
	// fences are what actually become separate blocks — a long list does not, it
	// stays one rich_text_list element inside one block. The threshold sits well
	// under 50 because the expansion factor is not measurable from here: guessing
	// low only costs an extra message, while guessing high is caught by the
	// msg_blocks_too_long degrade.
	slackStructureBudget = 40

	// plainTextFallbackRunes is the target length of the notification text that
	// rides alongside a markdown block. Mobile push only reads message.text.
	plainTextFallbackRunes = 300
)

// sendMethod distinguishes the two Slack write methods, which differ in ways
// that matter here: chat.postMessage generates a push notification, chat.update
// never does, and only chat.update can leave a stale blocks array behind.
type sendMethod int

const (
	methodPost sendMethod = iota
	methodUpdate
)

func (m sendMethod) String() string {
	if m == methodUpdate {
		return "update"
	}
	return "post"
}

// renderTarget says where a rendered payload goes.
type renderTarget struct {
	method    sendMethod
	channelID string
	msgTS     string // methodUpdate: the message being edited
	threadTS  string // thread to stay in; "" for none
}

// markdownNativeEnabled reports whether this instance renders standard markdown.
// nil means off, so an instance that never heard of the flag keeps the mrkdwn
// behavior it shipped with.
func (c *Channel) markdownNativeEnabled() bool {
	return c.config.MarkdownNative != nil && *c.config.MarkdownNative
}

// renderOptions builds the MsgOptions for text that is ALREADY in the right
// dialect for the active flag state. It does not convert anything: the call site
// converts before chunking, because converting per-chunk would move the split
// boundaries and cut tables in half.
//
// On the ON path the markdown goes in a markdown block and a stripped copy goes
// in top-level `text`. Both are needed: Slack documents that mobile push
// notifications only read message.text, and screen readers read the top-level
// text rather than the blocks. markdown_text is deliberately unused — it
// conflicts with `text`, which would cost the push notification.
func (c *Channel) renderOptions(text string) []slackapi.MsgOption {
	if !c.markdownNativeEnabled() {
		return []slackapi.MsgOption{slackapi.MsgOptionText(text, false)}
	}

	// Exactly one markdown block per payload. Slack's 12,000-character limit is
	// cumulative across the whole payload, not per block, so every character
	// budget downstream assumes this.
	return []slackapi.MsgOption{
		slackapi.MsgOptionBlocks(slackapi.NewMarkdownBlock("", text)),
		slackapi.MsgOptionText(plainTextFallback(text), false),
	}
}

// degradedOptions rebuilds the payload as legacy mrkdwn after Slack rejected the
// markdown block.
//
// The degraded body goes through markdownToSlackMrkdwn rather than
// plainTextFallback: the converter is the only transform proven in production,
// while the plain-text stripper drops link URLs and table structure. A safety
// net that renders worse than the state it is protecting is not a safety net.
func degradedOptions(t renderTarget, text string) []slackapi.MsgOption {
	opts := []slackapi.MsgOption{slackapi.MsgOptionText(markdownToSlackMrkdwn(text), false)}

	if t.method == methodUpdate {
		// chat.update keeps the existing blocks unless the request carries a
		// blocks value, and MsgOptionBlocks() with no arguments is a no-op (nil
		// guard in slack-go). Passing a non-nil empty slice is the only way to
		// send blocks: [] and actually drop the rejected markdown block —
		// otherwise block_mismatch repeats on every edit, silently.
		empty := []slackapi.Block{}
		opts = append(opts, slackapi.MsgOptionBlocks(empty...))
	}

	return opts
}

// sendRendered delivers text to Slack and, if Slack rejects it for formatting
// reasons, resends it once as mrkdwn.
//
// The degrade is the load-bearing part of this migration, not the renderer. The
// send path reports errors to the user only when media or a forward origin is
// involved (dispatch.go), so a payload Slack refuses is an answer the user never
// sees. Four new format error codes become reachable the moment markdown blocks
// go on the wire.
func (c *Channel) sendRendered(t renderTarget, text string) error {
	err := c.dispatch(t, c.renderOptions(text))

	// Only the markdown-native path can produce a format rejection, and only it
	// has something to fall back to. With the flag off there are no blocks to
	// blame, so a retry would just double the traffic.
	if err == nil || !c.markdownNativeEnabled() || !isFormatError(err) {
		return err
	}

	slog.Warn("slack.render_downgrade",
		"channel_id", t.channelID,
		"method", t.method.String(),
		"error", err)

	// Exactly one retry: a second failure propagates rather than looping.
	return c.dispatch(t, degradedOptions(t, text))
}

// dispatch performs the actual API call for a target.
func (c *Channel) dispatch(t renderTarget, opts []slackapi.MsgOption) error {
	if t.threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(t.threadTS))
	}

	if t.method == methodUpdate {
		_, _, _, err := c.api.UpdateMessage(t.channelID, t.msgTS, opts...)
		return err
	}

	_, _, err := c.api.PostMessage(t.channelID, opts...)
	return err
}

// formatErrorCodes are the Slack error codes that mean "this payload's shape is
// wrong", i.e. the ones a mrkdwn retry can fix.
//
// HTTP 5xx is deliberately absent. The public report of markdown blocks
// returning 500 concerns response_url and incoming webhooks, neither of which
// this codebase uses (grep for response_url / hooks.slack.com finds nothing).
// Treating a transient Slack outage as a format problem would make one blip a
// permanent downgrade for that message.
var formatErrorCodes = map[string]bool{
	"invalid_blocks":         true,
	"msg_blocks_too_long":    true,
	"block_mismatch":         true,
	"markdown_text_conflict": true,
}

// isFormatError reports whether err is a Slack format rejection.
//
// Matching is on the typed SlackErrorResponse, not on the error string: a
// substring check would also fire on an unrelated error that happens to quote a
// code, and on user content echoed back in a message.
func isFormatError(err error) bool {
	var se slackapi.SlackErrorResponse
	if !errors.As(err, &se) {
		return false
	}
	return formatErrorCodes[se.Err]
}

var (
	reFenceLine  = regexp.MustCompile("^\\s*```")
	reImageLink  = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
	reInlineCode = regexp.MustCompile("`([^`\n]+)`")
	reItalicStar = regexp.MustCompile(`\*([^*\n]+)\*`)
	reItalicUnd  = regexp.MustCompile(`_([^_\n]+)_`)
	reListMarker = regexp.MustCompile(`(?m)^[ \t]*[-*+][ \t]+`)
	reQuoteMark  = regexp.MustCompile(`(?m)^[ \t]*>[ \t]?`)
	reBlankRun   = regexp.MustCompile(`\n{3,}`)

	// Structural lines are the ones Slack is likely to expand into their own
	// block: dividers, table rows, fences and headings.
	reStructuralLine = regexp.MustCompile("(?m)^(?:---|\\||```|#{1,6}\\s)")
)

// plainTextFallback strips markdown down to something readable in a push
// notification or by a screen reader.
//
// It is NOT the degrade payload — see degradedOptions for why. This output only
// ever lands in top-level `text` next to a markdown block, where the markdown
// block is what people actually read.
//
// Slack tokens (<@U123>, <#C456>) and bare angle brackets pass through
// untouched: nothing here escapes HTML entities, because the markdown block
// documents backslash escaping instead and an &lt; would show up literally.
func plainTextFallback(text string) string {
	if text == "" {
		return ""
	}

	// Line pass first: fence markers and table separator rows are whole lines
	// that should disappear rather than leave a blank behind.
	kept := make([]string, 0, strings.Count(text, "\n")+1)
	for line := range strings.SplitSeq(text, "\n") {
		if reFenceLine.MatchString(line) || reTableSep.MatchString(line) {
			continue
		}
		kept = append(kept, line)
	}
	out := strings.Join(kept, "\n")

	out = reImageLink.ReplaceAllString(out, "$1") // before links: ![alt](url)
	out = reLink.ReplaceAllString(out, "$1")      // [text](url) -> text
	out = reInlineCode.ReplaceAllString(out, "$1")
	out = reBoldDouble.ReplaceAllString(out, "$1")     // before single-star italic
	out = reBoldUnderscore.ReplaceAllString(out, "$1") // before single-underscore italic
	out = reStrike.ReplaceAllString(out, "$1")
	out = reItalicStar.ReplaceAllString(out, "$1")
	out = reItalicUnd.ReplaceAllString(out, "$1")
	out = reHeader.ReplaceAllString(out, "$1")
	out = reListMarker.ReplaceAllString(out, "")
	out = reQuoteMark.ReplaceAllString(out, "")

	out = reBlankRun.ReplaceAllString(out, "\n\n")
	out = strings.TrimSpace(out)

	return cutRunes(out, plainTextFallbackRunes)
}

// cutRunes trims to a rune count, marking the cut. Counting runes rather than
// bytes keeps the result valid UTF-8 — Vietnamese diacritics are 2-3 bytes, so a
// byte slice lands mid-character on ordinary content.
func cutRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}

// countStructuralLines counts the lines Slack is likely to turn into their own
// block.
func countStructuralLines(text string) int {
	return len(reStructuralLine.FindAllStringIndex(text, -1))
}

// splitByStructureBudget breaks text into payloads that each stay under the
// block budget.
//
// It splits, it never truncates: an answer with many tables must arrive in
// several messages rather than being cut off. Joining the results with "\n"
// reproduces the input exactly.
//
// Splits only land on line boundaries outside a fenced code block; cutting
// between ``` and its closing fence would leave both halves unbalanced.
func splitByStructureBudget(text string) []string {
	lines := strings.Split(text, "\n")

	var payloads []string
	var cur []string
	count := 0
	inFence := false

	for _, line := range lines {
		isFence := reFenceLine.MatchString(line)
		structural := isFence || reStructuralLine.MatchString(line)

		if structural && count >= slackStructureBudget && !inFence && len(cur) > 0 {
			payloads = append(payloads, strings.Join(cur, "\n"))
			cur = nil
			count = 0
		}

		if structural {
			count++
		}
		if isFence {
			inFence = !inFence
		}
		cur = append(cur, line)
	}

	if len(cur) > 0 {
		payloads = append(payloads, strings.Join(cur, "\n"))
	}
	if len(payloads) == 0 {
		return []string{text}
	}
	return payloads
}
