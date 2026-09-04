> **For agentic executors:** work through this task-by-task, checking off each step as it's done. Don't skip ahead — a later task may depend on an earlier one's actual output, not just its description.

---

**Goal:** Implement `gateway/internal/adapter/gemini` for real (non-streaming + streaming), closing the last stub adapter `PRD.md`'s v1 scope line names explicitly. A genuine-translation adapter (like `anthropic`, not a near-identity copy like `openai`/`openaicompat`), plus one real cross-cutting `dataplane.go` change no prior adapter needed.

**Architecture:** `gemini.go` (types + `ToProvider`/`FromProvider`), `stream.go` (`streamDecoder`/`Decode`, reusing `gemini.go`'s own `Response` type — Gemini's streaming and buffered responses share one schema), test files + `testdata/`. Two changes outside the adapter package: a `"gemini"` entry in `dataplane.go`'s `responseUnmarshalers` map + `setUpstreamAuthHeaders`, and a new `streamUpstreamURL` function called from `NewHTTPUpstreamStreamCaller` (the real architectural gap this RFC's Motivation section names).

**Tech Stack:** No new dependency — pure Go stdlib (`net/url` for the streaming-URL derivation), matching every other adapter's own shape.

**Spec:** `docs/rfcs/2026-09-04-gemini-adapter.md`.

**Global Constraints:**
- Types are self-contained in the `gemini` package, never shared with `openai`/`anthropic`/`openaicompat` — matches this codebase's existing "every adapter package is self-contained" convention.
- `FunctionResponse.name` is always resolved via a real `ToolCall.ID`→`Name` lookup built from message history — never left empty, never guessed.
- `FromProvider` returns a typed error (not a fake successful `Choice`) for `finishReason` values indicating the model's own tool-call machinery broke (`MALFORMED_FUNCTION_CALL`, `UNEXPECTED_TOOL_CALL`, `TOO_MANY_TOOL_CALLS`, `MISSING_THOUGHT_SIGNATURE`, `MALFORMED_RESPONSE`).
- Zero changes to `cmd/gateway/main.go`'s registry or `streaming.go`'s dispatch logic — both are already fully generic, confirmed by direct read (only `dataplane.go` itself needs the two additions above).
- Multi-modal parts, `thoughtSignature`, code execution, grounding/search tools, safety ratings, `candidateCount > 1`, and Vertex AI OAuth2 are explicitly out of scope — see the RFC's Alternatives Considered.
- Bedrock's stub is untouched — not silently bundled with this pass.

---

## Phase 1: Non-streaming adapter

### Task 1: `gemini.go` — real types + `ToProvider`/`FromProvider`

**Files:**
- Modify: `gateway/internal/adapter/gemini/gemini.go` (replace the stub body in place)
- Test: `gateway/internal/adapter/gemini/gemini_test.go`
- Create: `gateway/internal/adapter/gemini/testdata/request_canonical.json`, `request_gemini_native.golden.json`, `response_gemini_native.json`, `response_canonical.golden.json`

