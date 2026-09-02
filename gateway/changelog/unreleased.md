# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `gateway` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.1.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) — load-bearing for the Go module path/tag (`v0.1.0`, `v0.2.0`, ...).

## Added

- Initial code skeleton per `docs/plans/2026-09-02-initial-code-scaffolding.md`: canonical request/response types; real OpenAI and Anthropic provider adapters with round-trip tests; stubbed Gemini/Bedrock/OpenAI-compat adapters (typed "not implemented" errors); the `cache.Cache` interface with a working in-process L1 exact-match implementation and dormant `grpcserver`/`grpcclient` extraction seams; a single static virtual-key auth check (constant-time comparison); an in-memory token-bucket rate limiter; a non-streaming dataplane pipeline wiring auth → rate-limit → cache → routing (round-robin + single fallback) → adapter → cost accounting → structured JSON logging; a stdlib-only YAML config loader; a multi-stage Dockerfile. Streaming, distributed rate limiting, Decimal-precision cost accounting, MCP/A2A, and guardrails are explicitly deferred — see `docs/rfcs/2026-09-02-initial-code-scaffolding.md`.

## Changed

## Deprecated

## Removed

## Fixed

## Security
