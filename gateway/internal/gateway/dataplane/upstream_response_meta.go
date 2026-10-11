package dataplane

import (
	"context"
	"net/http"
	"strings"
)

// UpstreamResponseMeta is what a passthrough hop to an anthropic deployment
// hands back beside the canonical response (RFC-1 §9, item 11 slices S11b
// and S11b2). The buffered caller records the 2xx body and the relayable
// headers; the stream caller records the headers alone (the frames are
// relayed as they arrive through streaming.RawRelay), so Relayable stays
// false for a stream and only the buffered handler ever relays Body:
// the upstream's 2xx body as received and the response headers the Anthropic
// Messages handler forwards (relayResponseHeaders). The handler relays Body
// byte-for-byte once the pipeline has accepted the response; the canonical
// shadow is still decoded for usage, cost, the cache and the post-call
// guardrail, and a response the pipeline refuses (a guardrail block, a
// decode failure) surfaces as an error, so these bytes are never written
// then. A cache hit or an Idempotency-Key replay makes no upstream call and
// leaves the carrier empty, so the handler re-encodes the canonical response
// as before -- RFC-1 §8's "cache hits are re-encoded"; a singleflight
// follower, whose own closure never ran, leaves it empty the same way. A hop
// that fails after its bytes were recorded -- a 2xx body the adapter cannot
// decode -- empties the carrier again (clearUpstreamResponseMeta, called by
// callDeployment on every error after the upstream call), so the fallback hop
// that follows never serves its canonical response beside another
// deployment's bytes.
type UpstreamResponseMeta struct {
	Provider   string
	StatusCode int
	Body       []byte
	Header     http.Header
}

// Relayable reports whether the carrier holds an anthropic deployment's 2xx
// body.
func (m *UpstreamResponseMeta) Relayable() bool {
	return m != nil && m.Provider == "anthropic" && m.StatusCode >= 200 && m.StatusCode < 300 && len(m.Body) > 0
}

type upstreamResponseMetaKey struct{}

// WithUpstreamResponseMeta returns a context carrying the pointer the HTTP
// caller fills on a passthrough hop; read it after HandleChatCompletion
// returns, never during (WithOverheadTracker's single-goroutine rule).
func WithUpstreamResponseMeta(ctx context.Context) (context.Context, *UpstreamResponseMeta) {
	m := new(UpstreamResponseMeta)
	return context.WithValue(ctx, upstreamResponseMetaKey{}, m), m
}

// UpstreamResponseMetaFromContext returns the carrier WithUpstreamResponseMeta
// stored, or nil when the caller never asked for one (a test or probe
// driving the pipeline directly).
func UpstreamResponseMetaFromContext(ctx context.Context) *UpstreamResponseMeta {
	m, _ := ctx.Value(upstreamResponseMetaKey{}).(*UpstreamResponseMeta)
	return m
}

// recordUpstreamResponseMeta fills the carrier, if any, with a 2xx response.
func recordUpstreamResponseMeta(ctx context.Context, provider string, resp *http.Response, body []byte) {
	m := UpstreamResponseMetaFromContext(ctx)
	if m == nil || resp == nil {
		return
	}
	*m = UpstreamResponseMeta{Provider: provider, StatusCode: resp.StatusCode, Body: body, Header: relayResponseHeaders(resp.Header)}
}

// clearUpstreamResponseMeta empties the carrier, if any. callDeployment calls
// it on every error after the upstream call: a 2xx the adapter cannot decode
// has already filled the carrier, and the fallback hop that follows must not
// inherit those bytes.
func clearUpstreamResponseMeta(ctx context.Context) {
	if m := UpstreamResponseMetaFromContext(ctx); m != nil {
		*m = UpstreamResponseMeta{}
	}
}

// relayResponseHeaders keeps the two classes of upstream response header the
// protocol page asks a gateway to pass through unchanged -- x-should-retry,
// which Claude Code reads when deciding whether to retry, and the
// anthropic-ratelimit-unified-* set it shows as plan usage -- and drops every
// other header (request and organisation ids, internals). nil when none.
func relayResponseHeaders(src http.Header) http.Header {
	var out http.Header
	for name, values := range src {
		lower := strings.ToLower(name)
		if lower != "x-should-retry" && !strings.HasPrefix(lower, "anthropic-ratelimit-unified-") {
			continue
		}
		if out == nil {
			out = http.Header{}
		}
		for _, v := range values {
			out.Add(name, v)
		}
	}
	return out
}
