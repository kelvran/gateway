package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/ingress/anthropicmsgs"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// POST /v1/messages -- the Anthropic Messages ingress (item 11 slice S10a,
// RFC-1 §9). The handler mirrors chatCompletionsHandler's shape: method
// check, the same body limit, decode (anthropicmsgs.Parse, which also fills
// adapter.ChatRequest.Passthrough), the same handler-level validators in the
// same order, then the same pipeline entry points -- buffered through
// HandleChatCompletion and anthropicmsgs.EncodeResponse, streamed through
// HandleChatCompletionStreamSink with the Anthropic SSE encoder as the sink.
// Everything the pipeline does for /v1/chat/completions (auth, budgets,
// rate limits, guardrails, cache, idempotency, routing, telemetry) runs
// unchanged; what differs is the wire format at both ends and the envelope.
// Registered on the exact path: Claude Code posts to /v1/messages?beta=true
// and ServeMux ignores the query; a subtree pattern would answer the bare
// path with a 301 the client treats as failure.

// anthropicKeepAlivePeriod is how long the Anthropic stream may stay silent
// before the encoder emits its own ping (an upstream without pings, such as
// Bedrock's event stream, would otherwise leave the client with no bytes
// during a long thinking pause). Checked 2026-10-10 against Claude Code's
// network-config page ("Streaming idle watchdogs"): through a custom
// ANTHROPIC_BASE_URL the byte-level watchdog aborts after 180 s (300 s when
// feature flags were not fetched) and the event-level one after 300 s, and
// arriving bytes -- pings included -- reset both; 15 s sits an order of
// magnitude under either.
//
// A variable, not a constant, so a test can shorten it and watch a ping land
// during a pause the mock upstream stages.
var anthropicKeepAlivePeriod = 15 * time.Second

func messagesHandler(p *dataplane.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAnthropicStatus(w, http.StatusMethodNotAllowed, "method_not_allowed", "", "method not allowed")
			return
		}

		// The anthropic-* header set is bounded before the body is read: the
		// headers are already parsed, and an oversized set on a 32 MiB body is
		// exactly the request the bound exists to refuse cheaply (slice S11a).
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

		req, pt, err := anthropicmsgs.Parse(body)
		if err != nil {
			code, param := parseErrorCode(err)
			writeAnthropicStatus(w, http.StatusBadRequest, code, param, err.Error())
			return
		}
		if code, param, message := validateMessagesRequest(req); code != "" {
			writeAnthropicStatus(w, http.StatusBadRequest, code, param, message)
			return
		}
		pt.ForwardHeaders = forward

		ctx := telemetry.ExtractContext(r.Context(), r)
		bearer := bearerFromRequest(r)
		endUser := endUserIDFor(r, pt)
		idempotencyKey := r.Header.Get("Idempotency-Key")
		// The response carrier serves both branches: the stream caller records
		// the relayable headers on it and the encoder copies them before the
		// first frame (slice S11b2); the buffered caller records body and
		// headers for the relay below (slice S11b).
		ctx, upstreamMeta := dataplane.WithUpstreamResponseMeta(ctx)
		if req.Stream {
			handleStreamingMessages(ctx, p, w, bearer, r.RemoteAddr, endUser, req, idempotencyKey)
			return
		}

		ctx, upstreamDuration := dataplane.WithOverheadTracker(ctx)
		requestStart := time.Now()
		resp, err := p.HandleChatCompletion(ctx, bearer, r.RemoteAddr, endUser, req, idempotencyKey)
		if err != nil {
			writeAnthropicError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set(overheadDurationHeader, strconv.FormatInt((time.Since(requestStart)-*upstreamDuration).Milliseconds(), 10))
		// Passthrough path (RFC-1 §9, slice S11b): an anthropic deployment's
		// own 2xx bytes, relayed as received -- model included -- together
		// with its x-should-retry and anthropic-ratelimit-unified-* headers,
		// once the pipeline accepted the response (a post-call block returns
		// an error above and relays nothing). A cache hit or an
		// Idempotency-Key replay made no upstream call, so the carrier is
		// empty and the canonical response is re-encoded below.
		if upstreamMeta.Relayable() {
			copyRelayHeaders(w.Header(), upstreamMeta.Header)
			if _, err := io.Copy(w, bytes.NewReader(upstreamMeta.Body)); err != nil {
				slog.Error("relaying anthropic message response", "error", err)
			}
			return
		}
		// Through the JSON encoder as a RawMessage rather than a bare Write:
		// the bytes are EncodeResponse's own json.Marshal output, and the
		// encoder validates them once more on the way out (the same shape
		// chatCompletionsHandler's json.NewEncoder(w).Encode(resp) has).
		if err := json.NewEncoder(w).Encode(json.RawMessage(anthropicmsgs.EncodeResponse(resp))); err != nil {
			slog.Error("encoding anthropic message response", "error", err)
		}
	}
}

