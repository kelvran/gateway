> **For agentic executors:** work through this task-by-task, checking off each step as it's done. Don't skip ahead — a later task may depend on an earlier one's actual output, not just its description.

---

**Goal:** Implement `gateway/internal/adapter/openaicompat` for real (non-streaming + streaming), as a near-verbatim copy of the working `openai` adapter, closing the one required `dataplane.go` wiring gap.

**Architecture:** Same shape as `internal/adapter/openai`: `openaicompat.go` (types + `ToProvider`/`FromProvider`), `stream.go` (`streamDecoder`/`Decode`), 3 test files + `testdata/`. One addition outside the adapter package: a `"openaicompat"` entry in `dataplane.go`'s `responseUnmarshalers` map.

**Tech Stack:** No new dependency — pure Go stdlib, matching `openai`'s own shape exactly.

**Spec:** `docs/rfcs/2026-09-04-openaicompat-adapter.md`.

**Global Constraints:**
- Types are duplicated from `openai`, never imported/shared — matches this codebase's existing "every adapter package is self-contained" convention.
- Zero changes to `cmd/gateway/main.go`'s registry or `dataplane.go`/`streaming.go`'s dispatch logic beyond the one `responseUnmarshalers` entry — both are already fully generic, confirmed by direct read.
- `FinishReason` stays a bare `string`/`*string` — no closed-enum validation.

---

## Phase 1: Non-streaming adapter

### Task 1: `openaicompat.go` — real types + `ToProvider`/`FromProvider`

**Files:**
- Modify: `gateway/internal/adapter/openaicompat/openaicompat.go` (replace the stub body in place)
- Test: `gateway/internal/adapter/openaicompat/openaicompat_test.go`
- Create: `gateway/internal/adapter/openaicompat/testdata/request_canonical.json`, `request_openaicompat_native.golden.json`, `response_openaicompat_native.json`, `response_canonical.golden.json`

**Steps:**
- [x] Copy `openai.go`'s `Request`/`StreamOptions`/`Message`/`ToolCall`/`FunctionCall`/`Tool`/`FunctionDef`/`Response`/`Choice`/`Usage` types verbatim into `openaicompat.go`, adjusting only the package name and doc comments (openaicompat-specific framing, plus the `finish_reason`-is-open-ended warning on `Choice.FinishReason` per the RFC).
- [x] Copy `ToProvider`/`FromProvider`/`toolCallsToProvider`/`toolCallsFromProvider` verbatim, same logic, same error message prefixes swapped to `"openaicompat: ..."`.
- [x] `TestRoundTrip`, `TestName`, `TestToProviderInvalidToolArguments` in `openaicompat_test.go`, mirroring `openai_test.go` exactly (adjust `Name()` expectation to `"openaicompat"`).
- [x] `regression_test.go` + 4 testdata JSON fixtures, mirroring `openai/regression_test.go` and its 4 fixtures exactly (content can be openaicompat-flavored — different IDs/model names — but must exercise the same shape: system message, tool call, multi-turn history for the request side; a plain text response for the response side).
- [x] `cd gateway && go test ./internal/adapter/openaicompat/... -run 'TestRoundTrip|TestName|TestToProviderInvalidToolArguments|TestRegression' -v`.

