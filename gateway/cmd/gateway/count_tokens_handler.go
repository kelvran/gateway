package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// POST /v1/messages/count_tokens (item 11 slice S10b, RFC-1 §9). The
// optional Anthropic endpoint Claude Code calls for exact context counts and
// falls back from, to a character-based estimate, when it answers 404. The
// handler reads only `model` from the body (the count needs no other member
// until the anthropic branch forwards the raw body, slice S11), then hands
// the body's own gates to dataplane.Pipeline.HandleCountTokens. Exact path,
// as every data-plane route: `/v1/messages/count_tokens/` is a plain 404.
func countTokensHandler(p *dataplane.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAnthropicStatus(w, http.StatusMethodNotAllowed, "method_not_allowed", "", "method not allowed")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				writeAnthropicStatus(w, http.StatusRequestEntityTooLarge, "request_too_large", "", "request body too large")
				return
			}
			writeAnthropicStatus(w, http.StatusBadRequest, "invalid_body", "", "reading request body")
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeAnthropicStatus(w, http.StatusBadRequest, "invalid_json", "", "invalid request body: "+err.Error())
			return
		}
		if req.Model == "" {
			writeAnthropicStatus(w, http.StatusBadRequest, "missing_required_parameter", "model", "model is required")
			return
		}
		ctx := telemetry.ExtractContext(r.Context(), r)
		counted, err := p.HandleCountTokens(ctx, bearerFromRequest(r), r.RemoteAddr, req.Model)
		if err != nil {
			writeAnthropicError(w, err)
			return
		}
		// Unreachable until slice S11: the provider's own count_tokens answer,
		// relayed as received.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if err := json.NewEncoder(w).Encode(json.RawMessage(counted)); err != nil {
			slog.Error("encoding count_tokens response", "error", err)
		}
	}
}
