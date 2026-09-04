- **Status**: accepted
- **Date**: 2026-09-04
- **Author(s)**: project founder + Claude Code

## Summary

Implement real streaming support for the Bedrock adapter via Amazon Bedrock Runtime's `ConverseStream` API, closing the one gap `docs/rfcs/2026-09-04-bedrock-adapter.md` deliberately deferred. Unlike every other streaming-capable provider (OpenAI, Anthropic, Gemini, openaicompat), `ConverseStream`'s wire format is AWS's own binary `application/vnd.amazon.eventstream` framing — length-prefixed, CRC-checksummed, not text-based SSE — so this cannot be a `streaming.StreamDecoder` implementation the existing SSE-based machinery just plugs into. It requires a genuinely parallel decode path inside `dataplane/streaming.go`, dispatched by provider identity exactly like `setUpstreamAuthHeaders`/`streamUpstreamURL` already dispatch on `dep.Provider == "bedrock"` for the buffered adapter.

## Motivation

Confirmed directly against the live tree: `bedrock.Adapter` does not implement `streaming.StreamingAdapter`, and `dataplane/streaming.go`'s `ErrStreamingNotSupported` doc comment already (if now stale — see Drawbacks) named Bedrock as excluded. Grounded via a 3-angle dynamic-workflow research pass (binary framing structure, the real Go decoder library, `ConverseStream`'s real event shapes) plus a synthesis stage — then, before trusting any of it, independently re-verified the two highest-risk claims and one more by reading the actual downloaded `aws-sdk-go-v2` module source directly (not docs pages):

1. **`aws-sdk-go-v2/aws/protocol/eventstream` is real, standalone, and already effectively free.** Its own `go.mod` requires only `smithy-go` — already an indirect dependency of `gateway/go.mod` since the buffered Bedrock adapter's SigV4 signing pass. Confirmed real signatures directly from `decode.go`/`message.go`: `eventstream.NewDecoder(optFns ...func(*DecoderOptions)) *Decoder`, `(*Decoder).Decode(reader io.Reader, payloadBuf []byte) (Message, error)`, `Message{Headers Headers, Payload []byte}`, `Headers.Get(name string) Value` (returns `nil` if absent — **must nil-check before calling a method on the result**, since `Value` is an interface and a nil interface has no method set to dispatch to).
2. **Tool-use input arrives as an accumulating string fragment, not a whole object per chunk.** Confirmed by reading the real Go struct directly in `service/bedrockruntime/types/types.go`: `ToolUseBlockDelta{ Input *string }`, and by reading the actual wire deserializer in `deserializers.go`, which asserts the JSON `"input"` field is a Go `string` (`value.(string)`) — Bedrock's streaming tool-call contract is OpenAI's/`openaicompat`'s accumulate-across-chunks shape, **not** Gemini's whole-object-per-chunk shape. Getting this backwards would silently corrupt every streamed tool call's arguments.
3. **The streaming URL is a path-segment swap, not a colon-suffix swap.** The task's own initial framing assumed a Gemini-style `:converse-stream` suffix; reading `service/bedrockruntime/serializers.go` directly shows the real, current routes are `/model/{modelId}/converse` (buffered) and `/model/{modelId}/converse-stream` (streaming) — a REST-style path-segment difference, genuinely different in shape from Gemini's `streamUpstreamURL` colon-suffix derivation, not reusable as-is.

## Detailed Design

### Architectural seam: a parallel binary decode path, not a `StreamDecoder`

`streaming.StreamDecoder.Decode(raw streaming.SSEEvent)` is fundamentally line/text-shaped, built around `streaming.Reader`'s `bufio.Scanner`-based blank-line-delimited parser — feeding binary, CRC-checksummed frames through it would corrupt them. Rather than invent a new exported interface in `internal/streaming` for a single implementor (which would also couple that deliberately provider-agnostic package to AWS-specific types — `internal/streaming`'s own package doc states it's "provider-agnostic"), `bedrock` gets its own concrete, non-interface `StreamDecoder` type, and `dataplane/streaming.go`'s existing `streamDeployment` function dispatches to a new `streamDeploymentBedrock` sibling at its very top:

