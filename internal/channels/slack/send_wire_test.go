package slack

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
)

// Characterization fixtures for Send(). These pin CURRENT behavior — every
// assertion below describes what GoClaw ships today, mrkdwn and all. Nothing
// here asserts a desired future state.
//
// Branch map (internal/channels/slack/send.go):
//
//	:33-40  placeholder_update  → chat.update, content sent RAW (no converter)
//	:44-52  empty content       → chat.delete, placeholder key removed
//	:54     converter           → markdownToSlackMrkdwn on everything else
//	:57-76  placeholder present → chat.update, returns on success
//	:78-85  media               → uploadFile (only reachable with NO placeholder)
//	:88     fall-through        → sendChunked → chat.postMessage per chunk

// richBody exercises every construct the converter transforms, so a single
// fixture covers bold, links, tables and HTML-entity escaping at once.
const richBody = "## Kết quả kiểm tra\n" +
	"**Đã tìm thấy 2 lỗi.** Chi tiết ở [issue #1520](https://github.com/x/y/pull/1520).\n\n" +
	"| File    | Dòng |\n|---------|------|\n| send.go | 57   |\n\n" +
	"```go\nif a < b && c > d {\n```"

func TestSlackWirePlaceholderEditUsesMrkdwn(t *testing.T) {
	ch, ws := newWireTestChannel(t, nil)

	const localKey = "C123:thread:1700.1"
	ch.placeholders.Store(localKey, "1700.9")

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "C123",
		Content: richBody,
		Metadata: map[string]string{
			"placeholder_key":   localKey,
			"message_thread_id": "1700.1",
		},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	requireCallCount(t, ws, "chat.update", 1)
	requireCallCount(t, ws, "chat.postMessage", 0)

	form := firstForm(t, ws, "chat.update", 0)
	text := form.Get("text")

	// Converted mrkdwn, not the LLM's markdown.
	assertContains(t, text, "*Kết quả kiểm tra*", "heading → mrkdwn bold")
	assertContains(t, text, "*Đã tìm thấy 2 lỗi.*", "**bold** → *bold*")
	assertContains(t, text, "<https://github.com/x/y/pull/1520|issue #1520>", "markdown link → mrkdwn link")
	assertContains(t, text, "&lt;", "code angle brackets HTML-escaped")
	assertContains(t, text, "&amp;&amp;", "ampersands HTML-escaped")
	if strings.Contains(text, "**Đã tìm thấy") {
		t.Error("text still carries markdown ** — converter did not run")
	}

	// Today's payload is text-only. Both guards fail the moment a migration
	// starts emitting Block Kit, which is exactly the signal we want.
	assertNoField(t, form, "blocks")
	assertNoField(t, form, "markdown_text")

	if got := form.Get("ts"); got != "1700.9" {
		t.Errorf("ts = %q, want the stored placeholder ts", got)
	}

	// The placeholder is consumed by a successful edit.
	if _, still := ch.placeholders.Load(localKey); still {
		t.Error("placeholder key still present after successful edit")
	}
}

func TestSlackWireSendChunkedPostsPerChunkWithThreadTS(t *testing.T) {
	ch, ws := newWireTestChannel(t, nil)

	// No placeholder → falls through to sendChunked. Paragraph breaks give
	// ChunkMarkdown clean split points so the chunk count is deterministic.
	para := strings.Repeat("nội dung dài ", 200) // ~2.6 KB of UTF-8
	body := para + "\n\n" + para + "\n\n" + para

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "C123",
		Content: body,
		Metadata: map[string]string{
			"message_thread_id": "1700.1",
		},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	wantChunks := len(channels.ChunkMarkdown(markdownToSlackMrkdwn(body), maxMessageLen))
	if wantChunks < 2 {
		t.Fatalf("fixture body too short to chunk (got %d chunks) — raise the repeat count", wantChunks)
	}
	posts := requireCallCount(t, ws, "chat.postMessage", wantChunks)
	requireCallCount(t, ws, "chat.update", 0)

	for i, p := range posts {
		if got := p.Form.Get("thread_ts"); got != "1700.1" {
			t.Errorf("chunk %d thread_ts = %q, want 1700.1", i, got)
		}
		if p.Form.Get("text") == "" {
			t.Errorf("chunk %d has empty text", i)
		}
		assertNoField(t, p.Form, "blocks")
		assertNoField(t, p.Form, "markdown_text")
	}
}

