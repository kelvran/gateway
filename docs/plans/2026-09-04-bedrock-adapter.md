> **For agentic executors:** work through this task-by-task, checking off each step as it's done. Don't skip ahead — a later task may depend on an earlier one's actual output, not just its description.

---

**Goal:** Implement `gateway/internal/adapter/bedrock` for real (buffered/non-streaming only), targeting Amazon Bedrock Runtime's Converse API — a deliberate scope expansion beyond `PRD.md`'s v1 line, explicitly requested by the user. The first adapter requiring a genuine `Deployment`/config-schema change (SigV4 credentials, not a bearer token).

**Architecture:** `bedrock.go` (types + `ToProvider`/`FromProvider`), no `stream.go` this pass (streaming explicitly deferred). Three changes outside the adapter package: (1) `controlplane.DeploymentConfig` gains `AccessKeyIDEnv`/`SecretAccessKeyEnv`/`SessionTokenEnv`/`Region`, with provider-conditional validation in `Load()`; (2) `dataplane.Deployment` gains matching resolved fields, populated in `cmd/gateway/main.go`'s `buildPipeline`; (3) `setUpstreamAuthHeaders` gains a `body []byte` param and now returns `error`, with a new `case "bedrock"` performing real SigV4 signing via `aws-sdk-go-v2/aws/signer/v4`.

**Tech Stack:** New dependency — `github.com/aws/aws-sdk-go-v2/aws/signer/v4` (+ its `aws` core package for `aws.Credentials`), the first AWS SDK dependency this project has taken on. Deliberately the small, focused signer package, not the full service-client SDK.

**Spec:** `docs/rfcs/2026-09-04-bedrock-adapter.md`.

**Global Constraints:**
- SigV4 service-signing name is `"amazonbedrockfrontendservice"` — confirmed directly against real `aws-sdk-go-v2` source, **not** `"bedrock"`. Get this exactly right; a wrong value fails signing with no local test to catch it (no live AWS account in this environment).
- No streaming this pass — `bedrock.Adapter` does not implement `streaming.StreamingAdapter`; a streaming request to it continues returning the existing `dataplane.ErrStreamingNotSupported`, unchanged.
- `access_key_id_env`/`secret_access_key_env`/`region` are required in config when (and only when) `provider == "bedrock"`; `api_key_env` becomes conditionally-required (still required for every other provider, no longer required for `bedrock`). `session_token_env` is always optional.
- `base_url` keeps its existing, universal meaning (the full literal upstream URL) — no new URL-derivation function needed, unlike Gemini's `streamUpstreamURL`.
- `ChatResponse.ID` is left empty for Bedrock responses (Converse has no native response-ID field) — an honest absence, never a fabricated placeholder.
- The 2 malformed-tool-use `stopReason` values surface as a typed error from `FromProvider`, never a fake successful `Choice`.

---

## Phase 1: Config schema + credential resolution

### Task 1: `controlplane.DeploymentConfig` + `Load()` validation

**Files:**
- Modify: `gateway/internal/gateway/controlplane/config.go`
- Test: `gateway/internal/gateway/controlplane/config_test.go`

**Steps:**
- [ ] Add `AccessKeyIDEnv string`, `SecretAccessKeyEnv string`, `SessionTokenEnv string`, `Region string` fields to `DeploymentConfig`, doc comments mirroring `APIKeyEnv`'s exact "name of the env var, never the raw value" phrasing (`Region` is the one exception — a plain, non-secret string, doc comment must say so explicitly).
- [ ] In `Load()`'s deployment-parsing loop: parse all four new fields via `getString`. Widen the existing required-fields check to be provider-conditional: `api_key_env` required unless `dep.Provider == "bedrock"`; when `dep.Provider == "bedrock"`, require `access_key_id_env`/`secret_access_key_env`/`region` instead (real, distinct error messages naming exactly which fields are missing for which provider case — don't collapse both cases into one vague message).
- [ ] Tests: a `bedrock` deployment missing `api_key_env` parses successfully (proving the conditional-requirement change); a `bedrock` deployment missing `access_key_id_env`/`secret_access_key_env`/`region` fails with a clear error naming the real missing field; a non-`bedrock` deployment missing `api_key_env` still fails exactly as before (a real regression-proof, not just "should still work").
- [ ] `cd gateway && go test ./internal/gateway/controlplane/... -v`.

