# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `gateway` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.1.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) — load-bearing for the Go module path/tag (`v0.1.0`, `v0.2.0`, ...).

- Initial code skeleton per `docs/plans/2026-09-02-initial-code-scaffolding.md`: canonical request/response types; real OpenAI and Anthropic provider adapters with round-trip tests; stubbed Gemini/Bedrock/OpenAI-compat adapters (typed "not implemented" errors); the `cache.Cache` interface with a working in-process L1 exact-match implementation and dormant `grpcserver`/`grpcclient` extraction seams; a single static virtual-key auth check (constant-time comparison); an in-memory token-bucket rate limiter; a non-streaming dataplane pipeline wiring auth → rate-limit → cache → routing (round-robin + single fallback) → adapter → cost accounting → structured JSON logging; a stdlib-only YAML config loader; a multi-stage Dockerfile. Streaming, distributed rate limiting, Decimal-precision cost accounting, MCP/A2A, and guardrails are explicitly deferred — see `docs/rfcs/2026-09-02-initial-code-scaffolding.md`.
- Test/lint infrastructure: a real end-to-end HTTP integration test suite (`cmd/gateway/integration_test.go`) driving the full pipeline through a real `httptest` server and client against a mock upstream; byte-for-field regression fixtures (`internal/adapter/{openai,anthropic}/testdata/`) pinning both adapters' wire formats independently of the existing round-trip unit tests; `go test -fuzz` targets for the cache-key fabricator and the hand-rolled YAML config parser; `go test -bench` baselines for the in-process cache and the token-bucket rate limiter; and a tuned `.golangci.yml` (v2 schema: errcheck, govet, staticcheck, unused, ineffassign, errorlint, gofmt/goimports) with zero outstanding findings.
- Real SSE streaming (`stream: true`) for `/v1/chat/completions`, per `docs/rfcs/2026-09-02-streaming-support.md`: a new provider-agnostic `internal/streaming` package (canonical `ChatCompletionChunk` types, an SSE `Reader`/`Writer`, and the `StreamDecoder`/`StreamingAdapter` interfaces); real stateful `StreamDecoder`s for the OpenAI (near-passthrough, multi-chunk tool-call argument accumulation) and Anthropic (typed event sequence — `message_start`/`content_block_start`/`content_block_delta`/`content_block_stop`/`message_delta`/`message_stop` — with per-block-index state tracking) adapters; dataplane wiring for both the cache-hit path (a complete cached response is synthesized into a fake stream) and the cache-miss path (a real upstream stream is simultaneously written to the client and accumulated back into a canonical `ChatResponse` for cache write-back and cost accounting); the existing pre-first-byte fallback rule now applies to streaming too. Gemini/Bedrock/openaicompat remain unsupported for streaming — a request to one of them returns a typed `ErrStreamingNotSupported` (HTTP 400), never a silent fallback to buffering.

## Changed

## Deprecated

## Removed

## Fixed

## Security
