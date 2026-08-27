package slack

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

// Config-surface tests for markdown_native.
//
// Slack has TWO config structs and only one of them is live in a normal
// deployment: config.SlackConfig comes from the JSON5 file plus env overlay, but
// registerConfigChannels (cmd/gateway_channels_setup.go:34-36) returns early
// whenever a DB instance loader exists, so anything DB-backed goes through
// slackInstanceConfig instead. A field added to only one of them is a flag that
// cannot be turned on where it matters.

// factoryChannel builds a Slack channel through a factory with the given
// instance config JSON.
func factoryChannel(t *testing.T, cfgJSON string, withPendingStore bool) *Channel {
	t.Helper()

	creds := json.RawMessage(`{"bot_token":"xoxb-test","app_token":"xapp-test"}`)

	var (
		ch  any
		err error
	)
	if withPendingStore {
		ch, err = FactoryWithPendingStore(nil)("slack-test", creds, json.RawMessage(cfgJSON), nil, nil)
	} else {
		ch, err = Factory("slack-test", creds, json.RawMessage(cfgJSON), nil, nil)
	}
	if err != nil {
		t.Fatalf("factory error = %v", err)
	}

	sc, ok := ch.(*Channel)
	if !ok {
		t.Fatalf("factory returned %T, want *slack.Channel", ch)
	}
	return sc
}

func TestSlackInstanceConfigCarriesMarkdownNative(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		want bool
	}{
		{"absent means off", `{}`, false},
		{"explicit false", `{"markdown_native":false}`, false},
		{"explicit true", `{"markdown_native":true}`, true},
		{"true alongside other keys", `{"dm_policy":"open","markdown_native":true}`, true},
	}

	for _, tc := range cases {
		// Both factory bodies decode the same JSONB and build the same
		// config.SlackConfig by hand. They are near-duplicates, so a field wired
		// into one and not the other diverges silently.
		for _, withPending := range []bool{false, true} {
			label := tc.name
			if withPending {
				label += "/with pending store"
			}
			t.Run(label, func(t *testing.T) {
				ch := factoryChannel(t, tc.cfg, withPending)
				if got := ch.markdownNativeEnabled(); got != tc.want {
					t.Errorf("markdownNativeEnabled() = %v, want %v for config %s", got, tc.want, tc.cfg)
				}
			})
		}
	}
}

// TestSlackMarkdownNativeFlagStatesOnTheWire pins that the off states are the
// same wire body, byte for byte, as the shipped default — the rollback path is
// only worth something if it is genuinely identical.
func TestSlackMarkdownNativeFlagStatesOnTheWire(t *testing.T) {
	send := func(t *testing.T, flag *bool) url.Values {
		t.Helper()
		ch, ws := newWireTestChannel(t, nil)
		ch.config.MarkdownNative = flag

		err := ch.Send(context.Background(), bus.OutboundMessage{
			ChatID:  "C123",
			Content: richBody,
		})
		if err != nil {
			t.Fatalf("Send() error = %v", err)
		}
		return firstForm(t, ws, "chat.postMessage", 0)
	}

	off := false
	on := true

	nilForm := send(t, nil)
	falseForm := send(t, &off)
	trueForm := send(t, &on)

	// nil and false must be indistinguishable on the wire.
	if len(nilForm) != len(falseForm) {
		t.Fatalf("nil form has %d fields, false form has %d", len(nilForm), len(falseForm))
	}
	for field, want := range nilForm {
		got := falseForm[field]
		if len(got) != len(want) {
			t.Errorf("field %q: nil has %d values, false has %d", field, len(want), len(got))
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("field %q[%d]:\n nil: %q\nfalse: %q", field, i, want[i], got[i])
			}
		}
	}

	// And both must be the legacy mrkdwn payload.
	assertNoField(t, nilForm, "blocks")
	assertContains(t, nilForm.Get("text"), "*Đã tìm thấy 2 lỗi.*", "mrkdwn bold")

	// true is the only state that emits Block Kit.
	if trueForm.Get("blocks") == "" {
		t.Error("markdown_native=true produced no blocks")
	}
}
