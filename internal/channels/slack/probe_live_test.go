//go:build slackprobe

// STATUS: never run against a real Slack workspace.
//
// This is a diagnostic tool, not a test. It gates nothing, and it does not
// reflect the production code path: optsFor() and plainish() below are
// probe-local shorthands, NOT renderOptions() and plainTextFallback() from
// render.go. Do not read them as documentation of what GoClaw sends.
//
// When to use it: when a test Slack workspace is available and you want to
// settle the rendering questions for good — single newlines, mentions, entity
// escaping, emoji and URL expansion — instead of relying on the post-enable
// checklist an operator runs by eye.
//
// Live Slack probe harness. NOT part of the normal test suite — guarded by the
// `slackprobe` build tag so `go test ./...` never runs it.
//
// Purpose: answer the ten Slack rendering questions that no documentation
// answers, by posting real messages to a real workspace. The API returning
// ok:true does NOT mean the message rendered correctly, so every case prints a
// permalink for human inspection and the results file has a column for the
// visual verdict.
//
// Run:
//
//	GOCLAW_SLACK_BOT_TOKEN=xoxb-... \
//	SLACK_PROBE_CHANNEL=C0123456789 \
//	SLACK_PROBE_USER=U0123456789 \
//	SLACK_PROBE_CHANNEL_REF=C0123456789 \
//	go test -tags slackprobe -v -timeout 30m -run TestLiveProbe ./internal/channels/slack/
//
// Optional:
//
//	SLACK_PROBE_DELAY_MS=1200   // per-post delay; chat.postMessage is ~1/sec/channel
//	SLACK_PROBE_OUT=/path.md    // results file (default: ./slack-probe-results.md)
package slack

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
)

// mechanism is one of the three ways to put text on the wire. mrkdwn is the
// current production behavior and is probed alongside the two candidates so the
// results file shows a real before/after rather than a claim about it.
type mechanism string

const (
	mechMrkdwn   mechanism = "mrkdwn (hiện tại)"
	mechBlock    mechanism = "markdown block"
	mechMDText   mechanism = "markdown_text"
	mechBlockRaw mechanism = "markdown block (không text)"
)

// probeEnv holds the resolved environment for a probe run.
type probeEnv struct {
	api        *slackapi.Client
	channel    string
	userRef    string // U... for the <@U> mention case
	channelRef string // C... for the <#C> mention case
	delay      time.Duration
}

// results accumulates markdown rows written to the results file at the end of
// the run. Guarded because subtests may run in parallel in future edits.
var (
	resultsMu sync.Mutex
	results   = map[string][]string{}
	sections  []string
)

func record(section, row string) {
	resultsMu.Lock()
	defer resultsMu.Unlock()
	if _, ok := results[section]; !ok {
		sections = append(sections, section)
	}
	results[section] = append(results[section], row)
}

// mustEnv resolves the probe environment or skips the test. Skipping rather
// than failing keeps `go test -tags slackprobe ./...` usable without secrets.
func mustEnv(t *testing.T) probeEnv {
	t.Helper()

	token := os.Getenv("GOCLAW_SLACK_BOT_TOKEN")
	channel := os.Getenv("SLACK_PROBE_CHANNEL")
	if token == "" || channel == "" {
		t.Skip("probe skipped: set GOCLAW_SLACK_BOT_TOKEN and SLACK_PROBE_CHANNEL")
	}

	delay := 1200 * time.Millisecond
	if v := os.Getenv("SLACK_PROBE_DELAY_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 0 {
			delay = time.Duration(ms) * time.Millisecond
		}
	}

	return probeEnv{
		api:        slackapi.New(token),
		channel:    channel,
		userRef:    os.Getenv("SLACK_PROBE_USER"),
		channelRef: os.Getenv("SLACK_PROBE_CHANNEL_REF"),
		delay:      delay,
	}
}

