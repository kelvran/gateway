package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// countTokensHandler serves POST /v1/messages/count_tokens (item 11 slices
// S10b and S11c): the method, the 16 KiB bound on the forwarded anthropic-*
// headers (checked before the body is read), the 32 MiB body cap and the
// shallow model parse are the handler's; the gates, the anthropic branch and
// the 404 for every other deployment are HandleCountTokens'. On an anthropic
// deployment the answer is relayed as received with its relayable headers; a
// body the parser or the request validators reject is the parser's 400.
func countTokensHandler(p *dataplane.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAnthropicStatus(w, http.StatusMethodNotAllowed, "method_not_allowed", "", "method not allowed")
			return
		}
		// The client's anthropic-* headers travel to an anthropic deployment as
		// an open list by prefix, bounded as on /v1/messages (slice S11a) and
		// checked before the body is read, so an over-long header set is refused
		// cheaply.
		forward := forwardHeadersFrom(r.Header)
		if n := headerBytes(forward); n > maxForwardedAnthropicHeaderBytes {
			writeAnthropicStatus(w, http.StatusBadRequest, "invalid_request", "", fmt.Sprintf("anthropic-* request headers total %d bytes; this gateway forwards at most %d", n, maxForwardedAnthropicHeaderBytes))
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
		counted, err := p.HandleCountTokens(ctx, bearerFromRequest(r), r.RemoteAddr, req.Model, body, forward)
		if err != nil {
			var bodyErr *dataplane.CountTokensBodyError
			if errors.As(err, &bodyErr) {
				// The same code and param /v1/messages gives the same body:
				// validateMessagesRequest names a tool_choice rejection; the parser's
				// sentinels keep their codes; anything else is invalid_request.
				code, param := parseErrorCode(bodyErr.Err)
				if errors.Is(bodyErr.Err, adapter.ErrInvalidToolChoice) {
					code, param = "invalid_tool_choice", "tool_choice"
				}
				writeAnthropicStatus(w, http.StatusBadRequest, code, param, bodyErr.Err.Error())
				return
			}
			writeAnthropicError(w, err)
			return
		}
		// The anthropic deployment's own answer, relayed as received with its
		// relayable headers (item 11 slice S11c).
		copyRelayHeaders(w.Header(), counted.Header)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if _, err := io.Copy(w, bytes.NewReader(counted.Body)); err != nil {
			slog.Error("writing count_tokens response", "error", err)
		}
	}
}