**Steps:**
- [ ] Define native types confirmed against the real discovery schema: `Content{Role, Parts}`, `Part{Text, FunctionCall, FunctionResponse}` (a union type — only one field populated per part, mirroring Anthropic's `ContentBlock` convention), `FunctionCall{ID, Name, Args map[string]any}`, `FunctionResponse{ID, Name, Response map[string]any}`, `Tool{FunctionDeclarations []FunctionDeclaration}`, `FunctionDeclaration{Name, Description, Parameters map[string]any}`, `GenerationConfig{Temperature *float64, MaxOutputTokens *int}`, `Request{Contents []Content, SystemInstruction *Content, GenerationConfig *GenerationConfig, Tools []Tool}`, `Candidate{Content Content, FinishReason string}`, `UsageMetadata{PromptTokenCount, CandidatesTokenCount, TotalTokenCount int}`, `Response{Candidates []Candidate, UsageMetadata UsageMetadata}`.
- [ ] `finishReasonFromGemini(finishReason string, hasFunctionCall bool) (string, error)`: implements the RFC's exact mapping table (`STOP`+functionCall→`"tool_calls"`; `STOP`→`"stop"`; `MAX_TOKENS`→`"length"`; `SAFETY`/`PROHIBITED_CONTENT`/`SPII`/`BLOCKLIST`→`"content_filter"`; the 5 malformed-tool-call values → a real `error`; everything else → `"stop"` pass-through-by-default).
- [ ] `ToProvider(req adapter.ChatRequest) (any, error)`:
  - Build the `ToolCall.ID`→`Name` lookup map by scanning every message's `ToolCalls` in order, before the main conversion loop.
  - Hoist `role:"system"` messages into `SystemInstruction`, joined with `"\n\n"` (matching `anthropic.go`'s exact convention).
  - `role:"user"/"assistant"` → `Content{Role: "user"/"model", Parts: [...]}`; text becomes `{Text: ...}`; each `ToolCall` becomes `{FunctionCall: {ID, Name, Args}}` with `ArgumentsJSON` parsed via `json.Unmarshal` into `map[string]any` (mirroring `anthropic.go` lines 139-149 exactly).
  - `role:"tool"` → `Content{Role: "user", Parts: [{FunctionResponse: {ID: m.ToolCallID, Name: <looked up>, Response: {"result": m.Content}}}]}`. If `m.ToolCallID` has no match in the lookup map, return a typed error (`fmt.Errorf("gemini: tool message references unknown tool_call_id %q", m.ToolCallID)`) — never send Gemini a `functionResponse` with an empty/guessed name.
  - `Tools`/`ToolDef.ParametersJSON` → `FunctionDeclaration.Parameters` (parsed `map[string]any`, same pattern as Anthropic's `InputSchema`).
  - `Temperature`/`MaxTokens` → `GenerationConfig.Temperature`/`MaxOutputTokens`.
- [ ] `FromProvider(resp any) (adapter.ChatResponse, error)`:
  - Type-assert `*Response`.
  - For the first candidate (index 0 — `candidateCount > 1` is out of scope, per the RFC): scan `Content.Parts` for text (concatenate) and `functionCall` parts (each becomes a canonical `ToolCall`, `Args` re-marshaled via `json.Marshal` back into `ArgumentsJSON`, mirroring `anthropic.go`'s `FromProvider` exactly).
  - Call `finishReasonFromGemini` with the candidate's `FinishReason` and whether any `functionCall` part was found; propagate its error immediately if non-nil.
  - Map `UsageMetadata` fields 1:1 into `adapter.Usage`.
- [ ] `TestRoundTrip`, `TestName` (expects `"gemini"`), `TestToProviderInvalidToolArguments`, `TestToProviderToolMessageWithUnknownToolCallIDFails` (the real, named hazard this RFC's grounding found), `TestFromProviderMalformedFunctionCallReturnsError` (one of the 5 real error-worthy finish reasons), `TestFromProviderStopWithFunctionCallMapsToToolCalls` (proves the finish-reason-needs-part-scanning hazard is actually handled, not just described).
- [ ] `regression_test.go` + 4 testdata JSON fixtures, mirroring `anthropic/regression_test.go`'s convention (a system message + tool call + multi-turn history on the request side; a text response with real `usageMetadata` on the response side) — Gemini-flavored (a real model name like `gemini-2.5-flash`, a real-shaped `functionCall`/`functionResponse` pair).
- [ ] `cd gateway && go test ./internal/adapter/gemini/... -v`.

### Task 2: `dataplane.go` wiring — response unmarshaler + auth header

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go` (`responseUnmarshalers` map, `setUpstreamAuthHeaders`, import)

**Steps:**
- [ ] Add the `"gemini"` entry to `responseUnmarshalers`, per the RFC's exact snippet.
- [ ] Add `case "gemini": httpReq.Header.Set("x-goog-api-key", dep.APIKey)` to `setUpstreamAuthHeaders`.
- [ ] Add the `github.com/kelvran/gateway/gateway/internal/adapter/gemini` import.
- [ ] `cd gateway && go build ./...` — confirm it compiles.

---

## Phase 2: Streaming adapter + the real URL-derivation gap

### Task 1: `stream.go`

**Files:**
- Create: `gateway/internal/adapter/gemini/stream.go`
- Test: `gateway/internal/adapter/gemini/stream_test.go`
- Create: `gateway/internal/adapter/gemini/testdata/stream_text.txt`, `stream_tool_call.txt`, `stream_usage.txt`

**Steps:**
- [ ] `streamDecoder` (no cross-call state needed beyond nothing — Gemini's SSE frames are each a complete, independent `Response`, unlike Anthropic's typed event sequence; confirm this is genuinely simpler than Anthropic's decoder, not accidentally under-implemented).
- [ ] `NewStreamDecoder` implementing `streaming.StreamingAdapter`.
- [ ] `Decode(raw streaming.SSEEvent) ([]streaming.ChatCompletionChunk, bool, *adapter.Usage, error)`: `json.Unmarshal(raw.Data)` into `Response` (the same type `gemini.go` defines — no separate stream-chunk type exists, confirmed against the real discovery schema); reshape the first candidate's parts into a `streaming.MessageDelta` (text → `Content`; `functionCall` parts → `ToolCallDelta`, one non-accumulated fragment per call since Gemini's `args` arrives as a complete object per chunk, not an incrementally-assembled string like OpenAI's — a real, named divergence from OpenAI/Anthropic's fragment-accumulation contract, document this explicitly in the decoder's doc comment); if `UsageMetadata` is non-zero, set `finalUsage`. **Always return `done=false`** — no terminal sentinel exists; the dataplane's existing `io.EOF`-driven loop (confirmed by direct read, not a new mechanism) ends the stream.
- [ ] 3 raw SSE `.txt` fixtures: text-only completion, a `functionCall`-bearing completion, a final chunk carrying `usageMetadata`.
- [ ] `stream_test.go`: `TestNewStreamDecoderSatisfiesStreamingAdapter`, `TestDecodeTextOnlyCompletion`, `TestDecodeFunctionCallChunk` (proves the whole-object-per-chunk divergence from OpenAI's fragment-accumulation contract is handled correctly, not silently mishandled), `TestDecodeFinalUsageChunk`, `TestDecodeNeverSignalsDone` (a real, load-bearing proof — assert `done == false` even on a chunk carrying a terminal `finishReason`).
- [ ] `cd gateway && go test ./internal/adapter/gemini/... -v` — full package green.

### Task 2: `streamUpstreamURL` — the real architectural fix

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go` (new function + one call-site change in `NewHTTPUpstreamStreamCaller`)
- Test: `gateway/internal/gateway/dataplane/dataplane_test.go` (or the nearest existing unit-test file for this package — confirm the real filename before editing)

**Steps:**
- [ ] Add `streamUpstreamURL(dep Deployment) (string, error)`, per the RFC's exact snippet (`net/url`-based `:generateContent`→`:streamGenerateContent` suffix swap + `alt=sse` query param; every non-`"gemini"` provider returns `dep.BaseURL` unchanged).
- [ ] In `NewHTTPUpstreamStreamCaller`, replace the direct `dep.BaseURL` reference in `http.NewRequestWithContext` with a call to `streamUpstreamURL(dep)`, propagating its error.
- [ ] Unit tests for `streamUpstreamURL` directly: a `gemini` deployment's `base_url` ending in `:generateContent` correctly becomes `:streamGenerateContent?alt=sse`; a `base_url` already carrying a query string still gets `alt=sse` appended correctly (not a broken double-`?`); a `base_url` NOT ending in `:generateContent` returns a real error; every other provider's `base_url` passes through completely unchanged (the decisive backward-compatibility proof — mirrors the router RFC's own "degrades to identical behavior" precedent).
- [ ] `cd gateway && go test ./internal/gateway/dataplane/... -v` — zero regressions to any existing streaming test for `openai`/`anthropic`/`openaicompat`.

### Task 3: Real end-to-end HTTP integration test

**Files:**
- Modify: `gateway/cmd/gateway/integration_test.go`

**Steps:**
- [ ] `newMockGeminiUpstream` — decode into `gemini.Request`, respond with a real-shaped `gemini.Response` including `usageMetadata`.
- [ ] `newMockGeminiStreamingUpstream` — asserts the incoming request URL path/query actually carries `:streamGenerateContent`+`alt=sse` (the real, load-bearing proof that `streamUpstreamURL` is correctly wired end-to-end, not just unit-tested in isolation), responds with real Gemini-shaped SSE frames.
- [ ] `newIntegrationServerGemini`, modeled on `newIntegrationServerAnthropic`/`newIntegrationServerOpenAICompat`.
- [ ] `TestIntegrationGeminiRequestSucceeds` (non-streaming) and `TestIntegrationStreamingRequestSucceedsGemini`, plus `TestIntegrationGeminiToolCallRoundTrip` (a real multi-turn exchange: assistant tool call → client tool result → second request — the one path this adapter's own `FunctionResponse.name`-resolution hazard could silently break if implemented wrong).
- [ ] `cd gateway && go test ./cmd/gateway/... -v -run Gemini`.

---

## Phase 3: Docs, verify, ship

### Task 1: Documentation

**Files:**
- Modify: `gateway/ARCHITECTURE.md` (Canonical Schema & Provider Adapters section; Package Layout's adapter line), `gateway/internal/streaming/types.go` (comment listing which adapters satisfy `StreamingAdapter`), `gateway/config.example.yaml` (a real, correctly-shaped example `gemini` deployment entry — `base_url` ending in `:generateContent`), `gateway/changelog/unreleased.md`, `DECISIONS.md`, `docs/agents/LOGS.md`, `STATUS.md`

**Steps:**
- [ ] Update `gateway/ARCHITECTURE.md`/`streaming/types.go` comments to name `gemini` as real, leaving Bedrock as the one remaining (and deliberately un-addressed) stub.
- [ ] Add a real `gemini` deployment example to `config.example.yaml`, matching the RFC's documented `base_url` convention (ends in `:generateContent`, never `:streamGenerateContent` — that's derived).
- [ ] Changelog + `DECISIONS.md` + `docs/agents/LOGS.md` + `STATUS.md`, per this project's established convention — explicitly naming the `streamUpstreamURL` architectural addition and the `id`-field independent-verification correction as the two real findings this pass's own grounding didn't originally surface.

### Task 2: Full verification and ship

**Steps:**
- [ ] `cd gateway && go build ./... && go test ./... && go vet ./... -race && golangci-lint run ./...` — clean, zero regressions to any other adapter/dataplane/streaming test.
- [ ] Root `make verify` (same pre-existing, unrelated rootless-Docker caveat as every prior pass this session).
- [ ] `git add` the exact touched files; commit with a `feat(gateway):` conventional-commit message.
- [ ] Push; watch real CI to green.
- [ ] Final `STATUS.md` commit confirming the exact commit SHA and CI run ID.

## Scope Gate

Architecturally-scoped work (a new real adapter closing a named `PRD.md` v1-scope gap, plus a genuine cross-cutting `dataplane.go` change no prior adapter needed) — correctly warranting this plan + `docs/rfcs/2026-09-04-gemini-adapter.md`, matching every other adapter's own precedent.
