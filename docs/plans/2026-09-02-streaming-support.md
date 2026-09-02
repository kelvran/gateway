> **For agentic executors:** work through this task-by-task. Task 1 is foundational and must land before Tasks 2/3 (they implement against its interface). Tasks 2 and 3 are independent of each other and can run in parallel. Task 4 depends on both 2 and 3.

---

**Goal:** Real SSE streaming for the gateway's two real adapters (OpenAI, Anthropic), wired into the dataplane pipeline with cache-tee and cost accounting.

**Architecture:** A new `gateway/internal/streaming/` package (transport-level SSE reader/writer, provider-agnostic) plus a `StreamDecoder` interface implemented per-adapter (stateful, one instance per request) plus dataplane wiring for the streaming request path.

**Tech Stack:** Stdlib only (`bufio`, `net/http`'s `http.Flusher`) — no new dependency, consistent with the rest of `gateway/`.

**Spec:** `docs/rfcs/2026-09-02-streaming-support.md` — the exact `ChatCompletionChunk`/`StreamDecoder`/`StreamingAdapter` type/interface definitions live there; this plan implements them verbatim, it does not redesign them.

**Global Constraints** (inherited from the spec + `AGENTS.md`):
- Gemini/Bedrock/openaicompat adapters are NOT touched — they remain fully stubbed, and streaming a request routed to one of them must return a clear typed error, never silently buffer or crash.
- Every new decoder must be tested against every documented SSE event type for its provider, not just the happy path.
- Fallback/retry on upstream failure applies only before the first chunk reaches the client — no exceptions, no attempted mid-stream retry.
- `docs/testing/TESTING.md`'s ban on hitting real upstream provider APIs in CI applies here too — all streaming tests use a mock SSE server.

---

## Task 1 — Shared types, `StreamDecoder` interface, SSE transport (foundational)

**Files:**
- Create: `gateway/internal/streaming/types.go` (`ChatCompletionChunk`, `ChunkChoice`, `MessageDelta`, `ToolCallDelta`, `SSEEvent`, `StreamDecoder`, `StreamingAdapter` — verbatim from the RFC)
- Create: `gateway/internal/streaming/reader.go` (`ReadSSE(io.Reader) (<-chan SSEEvent, <-chan error)` or an iterator-style reader — implementer's choice of exact API shape, but it must split on blank-line-delimited frames and parse `event:`/`data:` prefixes per the SSE spec)
- Create: `gateway/internal/streaming/writer.go` (`WriteChunk(http.ResponseWriter, http.Flusher, ChatCompletionChunk) error`, `WriteDone(http.ResponseWriter, http.Flusher) error`)
- Create: `gateway/internal/streaming/reader_test.go`, `writer_test.go`

**Steps:**
- [ ] Define the types/interfaces exactly as specified in the RFC.
- [ ] Implement the SSE reader: handles multi-line `data:` fields (per spec, concatenated with `\n`), ignores comment lines (`:`-prefixed), stops cleanly on EOF.
- [ ] Implement the SSE writer: JSON-encodes a chunk, writes `data: <json>\n\n`, flushes immediately; `WriteDone` writes `data: [DONE]\n\n` and flushes.
- [ ] Unit tests: reader correctly parses a multi-event, multi-line-data SSE stream (use a real captured-shape fixture, not synthetic minimal input); writer produces spec-correct SSE framing and calls Flush exactly once per write.

**Verify:** `cd gateway && go build ./internal/streaming/... && go test ./internal/streaming/...`

## Task 2 — OpenAI `StreamDecoder`

**Files:**
- Create: `gateway/internal/adapter/openai/stream.go` (`NewStreamDecoder() streaming.StreamDecoder`, satisfying `streaming.StreamingAdapter` on the existing `openai.Adapter` type)
- Create: `gateway/internal/adapter/openai/stream_test.go`
- Create: `gateway/internal/adapter/openai/testdata/stream_*.txt` (real captured-shape OpenAI SSE fixtures: a text-only completion, a tool-call completion split across multiple chunks, a final chunk carrying `usage` per `stream_options.include_usage`, and the `[DONE]` sentinel)

**Steps:**
- [ ] OpenAI's stream is close to homogeneous `chat.completion.chunk` objects already — the decoder is largely a direct passthrough/reshape into the canonical chunk type, but must correctly accumulate multi-chunk tool-call argument fragments (OpenAI splits a single tool call's `arguments` string across several chunks, keyed by `index`).
- [ ] Detect the `[DONE]` sentinel (a literal `data: [DONE]` line, not JSON) and return `done=true`.
- [ ] Extract `usage` from the final chunk when present.
- [ ] Tests: feed each fixture file through `ReadSSE` + the decoder, assert the resulting canonical chunks match expected deltas exactly (including correct tool-call-argument accumulation across chunks), assert `done`/`finalUsage` fire at the right point.

**Verify:** `cd gateway && go test ./internal/adapter/openai/...`

## Task 3 — Anthropic `StreamDecoder`

**Files:**
- Create: `gateway/internal/adapter/anthropic/stream.go`
- Create: `gateway/internal/adapter/anthropic/stream_test.go`
- Create: `gateway/internal/adapter/anthropic/testdata/stream_*.txt` (real captured-shape Anthropic SSE fixtures covering: `message_start`, `content_block_start`/`content_block_delta` with `text_delta`, `content_block_start`/`content_block_delta` with `input_json_delta` for a tool call, `content_block_stop`, `message_delta` carrying `usage`, `message_stop`)

**Steps:**
- [ ] Implement the stateful event-sequence parser per `gateway/ARCHITECTURE.md`'s already-documented hazard: track which content-block index is currently open and whether it's a text block or a tool-call block, so `input_json_delta` fragments accumulate against the correct tool-call index in the canonical `ToolCallDelta`.
- [ ] Map Anthropic's typed events to canonical chunks: `content_block_start`+`text` → a chunk with `Delta.Role` on the very first event of the stream, empty content otherwise; `content_block_delta`+`text_delta` → a chunk with `Delta.Content`; `content_block_start`+`tool_use` → a chunk with `Delta.ToolCalls[0].{ID,Name}`; `content_block_delta`+`input_json_delta` → a chunk with `Delta.ToolCalls[0].ArgumentsJSON` (the fragment); `message_delta`'s `usage` → `finalUsage`; `message_stop` → `done=true`.
- [ ] Tests: same shape as Task 2 — feed real fixtures through the full reader+decoder pipeline, assert exact canonical output at every step, including the stateful block-index tracking across a multi-tool-call response.

**Verify:** `cd gateway && go test ./internal/adapter/anthropic/...`

## Task 4 — Dataplane Integration (depends on Tasks 1-3)

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go` (add the streaming branch to the request-handling pipeline)
- Modify: `gateway/cmd/gateway/main.go` if the handler signature needs `http.Flusher` access (it should already have it via the standard `http.ResponseWriter`, which implements `http.Flusher` for the stdlib server — confirm, don't assume)
- Create: `gateway/internal/gateway/dataplane/streaming_test.go` (unit-level)
- Modify: `gateway/cmd/gateway/integration_test.go` (add real end-to-end streaming scenarios to the existing httptest-based integration suite)

**Steps:**
- [ ] Cache-hit + `stream:true` path: synthesize a single-delta-chunk + finish-chunk + `[DONE]` stream from the cached response.
- [ ] Cache-miss + `stream:true` path: type-assert the resolved adapter to `streaming.StreamingAdapter`; if it fails, return a typed `ErrStreamingNotSupported` naming the provider. Otherwise wire: upstream call (provider's native streaming flag set) → `streaming.ReadSSE` → decoder's `Decode` per event → `streaming.WriteChunk` per canonical chunk (flushed) AND accumulate into a `ChatResponse` builder for cache write-back + cost accounting + structured logging, in the same pass.
- [ ] Fallback rule: track a `firstChunkSent bool`; if the upstream call/first-event fails before it flips true, fall back to the next deployment exactly like the existing non-streaming fallback path; once true, no fallback — end the stream, log the error.
- [ ] Cost accounting: compute from `finalUsage` when present; log a clear warning + record zero/estimated cost when a provider stream ends without ever sending usage.
- [ ] New integration tests (added to the existing `cmd/gateway/integration_test.go` suite, using a mock SSE upstream server): a full streaming request against each of OpenAI and Anthropic mock upstreams, a streaming cache-hit (second identical streaming request served from cache, upstream call count stays at 1), and a streaming request to an unconfigured/non-streaming-capable provider returning the typed error.

**Verify:** `cd gateway && go build ./... && go vet ./... && go test ./... -v && golangci-lint run ./...`

## Task 5 — Docs, Changelog, Wrap-Up

**Files:**
- Modify: `gateway/ARCHITECTURE.md` (remove the "non-streaming only" caveat in the Request Lifecycle section; document the new `gateway/internal/streaming/` package)
- Modify: `PRD.md` (remove streaming from the v1 "explicitly out of scope" list if it's there — confirm current wording first)
- Modify: `gateway/changelog/unreleased.md` (Added entry)
- Modify: `DECISIONS.md` (one line, if any tooling/design choice in this pass is decision-worthy beyond what the RFC already covers)
- Modify: `docs/agents/LOGS.md` (new append-only entry)
- Modify: `STATUS.md` (Current Phase, Verification State, Next Action)

**Verify:** re-run the full Task 4 verify command once more after doc edits, to confirm nothing broke; cross-reference grep for the doc edits.