// TestSlackWirePlaceholderUpdateSendsRawContent pins the deliberate asymmetry
// at send.go:33-40: the retry notice bypasses markdownToSlackMrkdwn entirely.
// A migration must decide explicitly whether to keep that; this fixture makes
// changing it impossible to do by accident.
func TestSlackWirePlaceholderUpdateSendsRawContent(t *testing.T) {
	ch, ws := newWireTestChannel(t, nil)

	const localKey = "C123"
	ch.placeholders.Store(localKey, "1700.9")

	// Deliberately markdown-looking so "was it converted?" is unambiguous.
	const raw = "Provider busy, retrying... (1/3) **not converted**"

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "C123",
		Content: raw,
		Metadata: map[string]string{
			"placeholder_key":    localKey,
			"placeholder_update": "true",
		},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	form := firstForm(t, ws, "chat.update", 0)
	if got := form.Get("text"); got != raw {
		t.Errorf("retry text = %q, want raw %q (converter must NOT run on this branch)", got, raw)
	}

	// This branch does not consume the placeholder — the real answer still
	// needs it.
	if _, still := ch.placeholders.Load(localKey); !still {
		t.Error("placeholder_update consumed the placeholder; the final answer would post as a new message")
	}
}

func TestSlackWireEmptyContentDeletesPlaceholder(t *testing.T) {
	ch, ws := newWireTestChannel(t, nil)

	const localKey = "C123:thread:1700.1"
	ch.placeholders.Store(localKey, "1700.9")

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "C123",
		Content: "",
		Metadata: map[string]string{
			"placeholder_key": localKey,
		},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	del := requireCallCount(t, ws, "chat.delete", 1)
	requireCallCount(t, ws, "chat.postMessage", 0)
	requireCallCount(t, ws, "chat.update", 0)

	if got := del[0].Form.Get("ts"); got != "1700.9" {
		t.Errorf("chat.delete ts = %q, want 1700.9", got)
	}
	if _, still := ch.placeholders.Load(localKey); still {
		t.Error("NO_REPLY left the placeholder key in the map")
	}
}

// TestSlackWireMediaBranchNeedsNoPlaceholder documents a trap: the media upload
// loop at send.go:78-85 sits AFTER the placeholder-edit branch, and that branch
// returns on success. On the normal flow (placeholder always present) the
// upload loop is unreachable. Any future assertion of the form "media still
// uploads" is vacuous unless the test omits the placeholder, as this one does.
func TestSlackWireMediaBranchNeedsNoPlaceholder(t *testing.T) {
	ch, ws := newWireTestChannel(t, nil)

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "C123",
		Content: "kèm tệp",
		Media:   []bus.MediaAttachment{{URL: "/nonexistent/path/report.pdf"}},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	// The upload fails (file does not exist) and the channel degrades to a
	// text notice, then still posts the content. Two posts prove the media
	// branch was entered rather than skipped.
	posts := ws.callsTo("chat.postMessage")
	if len(posts) != 2 {
		t.Fatalf("chat.postMessage calls = %d, want 2 (failure notice + content); flow: %v",
			len(posts), ws.methods())
	}
	assertContains(t, posts[0].Form.Get("text"), "File upload failed", "upload failure notice")
	assertContains(t, posts[1].Form.Get("text"), "kèm tệp", "message content")
}

