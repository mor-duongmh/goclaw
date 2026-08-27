package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"
)

// Tests for the markdown-native render path: option building, the format-error
// degrade net, and the structural budget.
//
// Everything here goes through the wire harness rather than inspecting
// []MsgOption directly: slack-go's sendConfig is unexported, so the form body is
// the only place the payload is observable — and it is also the thing that
// matters.

// enableMarkdownNative flips the flag on a harness channel.
func enableMarkdownNative(ch *Channel) {
	on := true
	ch.config.MarkdownNative = &on
}

// blocksOf decodes the blocks form field, failing if it is absent or not JSON.
func blocksOf(t *testing.T, raw string) []map[string]any {
	t.Helper()
	if raw == "" {
		t.Fatal("form carries no blocks field")
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("blocks is not a JSON array: %v (raw: %q)", err, raw)
	}
	return out
}

// --- plainTextFallback ----------------------------------------------------

func TestPlainTextFallbackStripsMarkdown(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantNot []string
	}{
		{
			name: "heading and bold",
			in:   "## Kết quả\n**Đã xong** rồi",
			want: "Kết quả\nĐã xong rồi",
		},
		{
			name: "link keeps text drops url",
			in:   "xem [issue #1520](https://github.com/x/y/pull/1520) nhé",
			want: "xem issue #1520 nhé",
		},
		{
			name: "image degrades to alt text",
			in:   "![sơ đồ](https://x/y.png)",
			want: "sơ đồ",
		},
		{
			name: "fenced code keeps body drops fence",
			in:   "trước\n```go\na < b\n```\nsau",
			want: "trước\na < b\nsau",
		},
		{
			name: "inline code loses backticks",
			in:   "gọi `sendRendered()` đi",
			want: "gọi sendRendered() đi",
		},
		{
			name: "emphasis and strike markers",
			in:   "*nghiêng* _cũng nghiêng_ ~~bỏ~~ __đậm__",
			want: "nghiêng cũng nghiêng bỏ đậm",
		},
		{
			name: "list bullets and blockquote",
			in:   "- một\n* hai\n+ ba\n> trích",
			want: "một\nhai\nba\ntrích",
		},
		{
			name: "table separator row is dropped, data rows survive",
			in:   "| File | Dòng |\n|------|------|\n| send.go | 57 |",
			// Cell pipes stay: they read fine in a notification and stripping
			// them would merge columns into one ambiguous run of words.
			want: "| File | Dòng |\n| send.go | 57 |",
		},
		{
			// Emphasis needs flanking rules. Without them the underscore rule
			// eats everything between two identifiers, which for a dev-facing
			// agent is the common case, not an edge case.
			name: "snake_case identifiers survive",
			in:   "set user_id and tenant_id now",
			want: "set user_id and tenant_id now",
		},
		{
			name: "single snake_case token survives",
			in:   "file_name_here.go",
			want: "file_name_here.go",
		},
		{
			name: "literal asterisks survive",
			in:   "cost is 2 * 3 * 4 dollars",
			want: "cost is 2 * 3 * 4 dollars",
		},
		{
			name: "markdown inside a code span is left alone",
			in:   "`see [x](http://y)` end",
			want: "see [x](http://y) end",
		},
		{
			name: "emphasis around a code span still strips",
			in:   "**đậm** và `mã`",
			want: "đậm và mã",
		},
		{
			name:    "slack mention token is left alone",
			in:      "chào <@U0123>",
			want:    "chào <@U0123>",
			wantNot: []string{"&lt;"},
		},
		{
			name:    "angle brackets are not html-escaped",
			in:      "if a < b && c > d",
			want:    "if a < b && c > d",
			wantNot: []string{"&lt;", "&amp;", "&gt;"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := plainTextFallback(tc.in)
			if got != tc.want {
				t.Errorf("plainTextFallback(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
			for _, no := range tc.wantNot {
				if strings.Contains(got, no) {
					t.Errorf("output must not contain %q: %q", no, got)
				}
			}
		})
	}
}

func TestPlainTextFallbackCutIsRuneSafeAndMarked(t *testing.T) {
	// Vietnamese diacritics are 2-3 bytes, so a byte-based cut would land
	// mid-rune somewhere in this sweep.
	body := strings.Repeat("Cửa hàng đã nhận đủ đơn hàng ệ ỗ ự ỹ. ", 40)

	got := plainTextFallback(body)
	if !utf8.ValidString(got) {
		t.Fatalf("fallback is not valid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > plainTextFallbackRunes+3 {
		t.Errorf("fallback is %d runes, want <= %d (+3 for the marker)", n, plainTextFallbackRunes)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("cut fallback lost the truncation marker: %q", got)
	}

	short := plainTextFallback("câu ngắn")
	if short != "câu ngắn" {
		t.Errorf("short text = %q, want unchanged", short)
	}
	if strings.HasSuffix(short, "...") {
		t.Error("short text was marked truncated")
	}
}

// --- renderOptions -------------------------------------------------------

func TestSlackRenderFlagOffIsTextOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target renderTarget
		method string
	}{
		{"post", renderTarget{method: methodPost, channelID: "C123"}, "chat.postMessage"},
		{"update", renderTarget{method: methodUpdate, channelID: "C123", msgTS: "1700.9"}, "chat.update"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch, ws := newWireTestChannel(t, nil)

			if err := ch.sendRendered(tc.target, richBody); err != nil {
				t.Fatalf("sendRendered() error = %v", err)
			}

			form := firstForm(t, ws, tc.method, 0)
			// Flag off keeps the converted mrkdwn body of today's wire.
			assertContains(t, form.Get("text"), "*Đã tìm thấy 2 lỗi.*", "mrkdwn bold")
			assertNoField(t, form, "blocks")
			assertNoField(t, form, "markdown_text")
			if n := len(form["text"]); n != 1 {
				t.Errorf("text param appears %d times, want 1", n)
			}
		})
	}
}