```go
func (p *Pipeline) streamDeployment(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw *streaming.Writer, firstChunkSent *bool) (adapter.ChatResponse, error) {
    if dep.Provider == "bedrock" {
        return p.streamDeploymentBedrock(ctx, dep, req, sw, firstChunkSent)
    }
    // ... existing SSE-based logic, unchanged ...
}
```

`streamDeploymentWithFallback` (the caller, handling deployment-fallback-on-error retry) needs **zero changes** — it already calls `p.streamDeployment` uniformly regardless of provider.

### `bedrock.Adapter.ToProvider` is reused unchanged for `ConverseStream`

Confirmed: Bedrock's Converse and ConverseStream request bodies are the identical shape (no `"stream"` field distinguishes them the way OpenAI's body does — only the URL path differs). `bedrockAdapter.ToProvider(upstreamReq)` (the exact same method the buffered path already calls) needs no streaming-specific variant at all.

### `bedrock.StreamDecoder` — genuinely stateless, simpler than every prior decoder

Unlike Anthropic's decoder (must track open content-block kinds across calls, since `content_block_delta` doesn't repeat its type) or Gemini's (`sentRole` bool, since role only arrives on the first chunk), Bedrock's real wire events are each **self-describing**: `contentBlockDelta`'s own payload carries either `{"text": ...}` or `{"toolUse": {"input": ...}}` directly — no external state needed to classify it. `messageStart{role}` is structurally guaranteed to fire exactly once, at the true start, per the real event sequence (`messageStart` → `[contentBlockStart → contentBlockDelta* → contentBlockStop]*` → `messageStop` → `metadata`) — no `sentRole`-style tracking needed either. `bedrock.StreamDecoder` therefore carries **zero fields**.