// optsFor builds the MsgOptions for one mechanism.
//
// mechMrkdwn runs the real production converter so the baseline row is exactly
// what GoClaw ships today — being in package slack is what makes that possible.
//
// mechMDText deliberately omits MsgOptionText: markdown_text conflicts with
// both text and blocks (markdown_text_conflict). That conflict is probed
// separately in TestLiveProbeConflict.
func optsFor(m mechanism, body string) []slackapi.MsgOption {
	switch m {
	case mechMrkdwn:
		return []slackapi.MsgOption{slackapi.MsgOptionText(markdownToSlackMrkdwn(body), false)}
	case mechBlock:
		return []slackapi.MsgOption{
			slackapi.MsgOptionBlocks(slackapi.NewMarkdownBlock("", body)),
			slackapi.MsgOptionText(plainish(body), false),
		}
	case mechBlockRaw:
		return []slackapi.MsgOption{
			slackapi.MsgOptionBlocks(slackapi.NewMarkdownBlock("", body)),
		}
	case mechMDText:
		return []slackapi.MsgOption{slackapi.MsgOptionMarkdownText(body)}
	}
	return nil
}

// plainish is a deliberately crude notification-fallback stand-in. The real
// plainTextFallback belongs to Phase 3; this only needs to be non-empty so the
// push-notification cases have something to show.
func plainish(md string) string {
	r := strings.NewReplacer("**", "", "__", "", "`", "", "#", "", ">", "", "~~", "", "|", " ")
	return strings.TrimSpace(r.Replace(md))
}

// post sends one case and records the outcome. errCode is the raw Slack error
// string when the call fails — Phase 3's isFormatError needs those exact codes.
func (e probeEnv) post(t *testing.T, section, label string, m mechanism, body string) (ts string, errCode string) {
	t.Helper()

	_, ts, err := e.api.PostMessage(e.channel, optsFor(m, body)...)
	if err != nil {
		errCode = err.Error()
	}

	link := ""
	if ts != "" {
		if pl, plErr := e.api.GetPermalink(&slackapi.PermalinkParameters{Channel: e.channel, Ts: ts}); plErr == nil {
			link = pl
		}
	}

	status := "OK"
	if errCode != "" {
		status = "LỖI: `" + errCode + "`"
	}
	t.Logf("[%s] %s | %s | ts=%s | %s", m, label, status, ts, link)
	record(section, fmt.Sprintf("| %s | %s | %s | %s | | |", label, m, status, link))

	time.Sleep(e.delay)
	return ts, errCode
}

// update edits an existing message and records the outcome, including whether
// Slack rejected the transition (block_mismatch is the one to watch).
func (e probeEnv) update(t *testing.T, section, label string, ts string, opts ...slackapi.MsgOption) string {
	t.Helper()

	_, _, _, err := e.api.UpdateMessage(e.channel, ts, opts...)
	errCode := ""
	if err != nil {
		errCode = err.Error()
	}

	status := "OK"
	if errCode != "" {
		status = "LỖI: `" + errCode + "`"
	}
	link := ""
	if pl, plErr := e.api.GetPermalink(&slackapi.PermalinkParameters{Channel: e.channel, Ts: ts}); plErr == nil {
		link = pl
	}
	t.Logf("[update] %s | %s | ts=%s | %s", label, status, ts, link)
	record(section, fmt.Sprintf("| %s | update | %s | %s | | |", label, status, link))

	time.Sleep(e.delay)
	return errCode
}

// TestMain writes the results file after all probe tests finish so the output
// can be pasted straight into probe-results.md.
func TestMain(m *testing.M) {
	code := m.Run()
	writeResults()
	os.Exit(code)
}

func writeResults() {
	resultsMu.Lock()
	defer resultsMu.Unlock()
	if len(sections) == 0 {
		return
	}

	out := os.Getenv("SLACK_PROBE_OUT")
	if out == "" {
		out = "slack-probe-results.md"
	}

	var b strings.Builder
	b.WriteString("# Slack probe results\n\n")
	b.WriteString("Cột **Render** và **Kết luận** phải điền TAY sau khi xem message thật trên Slack desktop và mobile.\n")
	b.WriteString("API trả OK không có nghĩa render đúng.\n\n")
	for _, s := range sections {
		b.WriteString("## " + s + "\n\n")
		b.WriteString("| Case | Cơ chế | API | Permalink | Render (điền tay) | Kết luận (điền tay) |\n")
		b.WriteString("|---|---|---|---|---|---|\n")
		for _, row := range results[s] {
			b.WriteString(row + "\n")
		}
		b.WriteString("\n")
	}

	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "probe: write results failed: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "probe: results written to %s\n", out)
}