### Task 2: `dataplane.Deployment` + `main.go`'s `buildPipeline` credential resolution

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go` (`Deployment` struct)
- Modify: `gateway/cmd/gateway/main.go` (`buildPipeline`)

**Steps:**
- [ ] Add `AccessKeyID string`, `SecretAccessKey string`, `SessionToken string`, `Region string` to `dataplane.Deployment`.
- [ ] In `buildPipeline`'s deployment loop: for `d.Provider == "bedrock"`, resolve `os.Getenv(d.AccessKeyIDEnv)`/`os.Getenv(d.SecretAccessKeyEnv)`/`os.Getenv(d.SessionTokenEnv)` (session token: empty string if `SessionTokenEnv` itself is empty — never call `os.Getenv("")`), with the same "warn if unset, don't fail startup" `logger.Warn` posture `APIKeyEnv`'s resolution already has. Populate `Region` directly from `d.Region` (not a secret, no env-var indirection).
- [ ] `cd gateway && go build ./...` — confirm it compiles.

---

## Phase 2: SigV4 signing + adapter implementation

### Task 1: `setUpstreamAuthHeaders` — real SigV4 signing

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go` (`setUpstreamAuthHeaders` signature + both call sites)
- Test: nearest existing dataplane auth-related test file (confirm the real filename before editing) or a new small dedicated test file, mirroring `streamurl_test.go`'s precedent of a small dedicated file for one new function.

**Steps:**
- [ ] `cd gateway && go get github.com/aws/aws-sdk-go-v2/aws/signer/v4` (pulls in the `aws` core package transitively).
- [ ] Widen `setUpstreamAuthHeaders(httpReq *http.Request, dep Deployment)` to `setUpstreamAuthHeaders(ctx context.Context, httpReq *http.Request, dep Deployment, body []byte) error`. Every existing `case`/`default` branch returns `nil` after setting its header (unchanged behavior, now wrapped in the new return type).
- [ ] New `case "bedrock":` per the RFC's exact snippet — `sha256` hex of `body`, `aws.Credentials{AccessKeyID, SecretAccessKey, SessionToken}` from `dep`, `v4.NewSigner().SignHTTP(ctx, creds, httpReq, payloadHash, "amazonbedrockfrontendservice", dep.Region, time.Now())`, wrapped error on failure.
- [ ] Update both call sites (`NewHTTPUpstreamCaller`/`NewHTTPUpstreamStreamCaller`) to pass `ctx`/`body` and propagate the new error return.
- [ ] Unit tests for the `"bedrock"` branch specifically: a real signed request has a non-empty `Authorization` header starting with `AWS4-HMAC-SHA256`; a real `X-Amz-Date` header is set; every non-`"bedrock"` provider's existing auth-header behavior is unchanged (the decisive backward-compatibility proof, mirroring `streamUpstreamURL`'s own precedent).
- [ ] `cd gateway && go test ./internal/gateway/dataplane/... -v` — zero regressions to any existing auth-header-dependent test.

### Task 2: `bedrock.go` — real types + `ToProvider`/`FromProvider`

**Files:**
- Modify: `gateway/internal/adapter/bedrock/bedrock.go` (replace the stub body in place)
- Test: `gateway/internal/adapter/bedrock/bedrock_test.go`
- Create: `gateway/internal/adapter/bedrock/testdata/request_canonical.json`, `request_bedrock_native.golden.json`, `response_bedrock_native.json`, `response_canonical.golden.json`