// TestSlackWireNonFormatErrorReturnsWithoutRetry is the baseline that a later
// format-error degrade must be measured against: today a Slack rejection just
// propagates, with no second attempt.
func TestSlackWireNonFormatErrorReturnsWithoutRetry(t *testing.T) {
	ch, ws := newWireTestChannel(t, func(method string, _ int) string {
		if method == "chat.postMessage" {
			return errResponse("channel_not_found")
		}
		return ""
	})

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "C123",
		Content: "xin chào",
	})
	if err == nil {
		t.Fatal("Send() error = nil, want the Slack error to propagate")
	}
	assertContains(t, err.Error(), "channel_not_found", "propagated error")

	requireCallCount(t, ws, "chat.postMessage", 1)
}

// TestSlackWirePlaceholderEditFailureFallsBackToPost pins the recovery path at
// send.go:70-76 — a failed edit becomes a fresh post rather than a lost reply.
func TestSlackWirePlaceholderEditFailureFallsBackToPost(t *testing.T) {
	ch, ws := newWireTestChannel(t, func(method string, _ int) string {
		if method == "chat.update" {
			return errResponse("message_not_found")
		}
		return ""
	})

	const localKey = "C123"
	ch.placeholders.Store(localKey, "1700.9")

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "C123",
		Content: "xin chào",
		Metadata: map[string]string{
			"placeholder_key": localKey,
		},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	requireCallCount(t, ws, "chat.update", 1)
	requireCallCount(t, ws, "chat.postMessage", 1)
}

// --- markdown_native ON branch -------------------------------------------
//
// These fixtures run alongside the OFF-branch ones above, which stay the
// expectation for the shipped default. Nothing above was edited.

func TestSlackWireNativePlaceholderEditSendsMarkdownBlock(t *testing.T) {
	ch, ws := newWireTestChannel(t, nil)
	enableMarkdownNative(ch)

	const localKey = "C123:thread:1700.1"
	ch.placeholders.Store(localKey, "1700.9")

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "C123",
		Content: richBody,
		Metadata: map[string]string{
			"placeholder_key":   localKey,
			"message_thread_id": "1700.1",
		},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	requireCallCount(t, ws, "chat.update", 1)
	requireCallCount(t, ws, "chat.postMessage", 0)

	form := firstForm(t, ws, "chat.update", 0)

	blocks := blocksOf(t, form.Get("blocks"))
	if len(blocks) != 1 {
		t.Fatalf("blocks has %d elements, want exactly 1", len(blocks))
	}
	blockText, _ := blocks[0]["text"].(string)

	// The whole point of the migration: the LLM's markdown reaches Slack
	// unconverted, so Slack renders the table as a table and leaves the code
	// block's angle brackets alone.
	assertContains(t, blockText, "**Đã tìm thấy 2 lỗi.**", "raw ** bold")
	assertContains(t, blockText, "## Kết quả kiểm tra", "raw heading")
	assertContains(t, blockText, "[issue #1520](https://github.com/x/y/pull/1520)", "raw link")
	assertContains(t, blockText, "| File    | Dòng |", "raw table row")
	assertContains(t, blockText, "if a < b && c > d", "unescaped code")
	if strings.Contains(blockText, "&lt;") {
		t.Error("block text carries &lt; — the converter ran on the ON path")
	}

	if got := form.Get("ts"); got != "1700.9" {
		t.Errorf("ts = %q, want the stored placeholder ts", got)
	}
	if got := form.Get("thread_ts"); got != "1700.1" {
		t.Errorf("thread_ts = %q, want 1700.1", got)
	}
	if form.Get("text") == "" {
		t.Error("no top-level text: mobile push would arrive empty")
	}
	assertNoField(t, form, "markdown_text")
}

