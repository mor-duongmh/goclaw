package slack

import (
	"net/http"
	"os"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

// Send deadlines exist because slack-go honors the Retry-After header value
// uncapped (only its missing-header fallback is configurable), and an outbound
// shard delivers strictly in order on a single goroutine: an unbounded sleep in
// one Slack send stalls every other conversation on that shard, fills its queue,
// and blocks the shared bus reader for all channels.
//
// One Send gets ONE attempt budget covering every API call it makes — the
// placeholder delete, every attachment, every chunk, and every retry inside
// them. The budget does not restart per API call or per attachment, so a reply
// with N attachments costs the same wall clock ceiling as a reply with one.
//
// On top of that a Send may claim the delivery reserve AT MOST ONCE, so the
// per-Send ceiling is attempt + reserve and nothing scales with N.
const (
	// slackSendBudget bounds a text-only Send, reasoning bubbles included. This
	// is the hot path, so it stays tight.
	slackSendBudget = 60 * time.Second

	// slackMediaSendBudget bounds a Send that carries attachments: all uploads,
	// the placeholder delete and the caption together. Uploads are slow and
	// several large files must fit, so it is deliberately generous. A media send
	// that runs long does head-of-line-block its outbound shard; that is an
	// accepted trade against truncating a multi-file reply.
	slackMediaSendBudget = 15 * time.Minute

	// slackDeliveryReserve funds a delivery that REPLACES a failed attempt: the
	// new message sent after a placeholder edit failed, an upload-failure notice,
	// or a caption left stranded by uploads that consumed the attempt budget.
	// It is taken from the caller's context, never from the attempt budget,
	// because an attempt that burned every second it had must not be able to
	// starve the message that actually carries the answer.
	slackDeliveryReserve = 20 * time.Second

	// slackAPICallTimeout bounds a single Slack call made outside a Send, where
	// there is no logical send to budget as a whole.
	slackAPICallTimeout = 30 * time.Second

	// slackAuthTimeout bounds auth.test during Start. It is deliberately BELOW
	// InstanceLoader's reloadStartTimeout (90s): the loader does not retry a
	// failed Start, and on ITS timeout it stops the channel while Start keeps
	// running — a late success then calls SetRunning(true) and the channel
	// consumes Slack events while the manager has recorded it as failed
	// (internal/channels/instance_loader.go:437-453 logs exactly that).
	// Failing cleanly inside the loader's window is safer than racing past it
	// into that split-brain state.
	slackAuthTimeout = 60 * time.Second
)

// sendBudget reports the attempt budget for one outbound message. Attachments
// are the only thing that moves it: everything a media send does shares this one
// budget, so it has to cover several uploads.
func sendBudget(msg bus.OutboundMessage) time.Duration {
	if len(msg.Media) > 0 {
		return slackMediaSendBudget
	}
	return slackSendBudget
}

// slackRetryDisabledEnv turns HTTP retries off without a new binary. Named and
// parsed to match the repo's existing disable knobs (see dashScopeCacheDisabled
// in internal/providers/dashscope_cache_middleware.go): a DISABLE_ prefix so the
// name cannot be mistaken for an attempt count, and a truthy value list rather
// than strconv.ParseBool so "yes" works like "1". Read at channel start, so it
// takes effect on the next process or channel restart.
const slackRetryDisabledEnv = "GOCLAW_DISABLE_SLACK_SEND_RETRY"

// slackRetryMaxAttempts is the retry budget when the env knob leaves retries on.
const slackRetryMaxAttempts = 3

// slackRetryConfig builds the HTTP retry policy for Slack API clients.
//
// Only 429 and connection errors retry. 5xx deliberately does not:
// chat.postMessage is not idempotent, and a 5xx may mean the message landed, so
// resending would duplicate it. slack-go agrees — NewServerErrorRetryHandler is
// opt-in and absent from AllBuiltinRetryHandlers.
//
// Requests whose body cannot be replayed (file uploads stream it once) are never
// retried by the library, so no reader-reset hook is needed here.
func slackRetryConfig() slackapi.RetryConfig {
	cfg := slackapi.DefaultRetryConfig()
	cfg.MaxRetries = slackRetryMaxAttempts
	if slackRetryDisabled() {
		// MaxRetries 0 makes slack-go skip the retry wrapper entirely.
		cfg.MaxRetries = 0
	}
	// Connection errors + 429. Handlers must be built from the final config so
	// the rate-limit handler sees the same Retry-After fallback and jitter.
	cfg.Handlers = slackapi.AllBuiltinRetryHandlers(cfg)
	return cfg
}

// slackRetryDisabled reports whether the operator has turned HTTP retries off.
func slackRetryDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(slackRetryDisabledEnv))) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

// slackTransport is the single funnel every Slack API request passes through.
// It exists because no source-level check can enumerate the callers: the
// socketmode client embeds a COPY of this client by value (socketmode.New does
// Client: *api), so sm.PostMessage(...) compiles and carries this same policy
// while naming a different receiver, and apps.connections.open has no call site
// in this repo at all -- it is issued from inside socketmode's reconnect loop.
// Both land here, which is what makes a deadline check complete.
type slackTransport struct {
	base    http.RoundTripper
	observe func(*http.Request) // test-only; nil in production
}

func (t slackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.observe != nil {
		t.observe(req)
	}
	return t.base.RoundTrip(req)
}

// newSlackAPIClient is the only way this package builds a Slack client, so every
// client in the process carries the same retry policy and the same transport.
// TestSlackClientsComeFromOneConstructor enforces that.
func newSlackAPIClient(token string, extra ...slackapi.Option) *slackapi.Client {
	return newSlackAPIClientWithPolicy(token, slackRetryConfig(), nil, extra...)
}

// newSlackAPIClientWithPolicy takes the retry policy and a request observer so
// tests can make waits deterministic and inspect what reached the wire. Only
// tests may call it: production has no policy argument to get wrong.
func newSlackAPIClientWithPolicy(token string, cfg slackapi.RetryConfig, observe func(*http.Request), extra ...slackapi.Option) *slackapi.Client {
	opts := make([]slackapi.Option, 0, len(extra)+2)
	opts = append(opts, extra...)
	// OptionHTTPClient must precede OptionRetryConfig: slack-go wraps whichever
	// client is installed at the time the retry option runs.
	opts = append(opts, slackapi.OptionHTTPClient(&http.Client{
		Transport: slackTransport{base: http.DefaultTransport, observe: observe},
	}))
	opts = append(opts, slackapi.OptionRetryConfig(cfg))
	return slackapi.New(token, opts...)
}
