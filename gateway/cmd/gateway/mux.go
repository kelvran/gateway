package main

import (
	"net/http"

	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
)

// dataPlaneAuth names the credential rule a data-plane route applies. The
// handlers own the check; the table states it so the OpenAPI document's
// security requirements will derive from one place (plan item 14, slice S3)
// and mux_test pins each mode against a live server.
type dataPlaneAuth int

const (
	authNone            dataPlaneAuth = iota // healthz, readyz, hello: no credential is read
	authBearer                               // Authorization alone: chat, embeddings (x-api-key is not read)
	authBearerOrXAPIKey                      // bearerFromRequest: models, messages, count_tokens
)

// dataPlaneEnvelope names the error body a route's own 405 carries.
type dataPlaneEnvelope int

const (
	envelopeOpenAI    dataPlaneEnvelope = iota // the OpenAI JSON envelope, Allow set
	envelopeAnthropic                          // the Anthropic JSON envelope, Allow set
	envelopePlainText                          // http.Error text, no Allow: healthz, readyz
)

// dataPlaneRoute is one row of the data-plane route table. The pattern
// registered is Path alone, deliberately method-less: the handler answers a
// wrong method itself, in Envelope, so the error body stays the route's own
// rather than net/http's plain text (error_envelope_test pins that).
type dataPlaneRoute struct {
	Method   string // the one method the handler accepts; every other is its own 405
	Path     string // an exact path; a trailing-slash variant is Go's plain 404
	Auth     dataPlaneAuth
	Envelope dataPlaneEnvelope
}

// dataPlaneRoutes are the exact routes newDataPlaneMux registers, in
// registration order; attribution_test probes every one of them, and the
// item 14 OpenAPI contract test (slice S3) will compare the document against
// this table, so a route added here is probed now and documented once S3
// lands. run() registers nothing outside the builder.
var dataPlaneRoutes = []dataPlaneRoute{
	{http.MethodPost, "/v1/chat/completions", authBearer, envelopeOpenAI},
	{http.MethodPost, "/v1/embeddings", authBearer, envelopeOpenAI},
	{http.MethodGet, "/v1/models", authBearerOrXAPIKey, envelopeOpenAI},
	{http.MethodGet, "/healthz", authNone, envelopePlainText},
	{http.MethodGet, "/readyz", authNone, envelopePlainText},
	{http.MethodHead, "/api/hello", authNone, envelopeOpenAI},
	{http.MethodPost, "/v1/messages", authBearerOrXAPIKey, envelopeAnthropic},
	{http.MethodPost, "/v1/messages/count_tokens", authBearerOrXAPIKey, envelopeAnthropic},
}

// newDataPlaneMux is the one place the data-plane routes are registered:
// run(), the shared integration helpers and every single-purpose test that
// registers only data-plane routes build their server from it, so a test is
// "wired the same way main.go wires it" by construction and a later slice
// adds a route without a main.go edit (item 11 slice S9a; /v1/messages
// landed here in S10a, /v1/messages/count_tokens in S10b). Every pattern is
// an exact path -- no subtree, so a trailing-slash variant is Go's plain 404
// -- and the server wraps the mux in dataPlaneHandler for attribution and
// in-flight tracking.
func newDataPlaneMux(p *dataplane.Pipeline) *http.ServeMux {
	mux := http.NewServeMux()
	for _, rt := range dataPlaneRoutes {
		switch rt.Path {
		case "/v1/chat/completions":
			mux.HandleFunc(rt.Path, chatCompletionsHandler(p))
		case "/v1/embeddings":
			mux.HandleFunc(rt.Path, embeddingsHandler(p))
		case "/v1/models":
			mux.HandleFunc(rt.Path, modelsHandler(p))
		case "/healthz":
			mux.HandleFunc(rt.Path, healthzHandler)
		case "/readyz":
			mux.HandleFunc(rt.Path, readyzHandler(p))
		case "/api/hello":
			mux.HandleFunc(rt.Path, helloHandler)
		case "/v1/messages":
			mux.HandleFunc(rt.Path, messagesHandler(p))
		case "/v1/messages/count_tokens":
			mux.HandleFunc(rt.Path, countTokensHandler(p))
		}
	}
	return mux
}

// helloHandler answers Claude Code's gateway probe, HEAD /api/hello
// (Anthropic's gateway-protocol page: a 2xx tells the client its base URL is
// a gateway that understands the hint headers, so it sends them). 204 with
// no body and no auth check -- it reveals nothing /healthz does not already;
// any other method is the method_not_allowed envelope with Allow: HEAD. The
// JSON envelope (not /healthz's plain-text 405) is deliberate: /api/hello is
// an API-client route, read by the same client code that parses the other
// /v1 error bodies, while /healthz answers load balancers.
func helloHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodHead)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// bearerFromRequest is the credential the data plane verifies: the
// Authorization header verbatim when present, else the x-api-key header --
// the Anthropic SDK's and Claude Code's credential header (ANTHROPIC_API_KEY)
// -- as a bearer, else "". A non-empty Authorization wins (an empty one
// is absent) so exactly one credential is ever verified: a wrong ANTHROPIC_AUTH_TOKEN beside a valid
// ANTHROPIC_API_KEY is a 401 with no fallback (RFC-1 §4, owner decision Q2).
// Read on /v1/models (S9a), /v1/messages (S10a) and /v1/messages/count_tokens (S10b).
// Never log either value.
func bearerFromRequest(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		return auth
	}
	if key := r.Header.Get("x-api-key"); key != "" {
		return "Bearer " + key
	}
	return ""
}