// TestSlackWireNativeSendChunkedChunksRawMarkdown pins that the ON path chunks
// the raw markdown, not the converted mrkdwn. The split boundaries move because
// of it, which is why the ON chunk count is recomputed from the raw body rather
// than reused from the OFF fixture.
func TestSlackWireNativeSendChunkedChunksRawMarkdown(t *testing.T) {
	ch, ws := newWireTestChannel(t, nil)
	enableMarkdownNative(ch)

	para := strings.Repeat("nội dung dài ", 200)
	body := para + "\n\n" + para + "\n\n" + para

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:   "C123",
		Content:  body,
		Metadata: map[string]string{"message_thread_id": "1700.1"},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	wantChunks := len(channels.ChunkMarkdown(body, maxMessageLen))
	if wantChunks < 2 {
		t.Fatalf("fixture body too short to chunk (got %d) — raise the repeat count", wantChunks)
	}
	posts := requireCallCount(t, ws, "chat.postMessage", wantChunks)

	for i, p := range posts {
		if got := p.Form.Get("thread_ts"); got != "1700.1" {
			t.Errorf("chunk %d thread_ts = %q, want 1700.1", i, got)
		}
		blocks := blocksOf(t, p.Form.Get("blocks"))
		if len(blocks) != 1 {
			t.Errorf("chunk %d has %d blocks, want 1", i, len(blocks))
		}
		if p.Form.Get("text") == "" {
			t.Errorf("chunk %d has no top-level text", i)
		}
	}
}

// TestSlackWireNativeStructuralBudgetSplitsPosts proves the block budget splits
// rather than truncates: a divider-heavy answer arrives as several messages with
// every line still present.
func TestSlackWireNativeStructuralBudgetSplitsPosts(t *testing.T) {
	ch, ws := newWireTestChannel(t, nil)
	enableMarkdownNative(ch)

	// 60 dividers, well over the budget, but only ~1 KB so length chunking
	// cannot be what splits it.
	body := strings.TrimSuffix(strings.Repeat("---\nđoạn văn\n", 60), "\n")

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "C123",
		Content: body,
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	posts := ws.callsTo("chat.postMessage")
	if len(posts) < 2 {
		t.Fatalf("got %d post(s), want the structural budget to split (flow: %v)",
			len(posts), ws.methods())
	}

	var rejoined []string
	for i, p := range posts {
		blocks := blocksOf(t, p.Form.Get("blocks"))
		if len(blocks) != 1 {
			t.Fatalf("post %d has %d blocks, want 1", i, len(blocks))
		}
		text, _ := blocks[0]["text"].(string)
		if n := structuralUnits(text); n > slackStructureBudget {
			t.Errorf("post %d carries %d structural units, want <= %d", i, n, slackStructureBudget)
		}
		rejoined = append(rejoined, text)
	}

	if got := strings.Join(rejoined, "\n"); got != body {
		t.Errorf("content was lost across the split (%d bytes on the wire vs %d in)",
			len(got), len(body))
	}
}

// TestSlackWireNativeDegradeOnUpdatePath is the fixture Phase 4 must not merge
// without: chat.update is the single biggest unknown of this migration, so the
// edit path has to be inside the degrade net, and the retry has to clear the
// rejected blocks explicitly.
func TestSlackWireNativeDegradeOnUpdatePath(t *testing.T) {
	ch, ws := newWireTestChannel(t, func(method string, n int) string {
		if method == "chat.update" && n == 1 {
			return errResponse("invalid_blocks")
		}
		return ""
	})
	enableMarkdownNative(ch)

	const localKey = "C123"
	ch.placeholders.Store(localKey, "1700.9")

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:   "C123",
		Content:  richBody,
		Metadata: map[string]string{"placeholder_key": localKey},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	updates := requireCallCount(t, ws, "chat.update", 2)
	// The degrade repaired the edit, so no fresh message was posted.
	requireCallCount(t, ws, "chat.postMessage", 0)

	retry := updates[1].Form
	assertContains(t, retry.Get("text"), "*Đã tìm thấy 2 lỗi.*", "degraded mrkdwn bold")
	if got := retry.Get("blocks"); got != "[]" {
		t.Errorf("degraded update blocks = %q, want %q — a no-op MsgOptionBlocks() would leave the rejected block in place", got, "[]")
	}
}

