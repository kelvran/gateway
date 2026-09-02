- **Status**: accepted
- **Date**: 2026-09-02
- **Author(s)**: project founder + Claude Code

## Summary

Add real SSE streaming support to the Gateway's dataplane, for the two real adapters (OpenAI, Anthropic) only — matching the scope discipline `docs/rfcs/2026-09-02-initial-code-scaffolding.md` already established (the stubbed adapters — Gemini, Bedrock, OpenAI-compat — stay stubbed; streaming for them is out of scope here, not silently implied). Every request the gateway currently serves is buffered, non-streaming, per that RFC's explicit deferral. This closes that gap.

## Motivation

Real LLM traffic is overwhelmingly streaming — most production clients (chat UIs, agent frameworks) request `stream: true` and expect incremental tokens, not a single blocked response. The gateway cannot realistically serve most production traffic until this lands, which is why it was picked as the next priority over distributed rate limiting.

## Detailed Design

### Canonical streaming chunk shape (client-facing, OpenAI-compatible)

```go
type ChatCompletionChunk struct {
    ID      string
    Model   string
    Choices []ChunkChoice
    Usage   *Usage // non-nil only on the final chunk, when the provider sends usage
}
type ChunkChoice struct {
    Index        int
    Delta        MessageDelta
    FinishReason *string
}
type MessageDelta struct {
    Role      string // present only on the first chunk
    Content   string // incremental text fragment
    ToolCalls []ToolCallDelta
}
type ToolCallDelta struct {
    Index         int
    ID            string // present only on the first chunk for this tool call
    Name          string // present only on the first chunk for this tool call
    ArgumentsJSON string // incremental JSON-string fragment
}
```

### The seam: `StreamDecoder` + `StreamingAdapter`

A **separate, additive** interface from the existing stateless `Adapter` — streaming decoding is inherently stateful (Anthropic's typed event sequence requires tracking which content block is open and which tool-call index is accumulating, per `gateway/ARCHITECTURE.md`'s already-documented hazard), so it cannot live on the same interface as `ToProvider`/`FromProvider` without breaking their statelessness guarantee.

```go
type SSEEvent struct {
    Event string // the "event:" line, if present (Anthropic uses this; OpenAI doesn't)
    Data  string // the "data:" line's payload
}

type StreamDecoder interface {
    // Decode consumes one raw provider-native SSE event and returns zero or
    // more canonical chunks ready to forward to the client. done=true once
    // the provider's stream is logically complete (OpenAI's "[DONE]"
    // sentinel; Anthropic's message_stop event). finalUsage is non-nil the
    // moment usage info is observed (may arrive before done=true).
    Decode(raw SSEEvent) (chunks []ChatCompletionChunk, done bool, finalUsage *Usage, err error)
}

// Only OpenAI and Anthropic implement this. Gemini/Bedrock/openaicompat do
// not — a type-assertion at the router decides whether streaming is even
// possible for a given deployment's provider, per the stub-honestly rule.
type StreamingAdapter interface {
    Adapter
    NewStreamDecoder() StreamDecoder
}
```

A shared, provider-agnostic SSE transport reader/writer (splitting an `io.Reader` on blank-line-delimited `event:`/`data:` frames; writing canonical chunks to the client as `data: {json}\n\n` + a final `data: [DONE]\n\n`, flushing via `http.Flusher` after every write) lives in a new `gateway/internal/streaming/` package — this is transport framing, not provider translation, and belongs in neither `adapter` nor `cache`.

### Dataplane wiring

- **Cache hit + `stream: true`**: fake a stream from the cached complete response — emit the full content as a single delta chunk, then a finish-reason chunk, then `[DONE]`. Honest about being a synthesized stream from an already-known answer, not a re-play of the original token timing.
- **Cache miss + `stream: true`**: require the resolved deployment's adapter to satisfy `StreamingAdapter`; if it doesn't (Gemini/Bedrock/openaicompat), return a clear, typed error — never silently fall back to buffering. Otherwise: call upstream with the provider's own streaming flag set, read the raw SSE frames, feed each to `Decode`, forward every returned chunk to the client immediately (flushed), and **simultaneously** accumulate them into a full canonical `ChatResponse` for cache write-back and structured logging — a tee, not a choice between the two.
- **Fallback/retry scope boundary**: fallback to the next deployment on upstream failure only applies **before the first chunk has been forwarded to the client**. Once any byte of the stream has reached the client, there is no clean retry — the stream ends (with an error chunk if the failure is mid-stream), and this is stated explicitly rather than attempting a silent, potentially-duplicating retry.
- **Cost accounting**: computed from `finalUsage` when the provider supplies it (OpenAI's `stream_options.include_usage`; Anthropic's `message_delta`/`message_stop` usage fields). If a provider stream ends without ever sending usage, log a clear warning and record a zero/estimated cost rather than silently omitting the log entry.

## Drawbacks

- Two independent stateful decoders (one per real provider) is more code than a single stateless translation, and stateful code is inherently harder to test exhaustively — mitigated by requiring each decoder's test suite to cover every documented SSE event type for its provider, not just the happy path.
- The fallback-only-before-first-byte rule means a mid-stream provider failure is a real, visible failure to the client, not smoothed over — accepted as the honest tradeoff; smoothing it over would risk duplicating already-forwarded content.

## Alternatives Considered

1. **Buffer the full stream server-side, then send it as one chunk** — rejected: this isn't streaming, it defeats the entire motivation (time-to-first-token).
2. **A single generic `StreamDecoder` shared across providers with per-provider config** — rejected: OpenAI's and Anthropic's event shapes are structurally different enough (homogeneous deltas vs. typed event sequence with block-open/close semantics) that a shared implementation would need as many branches as two separate ones, with less clarity.
3. **Attempt mid-stream fallback/retry with de-duplication** — rejected as premature: real de-duplication (tracking exactly which tokens the client already received and resuming from there) is a genuinely hard problem across two different providers' resumption semantics; not worth building until there's evidence mid-stream failures are common enough to justify it.

## Unresolved Questions

- Whether cache-hit fake-streaming should ever chunk the cached content into multiple smaller deltas (to better mimic real streaming UX) rather than one big delta — left as a future refinement, not decided here; one big delta is correctness-equivalent and simpler.
- Whether `finalUsage`-missing should be a hard error rather than a warning+zero-cost — left as-is (warning) for this pass; revisit if it causes real cost-accounting drift once there's production traffic.
