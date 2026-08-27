//go:build slackprobe

// Probe case tables. See probe_live_test.go for the harness and run command.
//
// Each test maps to a numbered step in
// plans/260827-1436-slack-markdown-block-migration/phase-01-live-slack-probe.md
package slack

import (
	"fmt"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
)

// syntaxCase is one markdown construct probed across mechanisms.
type syntaxCase struct {
	label string
	body  string
}

// baseSyntaxCases covers phase-01 step 2. Each body is deliberately minimal so
// a wrong render is unambiguous when read by eye.
func baseSyntaxCases(userRef, channelRef string) []syntaxCase {
	cases := []syntaxCase{
		{"đậm", "**đậm**"},
		{"nghiêng sao", "*nghiêng*"},
		{"nghiêng gạch dưới", "_nghiêng_"},
		{"gạch ngang", "~~gạch~~"},
		{"heading H1..H6", "# H1\n## H2\n### H3\n#### H4\n##### H5\n###### H6"},
		{"link", "[text](https://example.com)"},
		{"bare URL", "https://example.com"},
		{"list không thứ tự", "- một\n- hai"},
		{"list có thứ tự", "1. một\n2. hai"},
		{"list lồng 2 cấp", "- cha\n  - con\n    - cháu"},
		{"task list", "- [ ] chưa xong\n- [x] xong rồi"},
		{"blockquote", "> câu trích"},
		{"inline code", "dùng `fmt.Println` nhé"},
		{"fenced code có <>", "```go\nif a < b && c > d {\n\tfmt.Println(\"x\")\n}\n```"},
		{"bảng", "| Cột A | Cột B |\n|---|---|\n| 1 | 2 |\n| 3 | 4 |"},
		{"divider", "trên\n\n---\n\ndưới"},
		{"ảnh", "![alt](https://example.com/a.png)"},
		{"ký tự thô < > &", "a < b && c > d"},
		{"entity viết sẵn", "&lt; &gt; &amp;"},
		{"emoji shortcode", "xin chào :smile:"},
		{"raw HTML", "<b>bold</b> và <i>italic</i>"},
		{"tiếng Việt đủ dấu", "Cửa hàng đã nhận đủ đơn hàng — xin cảm ơn quý khách. Ăn, ắt, ằng, ệ, ỗ, ự, ỹ."},
	}

	// Mention cases need real IDs; skip rather than post a broken reference that
	// would make the results row meaningless.
	if userRef != "" {
		cases = append(cases, syntaxCase{"mention user", "chào <@" + userRef + ">"})
	}
	if channelRef != "" {
		cases = append(cases, syntaxCase{"mention channel", "xem <#" + channelRef + ">"})
	}
	cases = append(cases, syntaxCase{"special mention", "<!here> chú ý"})

	return cases
}

// whitespaceCases covers phase-01 step 2b — the highest-blast-radius group.
// If Slack treats a single newline as a CommonMark soft break it renders as a
// space, collapsing every multi-line answer into one block.
func whitespaceCases() []syntaxCase {
	return []syntaxCase{
		{"xuống dòng đơn", "dòng một\ndòng hai\ndòng ba"},
		{"dòng trống liên tiếp", "trên\n\n\n\ndưới"},
		{"thụt 2 khoảng trắng", "bình thường\n  thụt hai"},
		{"thụt 4 khoảng trắng", "bình thường\n    thụt bốn"},
		{"thụt 8 khoảng trắng", "bình thường\n        thụt tám"},
		{"tab đầu dòng", "bình thường\n\tthụt tab"},
		{"cột căn bằng tab", "tên\tsố\nalpha\t1\nbeta\t22"},
		{"stdout thô kiểu df -h", "Filesystem      Size  Used Avail Use% Mounted on\n/dev/disk1s1   466Gi  120Gi 340Gi  27% /\n/dev/disk1s4   466Gi  2.0Gi 340Gi   1% /System"},
		{"không markdown gì", "Đây là một đoạn văn thuần không có ký tự đặc biệt nào cả."},
	}
}

// TestLiveProbeSyntax — phase-01 step 2.
func TestLiveProbeSyntax(t *testing.T) {
	e := mustEnv(t)
	const section = "Ma trận cú pháp (step 2)"

	for _, m := range []mechanism{mechMrkdwn, mechBlock, mechMDText} {
		for _, c := range baseSyntaxCases(e.userRef, e.channelRef) {
			e.post(t, section, c.label, m, c.body)
		}
	}
}