**Steps:**
- [ ] Define native types per the RFC's confirmed field mapping: `Message{Role, Content []ContentBlock}`, `ContentBlock{Text, ToolUse *ToolUse, ToolResult *ToolResult}` (union, mirroring gemini's `Part`), `ToolUse{ToolUseID, Name, Input map[string]any}`, `ToolResult{ToolUseID, Content []ToolResultContent, Status string}`, `ToolResultContent{Text string}`, `Tool{ToolSpec ToolSpec}`, `ToolSpec{Name, Description, InputSchema InputSchema}`, `InputSchema{JSON map[string]any}`, `ToolConfig{Tools []Tool}`, `InferenceConfig{Temperature *float64, MaxTokens *int}`, `SystemContentBlock{Text string}`, `Request{Messages []Message, System []SystemContentBlock, InferenceConfig *InferenceConfig, ToolConfig *ToolConfig}`, `Usage{InputTokens, OutputTokens, TotalTokens int}`, `Output{Message Message}`, `Response{Output Output, StopReason string, Usage Usage}`.
- [ ] `stopReasonFromBedrock(stopReason string) (string, error)`: the RFC's exact mapping table.
- [ ] `ToProvider`: hoist `role:"system"` into `System`, join `"\n\n"`; `role:"tool"` → `Message{Role:"user", Content:[{ToolResult:{ToolUseID: m.ToolCallID, Content:[{Text: m.Content}], Status:"success"}}]}` (no name-lookup needed — simpler than Gemini); assistant `ToolCalls` → `{ToolUse:{ToolUseID, Name, Input: <parsed ArgumentsJSON>}}`; `ToolDef` → `Tool{ToolSpec:{Name, Description, InputSchema:{JSON: <parsed ParametersJSON>}}}`; `Temperature`/`MaxTokens` → `InferenceConfig`.
- [ ] `FromProvider`: type-assert `*Response`; concatenate `Output.Message.Content`'s text blocks, collect `ToolUse` blocks into canonical `ToolCall`s (re-marshal `Input` to `ArgumentsJSON`); call `stopReasonFromBedrock`, propagate its error immediately; `ChatResponse.ID` left empty (named, not a bug); map `Usage` 1:1.
- [ ] `TestRoundTrip`, `TestName` (expects `"bedrock"`), `TestToProviderInvalidToolArguments`, `TestToProviderToolResultMessageNeedsNoNameLookup` (proves the real, named simplification vs. Gemini's hazard), `TestFromProviderMalformedToolUseReturnsError`, `TestFromProviderStopWithToolUseMapsToToolCalls`, `TestFromProviderResponseIDIsEmptyNotFabricated`.
- [ ] `regression_test.go` + 4 testdata JSON fixtures, mirroring `gemini/regression_test.go`'s exact convention (system message + tool call + multi-turn history on the request side; a text response with real `usage` on the response side).
- [ ] `cd gateway && go test ./internal/adapter/bedrock/... -v`.

### Task 3: `dataplane.go` wiring — response unmarshaler

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go` (`responseUnmarshalers` map + import)

**Steps:**
- [ ] Add the `"bedrock"` entry to `responseUnmarshalers`, per the RFC's exact snippet.
- [ ] Add the `github.com/kelvran/gateway/gateway/internal/adapter/bedrock` import (may already be present via `main.go`'s registry — confirm before adding a duplicate).
- [ ] `cd gateway && go build ./...`.

### Task 4: Real end-to-end HTTP integration test

**Files:**
- Modify: `gateway/cmd/gateway/integration_test.go`

**Steps:**
- [ ] `newMockBedrockUpstream` — decode into `bedrock.Request`, respond with a real-shaped `bedrock.Response`. Assert the incoming request's `Authorization` header starts with `AWS4-HMAC-SHA256` and a real `X-Amz-Date` header is present — the real, load-bearing proof that SigV4 signing genuinely reached the upstream call, not just unit-tested in isolation.
- [ ] `newIntegrationServerBedrock` — needs `t.Setenv` for `access_key_id_env`/`secret_access_key_env`/`region` (fake, clearly-non-real values, e.g. `"AKIAFAKEFAKEFAKEFAKE"`/`"fakeSecretNotARealAWSSecret"` — never anything resembling a real credential shape that could be mistaken for a leaked secret in test output).
- [ ] `TestIntegrationBedrockRequestSucceeds` and `TestIntegrationBedrockToolCallRoundTrip` (mirroring Gemini's own tool-call round-trip test, proving the simpler `toolUseId`-only correlation works end-to-end).
- [ ] `cd gateway && go test ./cmd/gateway/... -v -run Bedrock`.

---

## Phase 3: Docs, verify, ship

### Task 1: Documentation

**Files:**
- Modify: `gateway/ARCHITECTURE.md` (Canonical Schema & Provider Adapters section; Package Layout's adapter line), `gateway/internal/streaming/types.go` (confirm the existing comment already correctly excludes Bedrock — likely no change needed, verify don't assume), `gateway/config.example.yaml` (a real, correctly-shaped example `bedrock` deployment entry using fake credential env-var names), `gateway/changelog/unreleased.md`, `DECISIONS.md`, `docs/agents/LOGS.md`, `STATUS.md`

**Steps:**
- [ ] Update `gateway/ARCHITECTURE.md`'s adapter Package Layout line to name `bedrock` as real (non-streaming only) — every adapter now real except streaming-for-Bedrock.
- [ ] Add a real `bedrock` deployment example to `config.example.yaml` with a clear comment that `access_key_id_env`/`secret_access_key_env` name env vars holding real AWS credentials, never literal values, and `region` is a plain (non-secret) string.
- [ ] Changelog + `DECISIONS.md` + `docs/agents/LOGS.md` + `STATUS.md`, per this project's established convention — explicitly naming the `"amazonbedrockfrontendservice"` signing-name correction and the real config-schema expansion as the two findings that made this bigger than every prior adapter pass.

### Task 2: Full verification and ship

**Steps:**
- [ ] `cd gateway && go build ./... && go test ./... && go vet ./... -race && golangci-lint run ./...` — clean, zero regressions to any other adapter/dataplane/controlplane test.
- [ ] Root `make verify` (same pre-existing, unrelated rootless-Docker caveat as every prior pass this session).
- [ ] `git add` the exact touched files; commit with a `feat(gateway):` conventional-commit message.
- [ ] Push; watch real CI to green.
- [ ] Final `STATUS.md` commit confirming the exact commit SHA and CI run ID.

## Scope Gate

Architecturally-scoped work (a new real adapter requiring a genuine config-schema change, a new external dependency, and a new signing code path no prior adapter needed) — correctly warranting this plan + `docs/rfcs/2026-09-04-bedrock-adapter.md`.
