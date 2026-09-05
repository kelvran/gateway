# RFC: `gen_ai.provider.name` validation and enum remapping

## Status

Accepted, implemented 2026-09-05.

## Context

`docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md`'s own Unresolved Questions named this gap at v1 ship: "Whether `gen_ai.provider.name`'s well-known enum values... should be validated/normalized against the spec's known list, or passed through verbatim from each adapter's `Name()`... they already match the spec's known values for the two real adapters [`openai`/`anthropic`], so there's nothing to normalize yet, but a future non-standard provider name wouldn't be caught." Two more real adapters (`gemini`, `bedrock`) have shipped since, plus `openaicompat` — this is the exact "future" the original RFC anticipated.

Verified directly against the real registry via Context7 (`open-telemetry/semantic-conventions`, `model/gen-ai/deprecated/registry-deprecated.yaml`), not assumed from the grounding pass's own claim alone: `gen_ai.provider.name`'s well-known values include `openai`, `anthropic`, `aws.bedrock`, `gcp.gemini`, `gcp.vertex_ai`, `gcp.gen_ai`, `cohere`, `azure.ai.inference`, `azure.ai.openai`, `ibm.watsonx.ai`, `perplexity`, `x_ai`, `deepseek`, `groq`, `mistral_ai` — no listed value for a generic self-hosted OpenAI-compatible runtime.

Two independent, small gaps, both closed in this pass:

1. **Config-time validation**: `cmd/gateway.buildPipeline` never checked that a configured deployment's `Provider` string actually matched a key in the adapter `registry` — an unregistered provider (e.g. a typo) only failed once a real request happened to route to that deployment, via `dataplane.callDeployment`'s generic `"no adapter registered for provider %q"` error.
2. **Telemetry enum remapping**: Kelvran's own internal provider identifiers for `bedrock`/`gemini` don't match the registry's well-known values (`aws.bedrock`/`gcp.gemini`) — `openai`/`anthropic` already matched verbatim, exactly as the original RFC's own reasoning said.

## Design

### Config-time fail-fast

`buildPipeline` now checks `registry[d.Provider]` immediately after constructing `registry`, before building any `dataplane.Deployment` — a config-load-time (startup) failure, not a first-request-time one. Returns a plain wrapped error naming both the deployment and the bad provider string, matching every other `buildPipeline` validation error's style.

### `genAIProviderName`: a small remapping table in the telemetry leaf

`internal/telemetry/result.go` gains `genAIProviderNameOverrides` (a private `map[string]string`, `bedrock`→`aws.bedrock`, `gemini`→`gcp.gemini`) and `genAIProviderName(provider string) string`, applied at the one place `AttrGenAIProviderName` is set in `RecordChatCompletionResult`. `openai`/`anthropic` pass through unchanged (not in the map, matching their real values already). `openaicompat` also passes through verbatim — deliberately: it has no well-known value in the registry at all, since a self-hosted, wire-protocol-compatible runtime isn't a distinct GenAI provider in OTel's own vocabulary; forcing it into `openai` (the protocol it merely speaks) or leaving it as `openaicompat` (accurate but non-standard) — passing it through verbatim is the more honest choice, since OTel's own `gen_ai.provider.name` field remains "development"-stability and open to non-listed values by design.

This stays entirely inside `internal/telemetry` — no change to `Deployment.Provider`, `adapter.Registry`'s keys, or any adapter's own `Name()`; Kelvran's internal identifiers are unchanged everywhere except the one OTel-facing attribute.

## Alternatives considered

**Renaming Kelvran's own internal provider identifiers to match OTel's values** (e.g. `adapter.Registry`'s `"bedrock"` key becomes `"aws.bedrock"`) — rejected; would ripple through config files, the adapter registry, deployment YAML, and every existing test for zero real benefit — the internal identifier and the OTel-facing attribute value are allowed to differ, and now correctly do.

**Depending on `go.opentelemetry.io/otel/semconv`'s incubating GenAI module for the enum** — rejected, consistent with the original RFC's own already-made decision: that module's Go API changes as the still-"development"-stability spec changes; a small, self-contained map needs no such dependency.

## Verification

`go build ./... && go vet ./...` clean. `golangci-lint run ./...` → `0 issues`. `go run github.com/fe3dback/go-arch-lint@v1.18.0 check` → clean. `go test ./... -race` → every package `ok` except the same pre-existing, already-documented rootless-Docker failure.

New `internal/telemetry/result_test.go`: `TestGenAIProviderNameRemapsToWellKnownRegistryValues` (table test over all 5 real providers) and `TestRecordChatCompletionResultRemapsProviderOnSpan` (proves the mapping reaches the real emitted span attribute, not just the helper in isolation). New `cmd/gateway/provider_validation_test.go`: `TestBuildPipelineRejectsUnregisteredProvider` (a deliberate typo) and `TestBuildPipelineAcceptsEveryRealRegisteredProvider` (a subtest per real provider, proving the new check never false-positives against a valid config). Sanity-checked both halves independently: temporarily emptied `genAIProviderNameOverrides` — both new telemetry tests failed with the exact wrong (unmapped) string; temporarily disabled the config-time check — the negative `buildPipeline` test failed with "returned nil error, want an error." Both restored.