// TestLiveProbeWhitespace — phase-01 step 2b. Hard gate for Phase 3.
func TestLiveProbeWhitespace(t *testing.T) {
	e := mustEnv(t)
	const section = "Xuống dòng và khoảng trắng (step 2b) — GATE cho Phase 3"

	for _, m := range []mechanism{mechMrkdwn, mechBlock, mechMDText} {
		for _, c := range whitespaceCases() {
			e.post(t, section, c.label, m, c.body)
		}
	}
}

// TestLiveProbeChatUpdate — phase-01 step 3. The riskiest unknown: no source
// confirms the markdown block works with chat.update at all.
func TestLiveProbeChatUpdate(t *testing.T) {
	e := mustEnv(t)
	const section = "chat.update (step 3)"

	const bodyA = "**bản đầu** với [link](https://example.com)"
	const bodyB = "**bản sửa** có bảng\n\n| A | B |\n|---|---|\n| 1 | 2 |"

	// (1) block -> block
	if ts, err := e.post(t, section, "block: post ban đầu", mechBlock, bodyA); err == "" {
		e.update(t, section, "block -> block", ts,
			slackapi.MsgOptionBlocks(slackapi.NewMarkdownBlock("", bodyB)),
			slackapi.MsgOptionText(plainish(bodyB), false))
	}

	// (2) block -> text only. Docs say blocks get REMOVED and text renders.
	// block_mismatch is the risk, since markdown blocks become rich_text.
	if ts, err := e.post(t, section, "block: post cho update text-only", mechBlock, bodyA); err == "" {
		e.update(t, section, "block -> chỉ text", ts,
			slackapi.MsgOptionText("Provider busy, retrying... (1/3)", false))
	}

	// (3) block -> text + explicit empty blocks. MsgOptionBlocks() with no args
	// is a NO-OP (nil-guard at slack-go chat.go:689) — a non-nil empty slice is
	// the only form that actually sets blocks:[].
	if ts, err := e.post(t, section, "block: post cho update blocks:[]", mechBlock, bodyA); err == "" {
		empty := []slackapi.Block{}
		e.update(t, section, "block -> text + blocks:[] tường minh", ts,
			slackapi.MsgOptionText("Provider busy, retrying... (1/3)", false),
			slackapi.MsgOptionBlocks(empty...))
	}

	// (4) plain text -> block. THIS IS GOCLAW'S ACTUAL PLACEHOLDER FLOW:
	// handlers.go posts "Thinking..." as plain text, then send.go edits it.
	if ts, err := e.post(t, section, "plain: post \"Thinking...\"", mechMrkdwn, "Thinking..."); err == "" {
		e.update(t, section, "plain -> block (LUỒNG THẬT của GoClaw)", ts,
			slackapi.MsgOptionBlocks(slackapi.NewMarkdownBlock("", bodyB)),
			slackapi.MsgOptionText(plainish(bodyB), false))
	}

	// (5) markdown_text -> markdown_text
	if ts, err := e.post(t, section, "markdown_text: post ban đầu", mechMDText, bodyA); err == "" {
		e.update(t, section, "markdown_text -> markdown_text", ts,
			slackapi.MsgOptionMarkdownText(bodyB))
	}

	// (6) plain text -> markdown_text (the mixed-result path for GoClaw)
	if ts, err := e.post(t, section, "plain: post cho update markdown_text", mechMrkdwn, "Thinking..."); err == "" {
		e.update(t, section, "plain -> markdown_text", ts,
			slackapi.MsgOptionMarkdownText(bodyB))
	}

	t.Log("XEM TAY: cờ \"(edited)\" có hiện ở case nào? Docs nói update qua blocks thì ẨN cờ, qua text thì HIỆN.")
}

// TestLiveProbeNotification — phase-01 step 4. MUST be inspected on a real
// phone: Slack docs say mobile push uses message.text exclusively.
func TestLiveProbeNotification(t *testing.T) {
	e := mustEnv(t)
	const section = "Push notification (step 4) — XEM TRÊN ĐIỆN THOẠI THẬT"

	body := "**Kết quả kiểm tra** — đã tìm thấy 2 lỗi trong `send.go`"

	e.post(t, section, "block + text fallback", mechBlock, body)
	e.post(t, section, "block KHÔNG có text", mechBlockRaw, body)
	e.post(t, section, "markdown_text (không đặt text được)", mechMDText, body)
	e.post(t, section, "mrkdwn (baseline hiện tại)", mechMrkdwn, body)

	t.Log("XEM TAY: mở Slack trên điện thoại, so nội dung push của 4 message trên.")
}

