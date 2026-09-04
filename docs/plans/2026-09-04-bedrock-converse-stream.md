> **For agentic executors:** work through this task-by-task, checking off each step as it's done. Don't skip ahead — a later task may depend on an earlier one's actual output, not just its description.

---

**Goal:** Real Bedrock streaming via `ConverseStream`, closing the gap `docs/rfcs/2026-09-04-bedrock-adapter.md` deliberately deferred. A parallel binary decode path, not a `streaming.StreamDecoder` implementation.

**Architecture:** `bedrock/stream.go` — a genuinely stateless `StreamDecoder` (no cross-call fields), decoding `eventstream.Message`s (not `streaming.SSEEvent`s). `dataplane/streaming.go` — `streamDeployment` dispatches to a new `streamDeploymentBedrock` sibling at its top for `dep.Provider == "bedrock"`; a new `finishStreamedResponse` helper extracts the provider-agnostic response-finishing tail both paths share. `dataplane.go` — `streamUpstreamURL` gains a real Bedrock branch (path-segment swap, not colon-suffix); `NewHTTPUpstreamStreamCaller`'s `Accept` header becomes provider-conditional.

**Tech Stack:** No new dependency — `aws-sdk-go-v2/aws/protocol/eventstream` requires only `smithy-go`, already an indirect dependency since the buffered Bedrock adapter's SigV4 signing pass. Confirmed directly from the module's own `go.mod`.

**Spec:** `docs/rfcs/2026-09-04-bedrock-converse-stream.md`.