`Decode` never returns a `done` boolean at all (unlike `streaming.StreamDecoder`'s contract) — `metadata` (carrying real usage) is confirmed to arrive **after** `messageStop`, so signaling done on `messageStop` would drop the final usage event. Exactly like Gemini's `Decode` (which always returns `done=false`), the binary decode loop relies purely on `eventstream.Decoder.Decode` returning `io.EOF` when the transport closes — not a new mechanism, the same pattern already proven correct for Gemini.

```go
// bedrock/stream.go
type StreamDecoder struct{} // genuinely stateless

func NewStreamDecoder() *StreamDecoder { return &StreamDecoder{} }

func (d *StreamDecoder) Decode(msg eventstream.Message) ([]streaming.ChatCompletionChunk, *adapter.Usage, error) {
    if v := msg.Headers.Get(eventstreamapi.MessageTypeHeader); v != nil && v.String() != eventstreamapi.EventMessageType {
        // "error" or "exception" message-type -- a real, typed error, never silently dropped.
        return nil, nil, fmt.Errorf("bedrock: upstream stream %s: %s", v.String(), string(msg.Payload))
    }
    eventType := ""
    if v := msg.Headers.Get(eventstreamapi.EventTypeHeader); v != nil {
        eventType = v.String()
    }
    switch eventType {
    case "messageStart": // -> MessageDelta.Role, once
    case "contentBlockStart": // toolUse -> ToolCallDelta{Index, ID, Name}, no ArgumentsJSON yet
    case "contentBlockDelta": // text -> MessageDelta.Content; toolUse.input fragment -> ToolCallDelta{Index, ArgumentsJSON: fragment}
    case "contentBlockStop": // no client-visible delta
    case "messageStop": // stopReason -> FinishReason, via the EXISTING finishReasonFromBedrock (reused as-is)
    case "metadata": // usage -> returned as *adapter.Usage
    default: // forward-compatible: unknown event types produce no chunk, never an error
    }
    ...
}
```

### `dataplane/streaming.go`'s new binary loop

```go
func (p *Pipeline) streamDeploymentBedrock(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw *streaming.Writer, firstChunkSent *bool) (adapter.ChatResponse, error) {
    // ... resolve adapter, call ToProvider, call p.upstreamStream (unchanged call, see URL/Accept-header changes below) ...
    body, err := p.upstreamStream(ctx, dep, providerReq)
    defer func() { _ = body.Close() }()

    decoder := bedrock.NewStreamDecoder()
    dec := eventstream.NewDecoder()
    buf := make([]byte, 0, 64*1024) // safe to reuse across calls: eventstream.Decoder's own doc comment confirms this, as long as each Payload is fully consumed (it is, synchronously, before the next Decode call) before reuse
    acc := newStreamAccumulator()
    var finalUsage *adapter.Usage

    for {
        msg, err := dec.Decode(body, buf)
        if errors.Is(err, io.EOF) {
            break
        }
        if err != nil {
            return adapter.ChatResponse{}, fmt.Errorf("reading binary event stream from deployment %q: %w", dep.Name, err)
        }
        chunks, usage, decErr := decoder.Decode(msg)
        if decErr != nil {
            return adapter.ChatResponse{}, fmt.Errorf("decoding stream from deployment %q: %w", dep.Name, decErr)
        }
        if usage != nil {
            finalUsage = usage
        }
        for _, c := range chunks {
            acc.add(c)
            if writeErr := sw.WriteChunk(c); writeErr != nil {
                return adapter.ChatResponse{}, fmt.Errorf("writing streamed chunk to client: %w", writeErr)
            }
            *firstChunkSent = true
        }
    }
    return p.finishStreamedResponse(ctx, dep, req, sw, acc, finalUsage)
}
```

### Real refactor: extract the shared response-finishing tail

`streamDeployment`'s existing tail (usage-fallback warning, building the accumulated `ChatResponse`, the audit-only post-call guardrail check, writing the client-facing done sentinel) has **zero provider-specific logic** — confirmed by reading it directly. Extracted into `finishStreamedResponse(ctx, dep, req, sw, acc, finalUsage) (adapter.ChatResponse, error)`, called identically by both `streamDeployment` (after its existing SSE loop) and `streamDeploymentBedrock` (after its new binary loop) — avoiding ~35 duplicated lines, a real DRY win this refactor enables rather than one this RFC invents speculatively.

### URL and `Accept` header changes

`streamUpstreamURL`'s existing `dep.Provider != "gemini"` early-return needs a second real branch: Bedrock's derivation is `strings.TrimSuffix(path, "/converse") + "/converse-stream"` (a path-segment swap, confirmed above — not a colon-suffix). `NewHTTPUpstreamStreamCaller`'s currently-unconditional `httpReq.Header.Set("Accept", "text/event-stream")` becomes provider-conditional: `"application/vnd.amazon.eventstream"` for Bedrock, `"text/event-stream"` for every other provider (unchanged default).

### Tool-call argument accumulation — the real, load-bearing detail

`contentBlockStart`'s `toolUse` gives `{toolUseId, name}` (no `input` yet) → one `streaming.ToolCallDelta{Index: contentBlockIndex, ID, Name}`, mirroring OpenAI's/`openaicompat`'s "intro chunk carries ID+Name, no arguments" convention exactly. Each subsequent `contentBlockDelta`'s `toolUse.input` is a **fragment** of the JSON string — forwarded as-is via `ToolCallDelta{Index: contentBlockIndex, ArgumentsJSON: fragment}` (no ID/Name repeated), for the caller to concatenate in order, exactly matching `streaming.ToolCallDelta`'s own documented contract. Never accumulated internally by the decoder itself — accumulation is the existing `streamAccumulator`'s job, unchanged.

## Drawbacks

- **A second, non-interface-based dispatch path** inside `dataplane/streaming.go` — a real, if small, architectural asymmetry from every other provider's uniform `streaming.StreamingAdapter` path. Accepted because inventing a shared interface for exactly one binary-framed implementor would be premature abstraction with no second consumer to validate the shape against.
- `internal/streaming/types.go`'s `ErrStreamingNotSupported` doc comment ("Gemini, Bedrock, and openaicompat, in the current scaffolding") is **already stale before this RFC** — Gemini and openaicompat both shipped real streaming since. Corrected as part of this pass's docs, not a new staleness this RFC introduces.
- Exception/error message-types (`:message-type` = `"error"`/`"exception"`) surface as a generic typed error carrying the raw payload bytes this pass — no per-exception-type-specific handling (e.g. distinguishing throttling from validation errors). A real, deliberate first-pass simplification, not a silent gap.
- Citation streaming (`ContentBlockDeltaMemberCitation`), `reasoningContent` streaming, and guardrail-trace events are real Converse features with no canonical-schema equivalent — deliberately out of scope, same class of gap already named in the buffered Bedrock RFC for multi-modal content.

## Alternatives Considered

1. **Retrofit `streaming.SSEEvent`/`Reader` to carry binary frames** — rejected; the existing `Reader` is a `bufio.Scanner` over newline-delimited text, structurally incompatible with length-prefixed binary CRC-checksummed framing. Would corrupt data, not just be inelegant.
2. **Define a new exported `BinaryStreamDecoder` interface in `internal/streaming`** — rejected for now; would coincidentally import AWS-specific `eventstream.Message` into a package whose own doc comment states it's provider-agnostic, for exactly one real implementor. Revisit only if a second binary-framed provider ever appears.
3. **Assume tool-use `input` arrives as a whole object per chunk (Gemini's pattern)** — rejected, decisively, once independently verified against the real Go struct (`ToolUseBlockDelta.Input *string`) and the actual wire deserializer's own type assertion (`value.(string)`).
4. **Derive the streaming URL via a colon-suffix swap, matching Gemini's `streamUpstreamURL` pattern** — rejected once independently verified against `serializers.go`: Bedrock's real routes differ by a path segment (`/converse` → `/converse-stream`), not a colon-verb suffix.

## Unresolved Questions

- Per-exception-type-specific error handling (throttling vs. validation vs. other) — deferred; a generic typed error is the honest, correct-if-blunt first pass.
- Citation/`reasoningContent`/guardrail-trace streaming events — no canonical-schema equivalent exists; deferred alongside the buffered adapter's own named multi-modal gap.
- Whether a second binary-framed provider ever justifies promoting this pattern into a shared `internal/streaming` abstraction — not decided here, revisit only with real second-consumer evidence.

## Research Trail

Grounded via a 3-angle dynamic-workflow research pass (binary framing structure, the real Go decoder library, `ConverseStream`'s real event shapes) plus a synthesis stage. Before writing this RFC, further independently re-verified directly against the real, already-downloaded `aws-sdk-go-v2`/`aws-sdk-go-v2/service/bedrockruntime` module source in this machine's local Go module cache (not docs pages, not re-fetched blind): `aws/protocol/eventstream@v1.7.20`'s `go.mod` (confirms zero new dependencies beyond `smithy-go`, already indirect), `decode.go`/`message.go`/`header.go`/`header_value.go` (confirms `NewDecoder`/`Decode`/`Message`/`Headers.Get`/`Value` interface real signatures, and the documented safe-buffer-reuse contract), `eventstreamapi/headers.go` (confirms the real well-known header constants), `service/bedrockruntime@v1.60.0/types/types.go` and `deserializers.go` (confirms `ToolUseBlockDelta.Input *string` and the real wire-level `"input"` string-type assertion), and `serializers.go` (confirms the real `/converse` vs `/converse-stream` path-segment routes). Also read `gateway/internal/streaming/{types.go,reader.go}`, `gateway/internal/adapter/{gemini,openai}/stream.go`, `gateway/internal/gateway/dataplane/streaming.go` (the full existing `streamDeployment`/`streamDeploymentWithFallback` functions), `gateway/internal/gateway/dataplane/dataplane.go`'s `streamUpstreamURL`/`NewHTTPUpstreamStreamCaller`, and `gateway/internal/adapter/bedrock/bedrock.go` directly to ground every design decision in this specific codebase's real, current conventions.
