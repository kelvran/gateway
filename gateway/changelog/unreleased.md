# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `gateway` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.2.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) — load-bearing for the Go module path/tag (`v0.1.0`, `v0.2.0`, ...).

## Added

- Bedrock Guardrails `PROMPT_ATTACK` ML classifier as an opt-in, config-gated second opinion alongside the existing regex heuristic (`internal/guardrail/bedrockguard`), reusing the existing per-category fail-open/fail-closed policy.
- Real container-image publishing pipeline: `ghcr.io/kelvran/gateway`, built and pushed on every push to `main`, with a CycloneDX SBOM, a cosign keyless signature, and SLSA Build Level 2 provenance — all bound to the exact build digest. A real `gateway/vX.Y.Z` release tag additionally publishes a matching semver image tag.
- Reasoning-content (`ReasoningBlocks`) canonical schema, captured and losslessly replayed across all four providers — Anthropic (buffered + streaming), Bedrock, Gemini, and openaicompat (vLLM/Ollama/llama.cpp's live wire field names) — closing a real provider-hard-400 risk on multi-turn tool-use conversations that previously dropped these blocks entirely.
- Agent-run cost and cache-savings attribution: `agent_run_id`, `cost_usd`, and `savings_usd` on `GatewayDecisionEvent`, plus `evals cost-report` support for summing both, scoped to a single agent run rather than only an aggregate total.
- Cost-tier deployment routing: an optional, opt-in per-deployment cost tier that prefers the cheapest healthy tier within a model group before falling through.
- Cache negation-particle hard gate (L3-lite), cache cost-observability metrics/dashboard, and admin API read-route audit logging (not just writes).
- `internal/prompt`: a real prompt-management CRUD store with versioning and `{{name}}`-allowlist substitution.
- Structured-output, prompt-resolution, and cache-token telemetry; `trace_id`/`span_id` promoted to top-level structured-log fields; a span event emitted for each failed fallback-chain hop.

## Changed

## Deprecated

## Removed

## Fixed

- Truncated (`finish_reason:"length"`) responses were cacheable and could later be replayed to a request with a higher `max_tokens` that could genuinely have produced more content — now excluded from every cache write.
- The streaming path could durably cache a response that a post-call guardrail check had already blocked from delivery.
- Warn-tier guardrail findings (the fail-open-with-logging categories) were never actually logged, indistinguishable from the detector never running at all.
- Bedrock structured-output (`json_schema`) requests failed unless every object node explicitly set `additionalProperties: false`; Bedrock also rejected the `json_object` response-format shape entirely — both now handled transparently.
- Gemini's thought-part content rode the same `text` field an ordinary answer uses and was silently merged into visible output rather than captured separately.
- `selectHealthy`'s cost-tier filtering could return an actively unhealthy deployment even when a real, same-cycle healthy alternative existed.
- The negation-particle cache gate was blind to typographic (curly) apostrophes, silently defeating the gate for a common input class.
- A budget-tracker race during a concurrent rolling-window reset could undercount real spend by up to a cold-start key's entire remaining cap; the mid-stream reservation top-up had the identical race, fixed the same way.
- `kelvran.cache.lookup` counted every early-return request (auth failure, budget exceeded, rate-limited) as a cache miss even though it never reached the cache-check stage.
- Several cross-adapter response-shape parity gaps: Anthropic's buffered path didn't map `stop_reason` onto canonical `finish_reason`; OpenAI/openaicompat's `message.refusal` field was silently discarded; Gemini's prompt-level safety-block signal was unparsed on both paths.
- The router's health-check-then-ramp-admission sequence had a real TOCTOU race between an independent health snapshot and the actual admission check.
- Hidden Unicode tag characters could bypass `SecretKeyDetector`/`IPAddressDetector` pattern matching.
- A resolved prompt's content was not validated against MIME spoofing; prompt-version telemetry could report the wrong resolved version.
- `UpsertVirtualKey`/`DeleteVirtualKey` had a real concurrent-write race, fixed with a `CompareAndSwap` retry loop.
- Admin-created virtual keys could resolve to an incorrect rate limit default; `price_table` accepted missing/malformed/negative rates at config load instead of rejecting them.
- A cache-token accounting invariant violation could go unclamped in cost accounting.
- L3-lite cache matching was not gated on `ResponseFormat`/prompt identity, a real cross-contamination risk between differently-shaped requests for otherwise-similar content.

## Security

- A real GitHub Actions script-injection vector in the new image-tag-computation CI step: `github.ref_name` was interpolated directly into a shell script rather than bound through `env:` — fixed, and swept for the same pattern across both workflow files.
- All GitHub Actions across both `ci.yml` and `evals-judge-nightly.yml` pinned to commit SHAs, replacing every mutable version tag.
