# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `gateway` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.2.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) — load-bearing for the Go module path/tag (`v0.1.0`, `v0.2.0`, ...).

## Added

- `kelvran.guardrail.fail_open` OTel counter (`{request}`), attributed with `kelvran.virtual_key.id` and a new `kelvran.guardrail.stage` attribute (`precall` / `postcall` / `embeddings`). Incremented at most once per request per stage when a guardrail detector errored or panicked and the request was still allowed through — the guardrail equivalent of the existing `kelvran.ratelimit.fail_open` / `kelvran.budget.fail_open` counters. A Block-tier detector error fails closed and is not counted. Recorded from the dataplane's five `guardrails.Check` call sites (not inside `guardrail.Engine`, which stays an import-free leaf), next to a new `guardrail_fail_open` log line that carries `trace_id`/`key_id`/`stage`; the engine's own `guardrail_detector_error` line is unchanged. Per `docs/upgrade-research/kelvran-deep-research-round3-2026-10-07.md`.

## Changed

- `gen_ai.client.token.usage` (Histogram, with a `gen_ai.token.type` enum attribute) is replaced with the current OTel GenAI semantic-conventions token-metrics shape (`open-telemetry/semantic-conventions-genai` PR #374, merged 2026-09-22): 4 new Counters (`gen_ai.client.inference.usage.{input,output,cache_read.input,cache_write.input}_tokens`, each carrying a new `gen_ai.token.modality` attribute) plus 2 new percentile-only Histograms (`gen_ai.client.inference.operation.{input,output}_tokens`). `gen_ai.client.operation.duration` (the separate duration histogram a live Grafana dashboard and Prometheus SLO rules depend on) is unchanged. A 5th new counter in the same spec family, `reasoning.output_tokens`, is deliberately not added yet — Kelvran doesn't track reasoning-token counts anywhere today; adding an always-empty or fabricated-zero instrument would be worse than not adding it. Found via a live end-to-end research pass immediately after the v0.16.0 release.

## Deprecated

## Removed

## Fixed

- The prompt-injection detector's hidden-Unicode tag range now covers the whole Unicode Tags block `U+E0000`–`U+E007F` (previously `U+E0020`–`U+E007F`), as OWASP LLM01 2026 prescribes. `U+E0000` and `U+E0001` (LANGUAGE TAG) were encodable, emittable code points that passed undetected.

## Security