func TestSlackRenderFlagOnSendsMarkdownBlockAndTextFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target renderTarget
		method string
	}{
		{"post", renderTarget{method: methodPost, channelID: "C123"}, "chat.postMessage"},
		{"update", renderTarget{method: methodUpdate, channelID: "C123", msgTS: "1700.9"}, "chat.update"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch, ws := newWireTestChannel(t, nil)
			enableMarkdownNative(ch)

			if err := ch.sendRendered(tc.target, richBody); err != nil {
				t.Fatalf("sendRendered() error = %v", err)
			}

			form := firstForm(t, ws, tc.method, 0)

			blocks := blocksOf(t, form.Get("blocks"))
			// One markdown block per payload is the invariant that makes any
			// character budget meaningful — Slack counts 12,000 across the
			// whole payload, not per block.
			if len(blocks) != 1 {
				t.Fatalf("blocks has %d elements, want exactly 1: %v", len(blocks), blocks)
			}
			if blocks[0]["type"] != "markdown" {
				t.Errorf("block type = %v, want markdown", blocks[0]["type"])
			}

			blockText, _ := blocks[0]["text"].(string)
			// Raw markdown must survive untouched: this is the whole point.
			assertContains(t, blockText, "**Đã tìm thấy 2 lỗi.**", "raw ** bold")
			assertContains(t, blockText, "## Kết quả kiểm tra", "raw heading")
			assertContains(t, blockText, "[issue #1520](https://github.com/x/y/pull/1520)", "raw link syntax")
			assertContains(t, blockText, "| File    | Dòng |", "raw table syntax")
			assertContains(t, blockText, "if a < b && c > d", "unescaped angle brackets")
			for _, leak := range []string{"&lt;", "&amp;", "&gt;"} {
				if strings.Contains(blockText, leak) {
					t.Errorf("block text carries HTML entity %q — escapeHTMLEntities ran on the ON path", leak)
				}
			}
			if strings.Contains(blockText, "```\n| File") {
				t.Error("table was converted to a code block — convertTablesToCodeBlocks ran on the ON path")
			}

			// Top-level text stays for mobile push and screen readers.
			if n := len(form["text"]); n != 1 {
				t.Fatalf("text param appears %d times, want 1", n)
			}
			fallback := form.Get("text")
			if fallback == "" {
				t.Error("no top-level text: mobile push would arrive empty")
			}
			if strings.Contains(fallback, "**") {
				t.Errorf("text fallback still carries markdown: %q", fallback)
			}
			assertNoField(t, form, "markdown_text")
		})
	}
}