// TestLiveProbeLimits — phase-01 step 5. Must produce NUMBERS, not pass/fail.
func TestLiveProbeLimits(t *testing.T) {
	e := mustEnv(t)
	const section = "Giới hạn (step 5)"

	// ASCII ladder.
	for _, n := range []int{4000, 8000, 11900, 13000} {
		body := strings.Repeat("a", n)
		e.post(t, section, fmt.Sprintf("block %d ký tự ASCII", n), mechBlock, body)
		e.post(t, section, fmt.Sprintf("markdown_text %d ký tự ASCII", n), mechMDText, body)
	}

	// Vietnamese ladder at the same CHARACTER counts. maxLen in GoClaw counts
	// BYTES while Slack counts characters, so these two ladders diverge and
	// Phase 4 needs both numbers.
	for _, n := range []int{4000, 8000, 11900} {
		body := strings.Repeat("ế", n)
		e.post(t, section, fmt.Sprintf("block %d ký tự tiếng Việt (%d byte)", n, len(body)), mechBlock, body)
		e.post(t, section, fmt.Sprintf("markdown_text %d ký tự tiếng Việt (%d byte)", n, len(body)), mechMDText, body)
	}

	// Cumulative cap across two markdown blocks in one payload.
	half := strings.Repeat("b", 6500)
	_, _, err := e.api.PostMessage(e.channel,
		slackapi.MsgOptionBlocks(
			slackapi.NewMarkdownBlock("b1", half),
			slackapi.NewMarkdownBlock("b2", half),
		),
		slackapi.MsgOptionText("hai block 6500 mỗi block", false))
	_ = err
	record(section, "| 2 markdown block × 6500 = 13000 cộng dồn | block | xem log | | | |")
	t.Logf("[cumulative] 2 blocks x 6500: err=%v", err)

	// Block-expansion ratio — the one Slack ceiling a byte budget cannot bound.
	expansion := []struct {
		label string
		body  string
	}{
		{"60 mục list", "- mục\n" + strings.Repeat("- mục\n", 59)},
		{"20 heading", strings.Repeat("## tiêu đề\n\n", 20)},
		{"bảng 30 hàng", "| A | B |\n|---|---|\n" + strings.Repeat("| 1 | 2 |\n", 30)},
		{"10 fence", strings.Repeat("```go\nx := 1\n```\n\n", 10)},
		{"20 divider", strings.Repeat("a\n\n---\n\n", 20)},
	}
	for _, c := range expansion {
		e.post(t, section, c.label+" ("+fmt.Sprint(len(c.body))+" byte)", mechBlock, c.body)
	}

	t.Log("GHI SỐ: với mỗi case nở block, đếm số block Slack render ra. Cần TỈ LỆ block/phần tử, không chỉ pass/fail.")
}

// TestLiveProbeConflict — phase-01 step 6. Confirms markdown_text cannot carry
// a notification fallback, which is what makes push mobile the deciding
// trade-off between the two mechanisms.
func TestLiveProbeConflict(t *testing.T) {
	e := mustEnv(t)
	const section = "Conflict (step 6)"

	body := "**thử conflict**"

	_, _, err := e.api.PostMessage(e.channel,
		slackapi.MsgOptionMarkdownText(body),
		slackapi.MsgOptionText("fallback", false))
	got := "OK (KHÔNG conflict — trái với docs)"
	if err != nil {
		got = "LỖI: `" + err.Error() + "`"
	}
	t.Logf("[conflict] markdown_text + text: %s", got)
	record(section, "| markdown_text + text | cả hai | "+got+" | | | |")

	_, _, err = e.api.PostMessage(e.channel,
		slackapi.MsgOptionMarkdownText(body),
		slackapi.MsgOptionBlocks(slackapi.NewMarkdownBlock("", body)))
	got = "OK (KHÔNG conflict — trái với docs)"
	if err != nil {
		got = "LỖI: `" + err.Error() + "`"
	}
	t.Logf("[conflict] markdown_text + blocks: %s", got)
	record(section, "| markdown_text + blocks | cả hai | "+got+" | | | |")
}