### Task 2: `dataplane.go` wiring

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go` (`responseUnmarshalers` map + import)

**Steps:**
- [x] Add the `"openaicompat"` entry to `responseUnmarshalers`, per the RFC's exact snippet.
- [x] Add the `github.com/kelvran/gateway/gateway/internal/adapter/openaicompat` import.
- [x] `cd gateway && go build ./...` — confirm it compiles.

---

## Phase 2: Streaming adapter

### Task 1: `stream.go`

**Files:**
- Create: `gateway/internal/adapter/openaicompat/stream.go`
- Test: `gateway/internal/adapter/openaicompat/stream_test.go`
- Create: `gateway/internal/adapter/openaicompat/testdata/stream_text.txt`, `stream_tool_call.txt`, `stream_usage.txt`, `stream_done_only.txt`

**Steps:**
- [x] Copy `stream.go`'s `nativeStreamChunk`/`nativeStreamChoice`/`nativeStreamDelta`/`nativeToolCallDelta`/`nativeFunctionDelta`, `streamDecoder`, `NewStreamDecoder`, `Decode`, `toCanonicalDelta` verbatim, adjusting package name, doc comments, and error message prefixes.
- [x] 4 raw SSE `.txt` fixtures mirroring `openai/testdata/`'s exactly (text-only, tool-call-argument-accumulation, final-usage-chunk, done-only) — content can be openaicompat-flavored.
- [x] `stream_test.go` mirroring `openai/stream_test.go` exactly: `TestNewStreamDecoderSatisfiesStreamingAdapter`, `decodeFixture` helper, `TestDecodeTextOnlyCompletion`, `TestDecodeToolCallAccumulatesArgumentFragments`, `TestDecodeFinalUsageChunk`, `TestDecodeDoneSentinel`, `TestDecodeAfterDoneReturnsError`.
- [x] `cd gateway && go test ./internal/adapter/openaicompat/... -v` — full package green.

### Task 2: Real end-to-end HTTP integration test

**Files:**
- Modify: `gateway/cmd/gateway/integration_test.go`

**Steps:**
- [x] `newMockOpenAICompatUpstream`/`newMockOpenAICompatStreamingUpstream`, modeled byte-for-byte on `newMockUpstream`/`newMockStreamingUpstream` (openaicompat's wire shape is OpenAI's — decode into `openaicompat.Request`, respond with `openaicompat.Response`/SSE frames).
- [x] `newIntegrationServerOpenAICompat`, modeled on `newIntegrationServerAnthropic` (the "second provider" builder precedent, not the default `newIntegrationServer`).
- [x] `TestIntegrationOpenAICompatRequestSucceeds` (non-streaming) and `TestIntegrationStreamingRequestSucceedsOpenAICompat`, modeled on the equivalent OpenAI/Anthropic tests.
- [x] `cd gateway && go test ./cmd/gateway/... -v -run OpenAICompat`.

---

## Phase 3: Docs, verify, ship

### Task 1: Documentation

**Files:**
- Modify: `gateway/ARCHITECTURE.md` (Canonical Schema & Provider Adapters section; Package Layout's adapter line), `gateway/internal/streaming/types.go` (comment listing which adapters satisfy `StreamingAdapter`), `gateway/changelog/unreleased.md`, `DECISIONS.md`, `docs/agents/LOGS.md`, `STATUS.md`

**Steps:**
- [x] Update `gateway/ARCHITECTURE.md`/`streaming/types.go` comments to name `openaicompat` as real, leaving Gemini/Bedrock as the still-stubbed set.
- [x] Changelog + `DECISIONS.md` + `docs/agents/LOGS.md` + `STATUS.md`, per this project's established convention.

### Task 2: Full verification and ship

**Steps:**
- [x] `cd gateway && go build ./... && go test ./... && go vet ./... -race && golangci-lint run ./...` — clean, zero regressions to any other adapter/dataplane test.
- [x] Root `make verify` (same pre-existing, unrelated rootless-Docker caveat as every prior pass this session).
- [x] `git add` the exact touched files; commit with a `feat(gateway):` conventional-commit message.
- [x] Push; watch real CI to green.
- [x] Final `STATUS.md` commit confirming the exact commit SHA and CI run ID.

## Scope Gate

Architecturally-scoped work (a new real adapter, a new required dataplane wiring entry) — correctly warranting this plan + `docs/rfcs/2026-09-04-openaicompat-adapter.md`, matching every other adapter's own precedent (`docs/rfcs/2026-09-02-streaming-support.md`).
