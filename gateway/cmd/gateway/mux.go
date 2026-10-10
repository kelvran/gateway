package main

import (
	"net/http"

	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
)

// dataPlaneRoutes are the exact patterns newDataPlaneMux registers, in
// registration order; attribution_test probes every one of them, so a route
// added here is probed automatically (run() registers nothing outside the
// builder).
var dataPlaneRoutes = []string{"/v1/chat/completions", "/v1/embeddings", "/v1/models", "/healthz", "/readyz", "/api/hello"}

// newDataPlaneMux is the one place the data-plane routes are registered:
// run(), the shared integration helpers and every single-purpose test that
// registers only data-plane routes build their server from it, so a test is
// "wired the same way main.go wires it" by construction and a later slice
// adds a route without a main.go edit (item 11 slice S9a; the
// /v1/messages routes land here in S10a). Every pattern is an exact path --
// no subtree, so a trailing-slash variant is Go's plain 404 -- and the
// server wraps the mux in dataPlaneHandler for attribution and in-flight
// tracking.
func newDataPlaneMux(p *dataplane.Pipeline) *http.ServeMux {
	mux := http.NewServeMux()
	for _, route := range dataPlaneRoutes {
		switch route {
		case "/v1/chat/completions":
			mux.HandleFunc(route, chatCompletionsHandler(p))
		case "/v1/embeddings":
			mux.HandleFunc(route, embeddingsHandler(p))
		case "/v1/models":
			mux.HandleFunc(route, modelsHandler(p))
		case "/healthz":
			mux.HandleFunc(route, healthzHandler)
		case "/readyz":
			mux.HandleFunc(route, readyzHandler(p))
		case "/api/hello":
			mux.HandleFunc(route, helloHandler)
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
// Read on /v1/models today; the /v1/messages routes read it from S10a.
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
