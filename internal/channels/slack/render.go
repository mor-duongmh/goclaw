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
		// Slack removes the existing blocks when an update carries text and no
		// blocks, so the text alone would already drop the rejected block.
		// Send blocks: [] anyway — it states the intent at the call site and
		// keeps the retry correct if that behavior ever narrows.
		//
		// It has to be a non-nil empty slice: MsgOptionBlocks() with no
		// arguments hits slack-go's nil guard and silently does nothing.
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
		// Copy instead of appending in place: opts belongs to the caller, and a
		// slice with spare capacity would have its backing array written.
		opts = append(append(make([]slackapi.MsgOption, 0, len(opts)+1), opts...),
			slackapi.MsgOptionTS(t.threadTS))
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

// Line classifiers for the structural scan. Matched per line, so no (?m).
var (
	reFenceLine   = regexp.MustCompile("^[ \t]*```")
	reTableLine   = regexp.MustCompile(`^[ \t]*\|`)
	reDividerLine = regexp.MustCompile(`^[ \t]*-{3,}[ \t]*$`)
	reHeadingLine = regexp.MustCompile(`^[ \t]*#{1,6}[ \t]`)
)

// fenceMarker reports the length of a code-fence run at the start of line plus
// whatever info string follows it. Fewer than three backticks is not a fence.
//
// CommonMark closes a fence only with a run at least as long as the opener and
// carrying no info string, which is exactly what lets a markdown example quote
// ``` inside a ```` block.
func fenceMarker(line string) (backticks int, info string) {
	trimmed := strings.TrimLeft(line, " \t")

	n := 0
	for n < len(trimmed) && trimmed[n] == '`' {
		n++
	}
	if n < 3 {
		return 0, ""
	}
	return n, strings.TrimSpace(trimmed[n:])
}

// Inline and block markers stripped for the notification text.
var (
	reImageLink = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)

	// Emphasis needs flanking rules. Without them the underscore rule swallows
	// everything between two snake_case identifiers ("set user_id and
	// tenant_id" became "set userid and tenantid") and the star rule eats
	// literal asterisks in arithmetic ("2 * 3 * 4" became "2  3  4"). Both are
	// ordinary content for a developer-facing agent.
	//
	// The rules here: the opening marker must not sit inside a word, and the
	// emphasized text must not be space-flanked. RE2 has no lookaround, so the
	// preceding character is captured and written back.
	reItalicStar = regexp.MustCompile(`(^|[^\w])\*([^*\s\n](?:[^*\n]*[^*\s\n])?)\*`)
	reItalicUnd  = regexp.MustCompile(`(^|[^\w])_([^_\s\n](?:[^_\n]*[^_\s\n])?)_`)

	reListMarker = regexp.MustCompile(`(?m)^[ \t]*[-*+][ \t]+`)
	reQuoteMark  = regexp.MustCompile(`(?m)^[ \t]*>[ \t]?`)
	reBlankRun   = regexp.MustCompile(`\n{3,}`)
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

	// Inline markers are stripped only outside code spans: a link written
	// inside `backticks` is literal text, and rewriting it would report code
	// the sender never wrote.
	out = outsideCodeSpans(out, stripInlineMarkers)

	// Block markers are line-anchored, so they run on the whole text.
	out = reHeader.ReplaceAllString(out, "$1")
	out = reListMarker.ReplaceAllString(out, "")
	out = reQuoteMark.ReplaceAllString(out, "")

	out = reBlankRun.ReplaceAllString(out, "\n\n")
	out = strings.TrimSpace(out)

	return cutRunes(out, plainTextFallbackRunes)
}

// stripInlineMarkers removes inline markdown syntax from a run of prose.
func stripInlineMarkers(text string) string {
	text = reImageLink.ReplaceAllString(text, "$1")      // before links: ![alt](url)
	text = reLink.ReplaceAllString(text, "$1")           // [text](url) -> text
	text = reBoldDouble.ReplaceAllString(text, "$1")     // before single-star italic
	text = reBoldUnderscore.ReplaceAllString(text, "$1") // before single-underscore italic
	text = reStrike.ReplaceAllString(text, "$1")
	text = reItalicStar.ReplaceAllString(text, "${1}${2}")
	text = reItalicUnd.ReplaceAllString(text, "${1}${2}")
	return text
}

