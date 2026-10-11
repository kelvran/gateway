package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The data-plane patterns carry no method on purpose: every handler answers
// a wrong method itself, in its own envelope, which is what lets the OpenAPI
// document state each route's 405 shape. The tests here pin the table's
// Method, Auth and Envelope columns against the handlers, so a column is
// never a claim the code does not make.

var everyMethod = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}

// doDataPlane sends one request to a live data-plane server; the caller
// closes the body.
func doDataPlane(t *testing.T, gwURL, method, path string, headers http.Header, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, gwURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// assertMethodNotAllowedEnvelope checks a 405 against the row's Envelope:
// Allow names the one accepted method on the two JSON envelopes and is
// absent on the plain-text routes; the body is the envelope's own.
func assertMethodNotAllowedEnvelope(t *testing.T, rt dataPlaneRoute, method string, h http.Header, body []byte) {
	t.Helper()
	switch rt.Envelope {
	case envelopePlainText:
		if allow := h.Get("Allow"); allow != "" {
			t.Errorf("%s %s: Allow %q, want none on a plain-text route", method, rt.Path, allow)
		}
		if string(body) != "method not allowed\n" {
			t.Errorf("%s %s: body %q, want the plain-text 405", method, rt.Path, body)
		}
	case envelopeOpenAI:
		if allow := h.Get("Allow"); allow != rt.Method {
			t.Errorf("%s %s: Allow %q, want %q", method, rt.Path, allow, rt.Method)
		}
		var e apiErrorBody
		if err := json.Unmarshal(body, &e); err != nil || e.Error.Code == nil || *e.Error.Code != "method_not_allowed" {
			t.Errorf("%s %s: body %s, want the OpenAI envelope with code method_not_allowed (%v)", method, rt.Path, body, err)
		}
	case envelopeAnthropic:
		if allow := h.Get("Allow"); allow != rt.Method {
			t.Errorf("%s %s: Allow %q, want %q", method, rt.Path, allow, rt.Method)
		}
		if e := decodeAnthropicError(t, body); e.Error.Code != "method_not_allowed" {
			t.Errorf("%s %s: code %q, want method_not_allowed in the Anthropic envelope", method, rt.Path, e.Error.Code)
		}
	default:
		t.Fatalf("%s: unknown envelope %d", rt.Path, rt.Envelope)
	}
}

// TestDataPlaneRouteTableMethodsAreTheHandlersOwn: through the recorder
// (which keeps HEAD bodies) every method but the row's own is the handler's
// 405 in the row's envelope, and the pattern resolves whatever the method.
// Over a real server the row's own method is anything but 405, and a HEAD on
// a non-HEAD route arrives as 405 with Allow and no body -- the server strips
// it, so a client must not expect the envelope on HEAD.
func TestDataPlaneRouteTableMethodsAreTheHandlersOwn(t *testing.T) {
	mux := newDataPlaneMux(nil) // every handler checks the method before it touches the pipeline
	for _, rt := range dataPlaneRoutes {
		for _, method := range everyMethod {
			if _, pattern := mux.Handler(httptest.NewRequest(method, rt.Path, nil)); pattern != rt.Path {
				t.Errorf("%s %s resolved to %q, want %q: the pattern carries no method", method, rt.Path, pattern, rt.Path)
			}
			if method == rt.Method {
				continue
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(method, rt.Path, nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: %d, want the handler's own 405", method, rt.Path, rec.Code)
				continue
			}
			assertMethodNotAllowedEnvelope(t, rt, method, rec.Header(), rec.Body.Bytes())
		}
	}

	gw := newModelsIntegrationServer(t, modelsTestKeys(), nil)
	for _, rt := range dataPlaneRoutes {
		own := doDataPlane(t, gw.URL, rt.Method, rt.Path, nil, "")
		_ = own.Body.Close()
		if own.StatusCode == http.StatusMethodNotAllowed {
			t.Errorf("%s %s over the real server: 405, want the handler's own answer", rt.Method, rt.Path)
		}
		if rt.Method == http.MethodHead {
			continue
		}
		head := doDataPlane(t, gw.URL, http.MethodHead, rt.Path, nil, "")
		body, _ := io.ReadAll(head.Body)
		_ = head.Body.Close()
		if head.StatusCode != http.StatusMethodNotAllowed || len(body) != 0 {
			t.Errorf("HEAD %s over the real server: %d with %d body bytes, want 405 and no body", rt.Path, head.StatusCode, len(body))
		}
		if rt.Envelope != envelopePlainText && head.Header.Get("Allow") != rt.Method {
			t.Errorf("HEAD %s over the real server: Allow %q, want %q", rt.Path, head.Header.Get("Allow"), rt.Method)
		}
	}
}

// TestDataPlaneRouteTableAuthModes pins the Auth column on a live server:
// the credential-free routes answer without one; the Authorization-only
// routes treat a valid x-api-key alone as no credential (401) and the
// bearer as one; the alias routes accept a valid x-api-key alone and
// refuse an absent credential. Bodies are shaped to pass each handler's
// own validation so the pipeline's auth is what answers.
func TestDataPlaneRouteTableAuthModes(t *testing.T) {
	gw := newModelsIntegrationServer(t, modelsTestKeys(), nil)
	bodies := map[string]string{
		"/v1/chat/completions":      `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
		"/v1/embeddings":            `{"model":"text-embedding-3-small","input":"hi"}`,
		"/v1/messages":              `{"model":"gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		"/v1/messages/count_tokens": `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
	}
	withXAPIKey := http.Header{"x-api-key": {"all-secret"}}
	withBearer := http.Header{"Authorization": {"Bearer all-secret"}}
	status := func(headers http.Header, rt dataPlaneRoute) int {
		resp := doDataPlane(t, gw.URL, rt.Method, rt.Path, headers, bodies[rt.Path])
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for _, rt := range dataPlaneRoutes {
		switch rt.Auth {
		case authNone:
			if got := status(nil, rt); got == http.StatusUnauthorized {
				t.Errorf("%s %s without a credential: 401, want the route to need none", rt.Method, rt.Path)
			}
		case authBearer:
			if got := status(withXAPIKey, rt); got != http.StatusUnauthorized {
				t.Errorf("%s %s with only x-api-key: %d, want 401 (this route reads Authorization alone)", rt.Method, rt.Path, got)
			}
			if got := status(withBearer, rt); got == http.StatusUnauthorized {
				t.Errorf("%s %s with the bearer: 401, want admitted", rt.Method, rt.Path)
			}
		case authBearerOrXAPIKey:
			if got := status(withXAPIKey, rt); got == http.StatusUnauthorized {
				t.Errorf("%s %s with only x-api-key: 401, want the alias accepted", rt.Method, rt.Path)
			}
			if got := status(nil, rt); got != http.StatusUnauthorized {
				t.Errorf("%s %s without a credential: %d, want 401", rt.Method, rt.Path, got)
			}
		default:
			t.Fatalf("%s: unknown auth mode %d", rt.Path, rt.Auth)
		}
	}
}
