# Testing Strategy

## 1. Testing Philosophy

Every form of test below exists from the start, not bolted on later — a proxy/cache hot path and a security-sensitive semantic-cache layer earn that discipline. Organize tests by subsystem, mirroring the package layout in `gateway/ARCHITECTURE.md`/`evals/ARCHITECTURE.md`, not by test-type-first folders. Never sleep on wall-clock time to test a TTL, retry, or backoff — inject a clock/timer interface everywhere time matters, so tests run in milliseconds and are deterministic.

## 2. Test Pyramid Overview

| Layer | Go tooling (`gateway`) | Python tooling (`evals`) | Runs on | Speed |
|---|---|---|---|---|
| Unit | `go test`, table-driven tests | `pytest` | Every commit | Seconds |
| Integration | `go test` + `httptest`, real Redis/Postgres via testcontainers | `pytest` + testcontainers | Every PR | Under a minute |
| Contract (`api/`) | `buf breaking` in CI | `buf breaking` in CI (same run) | Every PR touching `api/` | Seconds |
| End-to-end | Go test binary driving a real built `gateway` binary | `pytest` driving a real `evals` process | PR smoke subset / nightly full sweep | Minutes (smoke) to tens of minutes (full) |
| Load/Performance | `k6` or `vegeta` against a running `gateway`, mocked upstream | Locust-style load against `evals`' rollout scheduler | Nightly / pre-release | Minutes |
| Chaos/Fault-Injection | Toxiproxy between `gateway` and mocked upstreams | Toxiproxy or fault-injecting mocks around sandbox execution | Nightly | Minutes |
| Security/Fuzz | Go native fuzzing (`go test -fuzz`) | Hypothesis property-based tests | Nightly / on-demand | Varies |

## 3. Unit Tests

Standard table-driven tests in Go (`testify` for assertions where it reduces boilerplate, not required), `pytest` fixtures/parametrize in Python. Every provider adapter (`gateway/internal/adapter/*`) gets a dedicated round-trip test: canonical → provider-native → canonical must be lossless for every field the adapter claims to handle, per `gateway/ARCHITECTURE.md`'s four documented normalization hazards (tool-call argument encoding, system-prompt placement, streaming event shape, unknown-field preservation).

## 4. Integration Tests

Real Redis/Postgres via testcontainers, never mocked at this layer — the point of an integration test is to catch what a mock would hide. **Explicit ban: never hit a real upstream LLM provider API in CI.** Use a local mock server that speaks each provider's actual wire format (recorded fixtures from real responses, replayed) — this is what makes the adapter round-trip tests in §3 meaningful at the integration layer too, against a server that behaves like the real thing rather than a hand-rolled stub.

## 5. Contract Tests (`api/` boundary)

`buf breaking` in CI is mandatory on any PR touching `api/` — this is not optional and not a "nice to have," per `AGENTS.md`'s Conventions section; real since `docs/rfcs/2026-09-03-api-gatewayevents-contract.md`. On top of that: a golden-fixture round-trip test, where `evals` decodes and `gateway` encodes the same checked-in set of `api/gatewayevents` messages, confirming both languages' generated bindings agree on the wire format — real for `gatewayevents` (`evals/tests/test_ingestion_golden_roundtrip.py`, fixture at `evals/tests/fixtures/gateway_decision_event.json`, produced by `gateway`'s own generated bindings, not hand-authored). `api/otel` stays out of scope for this test — that contract remains a deliberate placeholder (see `api/README.md`). A lightweight substitute for a full Pact Broker (tracked as an open question in `docs/research/RESEARCH.md` for whether that's ever worth adopting).

## 6. End-to-End Tests

A fast PR-blocking smoke subset (a handful of critical-path scenarios: one successful request per provider, one cache hit, one cache miss, one failover) plus a full nightly sweep covering the rest. Never let the full sweep block a PR — that's exactly the kind of slow-test-in-CI problem that erodes the discipline of running tests at all.

## 7. Load/Performance Tests

Mocked upstream only (a real provider's rate limits would throttle the test itself, not measure Kelvran). Tiny smoke-only load test on PR (catch an obvious regression), full load profile nightly and pre-release. Track p50/p95/p99 latency and memory under load as trend lines, not just pass/fail thresholds — a load test that only asserts "under 500ms" misses a p99 that quietly doubled.

## 8. Chaos/Fault-Injection Tests

Toxiproxy sits between `gateway` and its (mocked) upstreams, injecting latency, connection resets, and timeouts. Every chaos scenario is tied to a named row in `THREAT_MODEL.md`, not invented ad hoc:
- Retry-storm scenario → `THREAT_MODEL.md`'s Gateway "Denial of Service" row (many agents retrying on a fixed interval).
- Provider-outage scenario → the circuit-breaker/fallback-chain behavior described in `gateway/ARCHITECTURE.md`.
- Cache-miss-storm scenario → `THREAT_MODEL.md`'s Cache "Denial of Service" row, confirming the singleflight/request-coalescing mechanism actually collapses concurrent misses into one upstream call.

Revisit Chaos Mesh instead of Toxiproxy only if/when the production deployment target is actually Kubernetes (tracked in `docs/research/RESEARCH.md`).

## 9. Security/Fuzz Tests

Go native fuzzing (`go test -fuzz`) on every parser that touches untrusted input — the cache-key fabricator and any streaming-response parser are the two highest-value fuzz targets, given `THREAT_MODEL.md`'s cache-poisoning and streaming-statefulness concerns. Both are now real: `FuzzKey` (`internal/cache/key_fuzz_test.go`) and `FuzzReaderNeverPanics` (`internal/streaming/reader_fuzz_test.go`, added alongside streaming support — the SSE `Reader` parses bytes an upstream provider controls, the same untrusted-input boundary `FuzzKey` covers on the request side). Hypothesis property-based tests on the Cache's entity/date hard-gate specifically: generate adversarial near-duplicate prompt pairs and assert the hard-gate never lets a semantically-different pair through — this is the test-level enforcement of the CacheAttack/KeyPooling defenses `THREAT_MODEL.md` and `AGENTS.md`'s Boundaries both already commit to.

## 10. Agent-Conduct Testing (Scoped, Deferred)

A natural fourth test category would be "does an AI coding agent working in this repo actually follow `AGENTS.md`" — but this is explicitly **not** built now, and needs one hazard flagged before it ever is: **never name it `evals`** — that's the product's own deployable, and reusing the name for a test-harness concept (as the `obra/superpowers` plugin does with its own `evals/` directory for LLM-behavior scenarios) would collide badly in this specific repo. If this is ever built, reserve the name **`agent-conduct/`** instead.

The YAGNI gate: build nothing here until there's actual evidence of repeated `AGENTS.md` violations (tracked via `docs/agents/AGENTS_LEARNING.md`'s Evolution Log — if a Category: Anti-Pattern entry about agent conduct recurs, that's the trigger). Until then, the cheap v1 — if and when it's warranted — is a periodic manual checklist review, not a scenario-driven LLM-judge harness.