// handleStreamingMessages is handleStreamingChatCompletion's twin: the SSE
// headers go out before the pipeline runs, the encoder is the sink, and a
// failure is answered by the channel still open -- the Anthropic envelope
// while nothing has been written, the encoder's terminal error event once
// the stream has started (a clean end before message_delta would read as a
// dropped connection). The keepalive runs until the pipeline returns and is
// joined before the error fork reads the tracker: the ping goroutine writes
// the tracker's flag through tw.Write/Flush, and a terminal error event
// needs no keepalive. The encoder pings only between the first event and
// the end of the message.
func handleStreamingMessages(ctx context.Context, p *dataplane.Pipeline, w http.ResponseWriter, bearer, remoteAddr, endUser string, req adapter.ChatRequest, idempotencyKey string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	tw := &writeTracker{ResponseWriter: w}
	enc := anthropicmsgs.NewSSEEncoder(tw)
	stop := sync.OnceFunc(enc.StartKeepAlive(ctx, anthropicKeepAlivePeriod))
	defer stop()
	err := p.HandleChatCompletionStreamSink(ctx, bearer, remoteAddr, endUser, req, enc, idempotencyKey)
	stop()
	if err != nil {
		if tw.written {
			_ = enc.WriteError(anthropicErrorType(errorStatus(err)), anthropicClientMessage(err))
			return
		}
		writeAnthropicError(tw, err)
	}
}

// parseErrorCode maps anthropicmsgs.Parse's sentinels to the envelope's
// code and param: a missing required member is missing_required_parameter
// naming it (the embeddings route's precedent), a body that is not a JSON
// object is invalid_json, anything else invalid_request.
func parseErrorCode(err error) (code, param string) {
	switch {
	case errors.Is(err, anthropicmsgs.ErrMissingModel):
		return "missing_required_parameter", "model"
	case errors.Is(err, anthropicmsgs.ErrMissingMaxTokens):
		return "missing_required_parameter", "max_tokens"
	case errors.Is(err, anthropicmsgs.ErrMissingMessages):
		return "missing_required_parameter", "messages"
	case errors.Is(err, anthropicmsgs.ErrInvalidBody):
		return "invalid_json", ""
	default:
		return "invalid_request", ""
	}
}

// validateMessagesRequest runs chatCompletionsHandler's handler-level
// validators, in its order (cheap counts first), on the parsed request.
// "" means valid.
func validateMessagesRequest(req adapter.ChatRequest) (code, param, message string) {
	if err := adapter.ValidateMessageCount(req.Messages); err != nil {
		return "invalid_request", "", err.Error()
	}
	if err := adapter.ValidateToolDefs(req.Tools); err != nil {
		return "invalid_request", "", err.Error()
	}
	if err := adapter.ValidateToolChoice(req.ToolChoice, req.Tools); err != nil {
		return "invalid_tool_choice", "tool_choice", err.Error()
	}
	if err := adapter.ValidateFieldSizes(req.Messages); err != nil {
		return "invalid_request", "", err.Error()
	}
	if err := adapter.ValidateContentParts(req.Messages); err != nil {
		return "invalid_request", "", err.Error()
	}
	if err := adapter.ValidateResponseFormatSchema(req.ResponseFormat); err != nil {
		return "invalid_request", "", err.Error()
	}
	return "", "", ""
}

// maxForwardedAnthropicHeaderBytes bounds the anthropic-* request headers
// an anthropic deployment receives on the passthrough path (RFC-1 §5, slice
// S11a): names plus values, summed. The names are an open list (the
// protocol page forbids allow-listing the values), so the size is the one
// thing the gateway can bound -- the same rule every other client-supplied
// list that leaves the process follows. Claude Code's whole set is a few
// hundred bytes; the server's own 1 MiB header cap is the only other limit.
const maxForwardedAnthropicHeaderBytes = 16 << 10

// headerBytes is the wire size of h: every name and value, summed.
func headerBytes(h http.Header) int {
	n := 0
	for name, values := range h {
		for _, v := range values {
			n += len(name) + len(v)
		}
	}
	return n
}

// forwardHeadersFrom collects every anthropic-* request header -- an open
// list, never an allow-list of the values seen today (the protocol page) --
// for the passthrough hop to an anthropic deployment (slice S11a: the
// dataplane's forwardAnthropicHeaders copies them, the set bounded by
// maxForwardedAnthropicHeaderBytes above); the dataplane also folds the
// normalised anthropic-beta values into the cache and idempotency
// fingerprints on every hop. nil when the request carries none.
func forwardHeadersFrom(h http.Header) http.Header {
	var out http.Header
	for name, values := range h {
		if !strings.HasPrefix(strings.ToLower(name), "anthropic-") {
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

// endUserIDFor is the end-user scope argument: X-Kelvran-End-User-Id wins
// when present (the existing contract, stampable by an operator's proxy);
// metadata.user_id, the Anthropic form, fills it only when the header is
// absent.
func endUserIDFor(r *http.Request, pt *adapter.Passthrough) string {
	if header := r.Header.Get(dataplane.EndUserIDHeader); header != "" {
		return header
	}
	return pt.EndUserID
}
