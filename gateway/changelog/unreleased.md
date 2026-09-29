# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `gateway` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.2.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) — load-bearing for the Go module path/tag (`v0.1.0`, `v0.2.0`, ...).

## Added

## Changed

- `gen_ai.client.token.usage` (Histogram, with a `gen_ai.token.type` enum attribute) is replaced with the current OTel GenAI semantic-conventions token-metrics shape (`open-telemetry/semantic-conventions-genai` PR #374, merged 2026-09-22): 4 new Counters (`gen_ai.client.inference.usage.{input,output,cache_read.input,cache_write.input}_tokens`, each carrying a new `gen_ai.token.modality` attribute) plus 2 new percentile-only Histograms (`gen_ai.client.inference.operation.{input,output}_tokens`). `gen_ai.client.operation.duration` (the separate duration histogram a live Grafana dashboard and Prometheus SLO rules depend on) is unchanged. A 5th new counter in the same spec family, `reasoning.output_tokens`, is deliberately not added yet — Kelvran doesn't track reasoning-token counts anywhere today; adding an always-empty or fabricated-zero instrument would be worse than not adding it. Found via a live end-to-end research pass immediately after the v0.16.0 release.

## Deprecated

## Removed

## Fixed

## Security