**Global Constraints:**
- Tool-use input arrives as an accumulating **string fragment** per chunk (OpenAI's/`openaicompat`'s pattern), confirmed against the real `ToolUseBlockDelta.Input *string` struct and the real wire deserializer — never treat it as a whole JSON object per chunk (that would be Gemini's pattern, confirmed wrong for Bedrock).
- `bedrock.StreamDecoder.Decode` never returns a `done` boolean — `metadata` (usage) is confirmed to arrive *after* `messageStop`; the binary loop relies purely on `eventstream.Decoder.Decode` returning `io.EOF`, exactly like Gemini's existing `done=false`-always pattern.
- `msg.Headers.Get(name)` returns a `nil` interface when absent — always nil-check before calling `.String()`/`.Get()` on the result; a nil-interface method call panics.
- An `:message-type` of `"error"`/`"exception"` must surface as a real, typed error from `Decode` — never silently dropped.
- `streamDeploymentWithFallback` needs zero changes — it already calls `streamDeployment` uniformly; the dispatch lives entirely inside `streamDeployment` itself.
- The `eventstream.Decoder`'s `payloadBuf` is safe to reuse across the whole loop (confirmed via its own doc comment) as long as each message's payload is fully consumed before the next `Decode` call — it is, synchronously.

---

## Phase 1: `bedrock.StreamDecoder`

### Task 1: `bedrock/stream.go`

**Files:**
- Create: `gateway/internal/adapter/bedrock/stream.go`
- Test: `gateway/internal/adapter/bedrock/stream_test.go`

**Steps:**
- [ ] Define wire-level event payload structs (plain `encoding/json` tags, confirmed real field names from `deserializers.go`): `messageStartEvent{Role string}`, `contentBlockStartEvent{ContentBlockIndex int, Start struct{ToolUse *struct{ToolUseID, Name string} \`json:"toolUse,omitempty"\`}}`, `contentBlockDeltaEvent{ContentBlockIndex int, Delta struct{Text string \`json:"text,omitempty"\`; ToolUse *struct{Input string} \`json:"toolUse,omitempty"\`}}`, `contentBlockStopEvent{ContentBlockIndex int}`, `messageStopEvent{StopReason string}`, `metadataEvent{Usage Usage}` (reuse the existing `bedrock.Usage` type from `bedrock.go`).
- [ ] `StreamDecoder` struct with zero fields (genuinely stateless — name this explicitly in a doc comment, don't just leave it looking incomplete).
- [ ] `NewStreamDecoder() *StreamDecoder`.
- [ ] `(*StreamDecoder) Decode(msg eventstream.Message) ([]streaming.ChatCompletionChunk, *adapter.Usage, error)`: check `:message-type` first (nil-safe), return a typed error for `"error"`/`"exception"` carrying the raw payload; read `:event-type` (nil-safe); switch on the 6 real event types per the RFC's exact mapping, unmarshaling `msg.Payload` into the matching struct; `contentBlockStart`'s `toolUse` → `ToolCallDelta{Index, ID, Name}` (no `ArgumentsJSON`); `contentBlockDelta`'s `toolUse.input` → `ToolCallDelta{Index, ArgumentsJSON: fragment}` (no ID/Name); `contentBlockDelta`'s `text` → `MessageDelta.Content`; `messageStop`'s `stopReason` → reuse the *existing* `finishReasonFromBedrock` from `bedrock.go` unchanged; `metadata`'s `usage` → returned `*adapter.Usage`; unknown event types → no chunk, no error (forward-compatible).
- [ ] Tests: a `messageStart` event produces a chunk with `Delta.Role="assistant"`; a `contentBlockStart` with `toolUse` produces a `ToolCallDelta` with `ID`/`Name` set and empty `ArgumentsJSON`; two sequential `contentBlockDelta` `toolUse.input` fragments for the same index concatenate (via a test-level accumulation check) to the original JSON; a `contentBlockDelta` `text` event produces `MessageDelta.Content`; a `messageStop` with `stopReason="tool_use"` produces `FinishReason="tool_calls"` (proving reuse of the existing mapping function); a `metadata` event produces the real `*adapter.Usage`; an `:message-type`=`"exception"` message produces a real error, never a chunk; a nil `Headers.Get` result (missing `:event-type`) never panics.
- [ ] `cd gateway && go test ./internal/adapter/bedrock/... -v`.

---

## Phase 2: Dataplane wiring

### Task 1: Extract `finishStreamedResponse`

**Files:**
- Modify: `gateway/internal/gateway/dataplane/streaming.go`

**Steps:**
- [ ] Extract `streamDeployment`'s existing tail (usage-fallback warning through `sw.WriteDone()`) into `finishStreamedResponse(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw *streaming.Writer, acc *streamAccumulator, finalUsage *adapter.Usage) (adapter.ChatResponse, error)`, per the RFC's exact snippet.
- [ ] `streamDeployment` calls it at its own tail; confirm behavior is byte-for-byte unchanged (this is a pure refactor, zero new logic) — the decisive proof is every pre-existing streaming test passing with zero assertion changes.
- [ ] `cd gateway && go test ./internal/gateway/dataplane/... -v` — zero regressions, zero test changes needed.

### Task 2: `streamUpstreamURL` + `Accept` header

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go`
- Modify: `gateway/internal/gateway/dataplane/streamurl_test.go`

**Steps:**
- [ ] Add a real Bedrock branch to `streamUpstreamURL`: `strings.TrimSuffix(path, "/converse") + "/converse-stream"` (confirmed real path-segment swap, not a colon-suffix) — return a clear error if `dep.BaseURL` doesn't end in `/converse`.
- [ ] `NewHTTPUpstreamStreamCaller`: make the `Accept` header provider-conditional (`"application/vnd.amazon.eventstream"` for `"bedrock"`, `"text/event-stream"` for everyone else, unchanged default).
- [ ] New tests mirroring `TestStreamUpstreamURLGeminiDerivesStreamingEndpoint`'s exact convention: a real Bedrock `base_url` ending in `/converse` correctly derives `.../converse-stream`; a malformed one (not ending in `/converse`) fails loudly; every non-Bedrock, non-Gemini provider's URL is unchanged (extend the existing `TestStreamUpstreamURLNonGeminiProvidersUnchanged`-style backward-compatibility test).
- [ ] `cd gateway && go test ./internal/gateway/dataplane/... -run StreamUpstreamURL -v`.

### Task 3: `streamDeploymentBedrock`

**Files:**
- Modify: `gateway/internal/gateway/dataplane/streaming.go` (new function + `streamDeployment`'s top-level dispatch)

**Steps:**
- [ ] Add the `if dep.Provider == "bedrock" { return p.streamDeploymentBedrock(...) }` dispatch at the very top of `streamDeployment`.
- [ ] Implement `streamDeploymentBedrock` per the RFC's exact snippet: resolve the `*bedrock.Adapter` from `p.adapters` (defensive error if the type assertion fails — should never happen in practice since `dep.Provider == "bedrock"` always resolves to `*bedrock.Adapter` in `main.go`'s registry, but never silently ignore a mismatch), call `ToProvider` (reused unchanged), call `p.upstreamStream` (unchanged call site — the URL/Accept-header logic from Task 2 already handles the Bedrock-specific transport details), then the binary `eventstream.Decoder`-driven loop (reusable `payloadBuf`, `errors.Is(err, io.EOF)` loop-exit, `bedrock.NewStreamDecoder()`), then `p.finishStreamedResponse(...)`.
- [ ] Update `internal/streaming/types.go`'s `ErrStreamingNotSupported` doc comment (already stale — Gemini/openaicompat both ship real streaming) and confirm whether it should still mention Bedrock at all once this ships (it shouldn't — Bedrock now streams too, via its own path).
- [ ] `cd gateway && go build ./...` — confirm it compiles.

### Task 4: `responseUnmarshalers`/dataplane-level tests

**Files:**
- Modify: `gateway/internal/gateway/dataplane/streaming_test.go` or a new small dedicated file, matching this codebase's own "small file per new concern" convention.

**Steps:**
- [ ] A real end-to-end-shaped test (using a fake `UpstreamStreamCaller` returning a real, hand-built binary event-stream body — construct it via `eventstream.Encoder`, the real encode-side counterpart, so the fixture is genuinely wire-accurate, not hand-rolled bytes) proving `streamDeploymentBedrock` correctly decodes a full messageStart→contentBlockStart→contentBlockDelta×N→contentBlockStop→messageStop→metadata sequence into the right accumulated `ChatResponse`.
- [ ] A test proving an `:message-type`=`"exception"` frame surfaces as a real Go error from `streamDeploymentBedrock`, not a silently-empty response.
- [ ] `cd gateway && go test ./internal/gateway/dataplane/... -v`.

---

## Phase 3: Real end-to-end HTTP integration test

### Task 1: Mock Bedrock streaming upstream

**Files:**
- Modify: `gateway/cmd/gateway/integration_test.go`

**Steps:**
- [ ] `newMockBedrockStreamingUpstream` — asserts the incoming request's URL path ends in `/converse-stream` and `Accept: application/vnd.amazon.eventstream` is present (the real, load-bearing proof the URL/header wiring from Phase 2 Task 2 reached the real HTTP call); responds with a real, `eventstream.Encoder`-constructed binary body (`Content-Type: application/vnd.amazon.eventstream`).
- [ ] `TestIntegrationStreamingRequestSucceedsBedrock` — drives a real streaming request through the full HTTP pipeline, asserting the accumulated SSE body the *client* receives (Kelvran's own client-facing wire format is always SSE, regardless of the upstream's transport — confirm this is still true by reading `streaming.Writer`'s real behavior before asserting it) contains the expected content deltas and a real `finish_reason`.
- [ ] `cd gateway && go test ./cmd/gateway/... -v -run Bedrock`.

---

## Phase 4: Docs, verify, ship

### Task 1: Documentation

**Files:**
- Modify: `gateway/ARCHITECTURE.md`, `gateway/internal/streaming/types.go`, `gateway/changelog/unreleased.md`, `DECISIONS.md`, `docs/agents/LOGS.md`, `STATUS.md`

**Steps:**
- [ ] Update `gateway/ARCHITECTURE.md`'s adapter Package Layout line: Bedrock now streams too, via its own binary path — name the real architectural asymmetry (a dispatch inside `streamDeployment`, not a `StreamingAdapter` implementation) explicitly, not glossed over.
- [ ] Fix `internal/streaming/types.go`'s stale `ErrStreamingNotSupported` doc comment (already inaccurate before this pass — Gemini/openaicompat both ship real streaming; after this pass, Bedrock does too, just via a different mechanism).
- [ ] Changelog + `DECISIONS.md` + `docs/agents/LOGS.md` + `STATUS.md`, per this project's established convention — naming the real, load-bearing corrections found (tool-use fragment-vs-whole-object, the path-segment vs. colon-suffix URL derivation) explicitly.

### Task 2: Full verification and ship

**Steps:**
- [ ] `cd gateway && go build ./... && go test ./... && go vet ./... -race && golangci-lint run ./...` — clean, zero regressions to any other adapter/dataplane/streaming test.
- [ ] Root `make verify` (same pre-existing, unrelated rootless-Docker caveat as every prior pass this session).
- [ ] `git add` the exact touched files; commit with a `feat(gateway):` conventional-commit message.
- [ ] Push; watch real CI to green.
- [ ] Final `STATUS.md` commit confirming the exact commit SHA and CI run ID.

## Scope Gate

Architecturally-scoped work (a genuinely parallel binary decode path, a real refactor extracting shared logic, real dataplane-level dispatch changes) — correctly warranting this plan + `docs/rfcs/2026-09-04-bedrock-converse-stream.md`.