// --- isFormatError -------------------------------------------------------

func TestIsFormatError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"invalid_blocks", slackapi.SlackErrorResponse{Err: "invalid_blocks"}, true},
		{"msg_blocks_too_long", slackapi.SlackErrorResponse{Err: "msg_blocks_too_long"}, true},
		{"block_mismatch", slackapi.SlackErrorResponse{Err: "block_mismatch"}, true},
		{"markdown_text_conflict", slackapi.SlackErrorResponse{Err: "markdown_text_conflict"}, true},
		{"wrapped format error", fmt.Errorf("send slack message: %w",
			slackapi.SlackErrorResponse{Err: "invalid_blocks"}), true},

		{"channel_not_found", slackapi.SlackErrorResponse{Err: "channel_not_found"}, false},
		{"rate_limited", slackapi.SlackErrorResponse{Err: "rate_limited"}, false},
		{"rate limit type", &slackapi.RateLimitedError{}, false},
		// A transient Slack outage must not be mistaken for bad formatting:
		// degrading on 5xx would make one blip permanent for that message.
		{"http 500", slackapi.StatusCodeError{Code: 500, Status: "500 Internal Server Error"}, false},
		{"network error", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
		// Substring matching would say true here; typed matching must not.
		{"plain string lookalike", errors.New("invalid_blocks"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFormatError(tc.err); got != tc.want {
				t.Errorf("isFormatError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// --- degrade -------------------------------------------------------------

// TestSlackRenderDegradesOnFormatError is the reason this whole phase exists:
// the migration adds four new format error codes to a path that has no user
// visible error reporting, so without a degrade a rejected payload is a
// silently lost answer.
func TestSlackRenderDegradesOnFormatError(t *testing.T) {
	for _, code := range []string{
		"invalid_blocks", "msg_blocks_too_long", "block_mismatch", "markdown_text_conflict",
	} {
		for _, tc := range []struct {
			name   string
			target renderTarget
			method string
		}{
			{"post", renderTarget{method: methodPost, channelID: "C123"}, "chat.postMessage"},
			{"update", renderTarget{method: methodUpdate, channelID: "C123", msgTS: "1700.9"}, "chat.update"},
		} {
			t.Run(code+"/"+tc.name, func(t *testing.T) {
				ch, ws := newWireTestChannel(t, func(method string, n int) string {
					if method == tc.method && n == 1 {
						return errResponse(code)
					}
					return ""
				})
				enableMarkdownNative(ch)

				if err := ch.sendRendered(tc.target, richBody); err != nil {
					t.Fatalf("sendRendered() error = %v, want the degrade to succeed", err)
				}

				calls := requireCallCount(t, ws, tc.method, 2)
				retry := calls[1].Form

				want := markdownToSlackMrkdwn(richBody)
				if got := retry.Get("text"); got != want {
					t.Errorf("degraded text is not markdownToSlackMrkdwn output\n got: %q\nwant: %q", got, want)
				}

				if tc.target.method == methodUpdate {
					// MsgOptionBlocks() with no args is a no-op (nil guard in
					// slack-go), so "blocks absent" would leave the rejected
					// markdown block in place and block_mismatch would repeat
					// silently. The retry must send blocks: [] explicitly.
					if got := retry.Get("blocks"); got != "[]" {
						t.Errorf("degraded update blocks = %q, want %q to clear the rejected block", got, "[]")
					}
				} else {
					assertNoField(t, retry, "blocks")
				}
			})
		}
	}
}

func TestSlackRenderDegradesOnlyOnce(t *testing.T) {
	ch, ws := newWireTestChannel(t, func(method string, _ int) string {
		if method == "chat.postMessage" {
			return errResponse("invalid_blocks")
		}
		return ""
	})
	enableMarkdownNative(ch)

	err := ch.sendRendered(renderTarget{method: methodPost, channelID: "C123"}, richBody)
	if err == nil {
		t.Fatal("sendRendered() error = nil, want the second failure to propagate")
	}
	assertContains(t, err.Error(), "invalid_blocks", "propagated error")

	requireCallCount(t, ws, "chat.postMessage", 2)
}

func TestSlackRenderDoesNotDegradeOnNonFormatError(t *testing.T) {
	for _, code := range []string{"channel_not_found", "ratelimited", "message_not_found"} {
		t.Run(code, func(t *testing.T) {
			ch, ws := newWireTestChannel(t, func(method string, _ int) string {
				if method == "chat.postMessage" {
					return errResponse(code)
				}
				return ""
			})
			enableMarkdownNative(ch)

			err := ch.sendRendered(renderTarget{method: methodPost, channelID: "C123"}, richBody)
			if err == nil {
				t.Fatalf("sendRendered() error = nil, want %s to propagate", code)
			}
			requireCallCount(t, ws, "chat.postMessage", 1)
		})
	}
}

// TestSlackRenderFlagOffNeverDegrades pins that the OFF path is exactly one
// request. There are no blocks to blame, so a retry would only double the
// traffic — and it would break the existing stream throttle fixture.
func TestSlackRenderFlagOffNeverDegrades(t *testing.T) {
	ch, ws := newWireTestChannel(t, func(method string, _ int) string {
		if method == "chat.update" {
			return errResponse("invalid_blocks")
		}
		return ""
	})

	err := ch.sendRendered(renderTarget{method: methodUpdate, channelID: "C123", msgTS: "1700.9"}, richBody)
	if err == nil {
		t.Fatal("sendRendered() error = nil, want the error to propagate")
	}
	requireCallCount(t, ws, "chat.update", 1)
}

func TestSlackRenderKeepsThreadTS(t *testing.T) {
	ch, ws := newWireTestChannel(t, nil)
	enableMarkdownNative(ch)

	target := renderTarget{method: methodPost, channelID: "C123", threadTS: "1700.1"}
	if err := ch.sendRendered(target, "xin chào"); err != nil {
		t.Fatalf("sendRendered() error = %v", err)
	}

	if got := firstForm(t, ws, "chat.postMessage", 0).Get("thread_ts"); got != "1700.1" {
		t.Errorf("thread_ts = %q, want 1700.1", got)
	}
}

// --- structural budget ---------------------------------------------------

func TestSplitByStructureBudgetKeepsContentIntact(t *testing.T) {
	// Counts are in structural UNITS: a whole table is one, a whole fenced
	// block is one. Each body below is over the budget in units, so each must
	// split.
	bodies := map[string]string{
		"dividers":    strings.Repeat("---\nđoạn văn\n", 60),
		"tables":      strings.Repeat("| a | b |\n|---|---|\n| x | y |\n\n", 60),
		"headings":    strings.Repeat("## tiêu đề\nnội dung\n", 60),
		"code fences": strings.Repeat("```go\nfmt.Println()\n```\nvăn bản\n", 60),
		"mixed":       strings.Repeat("## h\n---\n| a | b |\n\n", 30),
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			parts := splitByStructureBudget(body)
			if len(parts) < 2 {
				t.Fatalf("got %d payload(s), want a split — fixture is below the budget", len(parts))
			}
			// Nothing may be dropped: the budget splits, it never truncates.
			if got := strings.Join(parts, "\n"); got != body {
				t.Errorf("rejoined payloads differ from the input (len %d vs %d)", len(got), len(body))
			}
			for i, p := range parts {
				// Assert against the counter the splitter itself uses, so the
				// assertion cannot pass while the invariant is violated.
				if n := structuralUnits(p); n > slackStructureBudget {
					t.Errorf("payload %d has %d structural units, want <= %d", i, n, slackStructureBudget)
				}
			}
		})
	}
}

func TestSplitByStructureBudgetAtThreshold(t *testing.T) {
	for _, delta := range []int{-1, 0, 1} {
		n := slackStructureBudget + delta
		t.Run(fmt.Sprintf("budget%+d", delta), func(t *testing.T) {
			body := strings.TrimSuffix(strings.Repeat("---\nvăn bản\n", n), "\n")
			parts := splitByStructureBudget(body)

			wantSplit := delta > 0
			if gotSplit := len(parts) > 1; gotSplit != wantSplit {
				t.Errorf("%d dividers → %d payloads, want split=%v", n, len(parts), wantSplit)
			}
			if got := strings.Join(parts, "\n"); got != body {
				t.Error("rejoined payloads differ from the input")
			}
		})
	}
}

// TestSplitByStructureBudgetNeverCutsInsideAFence guards the failure mode that
// would be invisible in a content-preservation check: a split landing between
// ``` and its closing fence leaves both payloads with an unbalanced fence.
func TestSplitByStructureBudgetNeverCutsInsideAFence(t *testing.T) {
	// One long fenced block whose body is full of table-looking lines, so the
	// structural counter is well over budget while still inside the fence.
	body := "mở đầu\n```\n" + strings.Repeat("| x | y |\n", 120) + "```\nkết\n"

	for i, p := range splitByStructureBudget(body) {
		if n := strings.Count(p, "```"); n%2 != 0 {
			t.Errorf("payload %d has an odd number of fences (%d) — the split landed inside a code block:\n%s",
				i, n, p)
		}
	}
}

// TestSplitByStructureBudgetKeepsTablesWhole covers the failure a
// content-preservation check cannot see: a split between two table rows leaves
// a header with no delimiter row in one payload and a delimiter with no header
// in the next. Neither half is a table any more, so Slack renders both as
// literal pipes — losing exactly the rendering this migration exists to gain.
func TestSplitByStructureBudgetKeepsTablesWhole(t *testing.T) {
	t.Run("table after a full budget of dividers", func(t *testing.T) {
		body := strings.Repeat("---\nvăn bản\n", slackStructureBudget-1) +
			"| h1 | h2 |\n|----|----|\n| a | b |\n| c | d |"

		holders := 0
		for _, p := range splitByStructureBudget(body) {
			if !strings.Contains(p, "| h1 | h2 |") {
				continue
			}
			holders++
			for _, want := range []string{"|----|----|", "| a | b |", "| c | d |"} {
				if !strings.Contains(p, want) {
					t.Errorf("payload holding the header lost %q:\n%s", want, p)
				}
			}
		}
		if holders != 1 {
			t.Errorf("table header appears in %d payloads, want 1", holders)
		}
	})

	t.Run("long table is one unit", func(t *testing.T) {
		// A table translates to one table block, not one per row, so 60 rows
		// must not trip a 40-block budget.
		body := "| h | i |\n|---|---|\n" + strings.Repeat("| r | s |\n", 60)
		if parts := splitByStructureBudget(body); len(parts) != 1 {
			t.Errorf("60-row table split into %d payloads, want 1", len(parts))
		}
	})

	t.Run("fenced block is one unit", func(t *testing.T) {
		body := "```go\n" + strings.Repeat("fmt.Println()\n", 60) + "```"
		if parts := splitByStructureBudget(body); len(parts) != 1 {
			t.Errorf("one fenced block split into %d payloads, want 1", len(parts))
		}
	})
}

func TestSplitByStructureBudgetShortContentIsOnePayload(t *testing.T) {
	body := "## tiêu đề\nmột dòng\n\n| a | b |\n|---|---|\n| 1 | 2 |"
	parts := splitByStructureBudget(body)
	if len(parts) != 1 || parts[0] != body {
		t.Errorf("short content became %d payload(s), want 1 identical", len(parts))
	}
}