// TestSlackWireNativePlaceholderUpdateStaysPlain guards decision D3 under the
// flag: the retry notice is a fixed string with no markdown, so it keeps going
// out as plain text even when markdown rendering is on.
func TestSlackWireNativePlaceholderUpdateStaysPlain(t *testing.T) {
	ch, ws := newWireTestChannel(t, nil)
	enableMarkdownNative(ch)

	const localKey = "C123"
	ch.placeholders.Store(localKey, "1700.9")
	const raw = "Provider busy, retrying... (1/3) **not converted**"

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "C123",
		Content: raw,
		Metadata: map[string]string{
			"placeholder_key":    localKey,
			"placeholder_update": "true",
		},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	form := firstForm(t, ws, "chat.update", 0)
	if got := form.Get("text"); got != raw {
		t.Errorf("retry text = %q, want raw %q", got, raw)
	}
	assertNoField(t, form, "blocks")
}

// TestSlackWireNativeEmptyContentStillDeletesPlaceholder and the media check
// below confirm the branches the flag must not touch.
func TestSlackWireNativeEmptyContentStillDeletesPlaceholder(t *testing.T) {
	ch, ws := newWireTestChannel(t, nil)
	enableMarkdownNative(ch)

	const localKey = "C123:thread:1700.1"
	ch.placeholders.Store(localKey, "1700.9")

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:   "C123",
		Content:  "",
		Metadata: map[string]string{"placeholder_key": localKey},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	requireCallCount(t, ws, "chat.delete", 1)
	if _, still := ch.placeholders.Load(localKey); still {
		t.Error("NO_REPLY left the placeholder key in the map")
	}
}

func TestSlackWireNativeMediaStillUploads(t *testing.T) {
	// No placeholder seeded: the upload loop sits after the placeholder-edit
	// branch, which returns on success, so seeding one makes this vacuous.
	ch, ws := newWireTestChannel(t, nil)
	enableMarkdownNative(ch)

	err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "C123",
		Content: "kèm tệp",
		Media:   []bus.MediaAttachment{{URL: "/nonexistent/path/report.pdf"}},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	posts := ws.callsTo("chat.postMessage")
	if len(posts) != 2 {
		t.Fatalf("chat.postMessage calls = %d, want 2 (failure notice + content); flow: %v",
			len(posts), ws.methods())
	}
}

// TestSlackWirePlaceholderUpdateIdenticalUnderBothFlags makes the D3 guard
// falsifiable. Asserting "no blocks" on the ON path alone would also pass if
// enableMarkdownNative were a no-op; comparing the two payloads field by field
// cannot.
func TestSlackWirePlaceholderUpdateIdenticalUnderBothFlags(t *testing.T) {
	send := func(t *testing.T, native bool) url.Values {
		t.Helper()
		ch, ws := newWireTestChannel(t, nil)
		if native {
			enableMarkdownNative(ch)
		}
		ch.placeholders.Store("C123", "1700.9")

		err := ch.Send(context.Background(), bus.OutboundMessage{
			ChatID:  "C123",
			Content: "Provider busy, retrying... (1/3) **not converted**",
			Metadata: map[string]string{
				"placeholder_key":    "C123",
				"placeholder_update": "true",
			},
		})
		if err != nil {
			t.Fatalf("Send() error = %v", err)
		}
		return firstForm(t, ws, "chat.update", 0)
	}

	off, on := send(t, false), send(t, true)

	if len(off) != len(on) {
		t.Fatalf("field count differs: off=%d on=%d (%v vs %v)", len(off), len(on), off, on)
	}
	for field, want := range off {
		got := on[field]
		if len(got) != len(want) {
			t.Errorf("field %q: off has %d values, on has %d", field, len(want), len(got))
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("field %q[%d]:\n off: %q\n  on: %q", field, i, want[i], got[i])
			}
		}
	}
}