// outsideCodeSpans applies fn to the segments of text that sit outside a
// `code span`, dropping the backticks either way.
//
// Splitting on backticks instead of masking spans with a sentinel is
// deliberate: D13 kept placeholder tokens off this path, and a sentinel that
// survives into the payload is exactly the failure mode that ruled them out.
func outsideCodeSpans(text string, fn func(string) string) string {
	if !strings.Contains(text, "`") {
		return fn(text)
	}

	parts := strings.Split(text, "`")
	var sb strings.Builder
	for i, part := range parts {
		// Odd indexes sit between a pair of backticks. An unpaired trailing
		// backtick leaves its segment last, and that segment is prose again.
		if i%2 == 1 && i < len(parts)-1 {
			sb.WriteString(part)
			continue
		}
		sb.WriteString(fn(part))
	}
	return sb.String()
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

// structuralUnits counts the elements Slack is likely to translate into their
// own block.
//
// A unit is not a line. A whole table becomes one table block and a whole
// fenced block becomes one code block, so counting their rows would overstate
// a table by a factor of its length — and, worse, would invite a split between
// two rows. Dividers and headings are one unit each.
func structuralUnits(text string) int {
	n := 0
	sc := structureScanner{}
	for _, line := range strings.Split(text, "\n") {
		if sc.startsUnit(line) {
			n++
		}
	}
	return n
}

// structureScanner walks lines in order and reports where a new structural unit
// begins. The fence and table state it carries is what makes a split point safe.
type structureScanner struct {
	fence   int // backtick count of the open fence; 0 = not inside one
	inTable bool
}

func (s *structureScanner) startsUnit(line string) bool {
	if n, info := fenceMarker(line); n > 0 {
		switch {
		case s.fence == 0:
			// Opening fence. One code block, however long.
			s.fence = n
			s.inTable = false
			return true

		case n >= s.fence && info == "":
			// Closing fence.
			s.fence = 0
		}

		// Anything else is content. A ````block quoting an inner ``` fence is
		// one code block, and treating that inner line as a boundary inverts
		// the state for the rest of the message: the real code that follows
		// then looks like prose, and its comment lines get counted as headings
		// and split through.
		return false
	}

	if s.fence > 0 {
		// Body of a code block. Table-looking lines in here are just code.
		return false
	}

	switch {
	case reTableLine.MatchString(line):
		starts := !s.inTable
		s.inTable = true
		return starts

	default:
		s.inTable = false
		return reDividerLine.MatchString(line) || reHeadingLine.MatchString(line)
	}
}

// splitByStructureBudget breaks text into payloads that each stay under the
// block budget.
//
// It splits, it never truncates: an answer with many tables must arrive in
// several messages rather than being cut off. Joining the results with "\n"
// reproduces the input exactly.
//
// A split may only land where a new structural unit begins, which by
// construction is never inside a fenced block — including a ```` block that
// quotes shorter fences — and never partway through a table. Cutting a table between two rows leaves a header with no delimiter row
// in one payload and a delimiter with no header in the next: neither half is a
// table any more, so Slack renders both as literal pipes — losing exactly the
// rendering this path exists to gain.
func splitByStructureBudget(text string) []string {
	lines := strings.Split(text, "\n")

	var payloads []string
	var cur []string
	count := 0
	sc := structureScanner{}

	for _, line := range lines {
		startsUnit := sc.startsUnit(line)

		if startsUnit && count >= slackStructureBudget && len(cur) > 0 {
			payloads = append(payloads, strings.Join(cur, "\n"))
			cur = nil
			count = 0
		}
		if startsUnit {
			count++
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
