# Project Status

## Status

🟢 Initial code scaffolding + a deepened test suite + real SSE streaming + real multi-tenant virtual keys/budgets + real OTel tracing/agent_run_id + Decimal-precision cost accounting + restart-durable budget persistence — all complete, verified, and committed. `make verify` passes end-to-end for both deployables (build/vet/lint/test). Git initialized (trunk-based, `main`-only — reaffirmed a third time 2026-09-03, see `docs/development/BRANCHES.md`). CI exists (`.github/workflows/ci.yml`), still not yet run against a real push — this pass's own research flagged that as the more urgent gap than any branch-topology change.

## IMPORTANT

Real source code now exists in `gateway/` and `evals/`, but it is a **deliberately narrow skeleton**, not a feature-complete Phase 0. `docs/rfcs/2026-09-02-initial-code-scaffolding.md` is the authoritative list of what's real vs. intentionally stubbed vs. not built at all — read it before assuming any capability works. Everything in `README.md`/`docs/users/USER_GUIDE.md`/`docs/operations/DEPLOY.md` describing behavior beyond that scaffolding's actual scope is still the *intended* shape, not a tested reality.

## Current Version

Unreleased. Neither `gateway` nor `evals` has a tagged version yet — both have unreleased Added entries in their respective `changelog/unreleased.md`.

## Current Phase

Restart-durable budget-spend persistence, just landed on top of Decimal-precision cost accounting + OTel tracing + virtual keys/budgets + streaming + the initial scaffolding — the direct continuation of the Decimal fix (the numbers are now exact, and now they also survive a restart). Same spec→plan→implement pipeline: `docs/rfcs/2026-09-03-budget-persistence.md` (spec) → `docs/plans/2026-09-03-budget-persistence.md` (5-task plan) → implementation. `go.etcd.io/bbolt` (third external Go dependency, near-zero *new* transitive weight) backs `budget.Tracker` when a new `budget.persist_path` config field is set — a deliberate, bounded stepping stone ahead of the already-planned Postgres control-plane store, single-instance only, opt-in only (unset = unchanged pure in-memory behavior). Research changed the design mid-flight: the initial SQLite lean was replaced with `bbolt` after research surfaced SQLite's own documented dependency-pinning fragility for this narrow use case.

## Verification State (measured, not assumed)

- `make verify` (root) — **passes cleanly**: `golangci-lint run ./...` → `0 issues`; `ruff check .` → `All checks passed!`; `go build/test` → all packages `ok`; `uv run pytest tests/` → **43 passed, 4 skipped** (Docker-sandbox integration tests, skip-by-default unless `RUN_DOCKER_TESTS=1`; separately confirmed 4/4 passing against a real local Docker daemon).
- Virtual keys + budgets, specifically: `cd gateway && go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./...` → all packages `ok`, `0 issues`, race-clean. New/updated tests: `internal/identity` (11 tests, including hash-casing normalization), `internal/budget` (8 tests, including a 100-goroutine × 100-call concurrent-update race test), `internal/cache` (added the load-bearing cross-tenant isolation test + updated fuzz), `internal/gateway/controlplane` (virtual_keys parsing), `internal/gateway/dataplane` (9 new tests across the buffered and streaming paths — check-ordering, per-key rate-limit independence, budget rejection, and the load-bearing cache-isolation proof), and 3 new real end-to-end HTTP integration tests in `cmd/gateway` (model-not-allowed → 403, budget-exceeded → 429 distinguishable from rate-limit, cross-tenant cache isolation through the real HTTP path).
- Streaming, specifically: `cd gateway && go build ./... && go vet ./... && go test ./... && golangci-lint run ./...` → all packages `ok`, `0 issues`. `cmd/gateway/integration_test.go` now has 8 real end-to-end HTTP tests (up from 4), including a real streaming request against a genuine OpenAI-shaped mock upstream, a genuine Anthropic-shaped mock upstream (typed `event:`/`data:` frames, no `[DONE]`, matching the real API), a streaming cache-hit (fake-streamed, second upstream call count stays at 1), and a streaming request to an unsupported provider returning a typed 400. One real bug found and fixed during this pass — see `DECISIONS.md`'s streaming line and `docs/agents/LOGS.md`'s latest entry.
- Streaming test coverage then brought to full parity with every category the initial scaffolding's test-deepening pass established: `FuzzReaderNeverPanics` on the SSE reader (583,023 execs / 20s local run, no crash found), `BenchmarkReaderNext`/`BenchmarkWriteChunk` baselines (~114 ns/op and ~731 ns/op respectively on the dev machine — recorded, not gated), byte-exact golden tests pinning the outbound SSE wire format (both fields present and the null-vs-omitted distinction for `finish_reason` vs. `usage`), and `go test ./... -race` clean across the whole module.
- Gateway now has real HTTP integration tests (full pipeline through `httptest`, mock upstream only), wire-format regression/golden fixtures for both real adapters, `go test -fuzz` on the cache-key fabricator and YAML config parser (both clean — no crashing input found after 20s each), and cache/rate-limiter benchmarks (recorded baselines, not gated).
- Evals now has CLI integration tests, 5 Hypothesis property-based regression tests on the Wilson-interval math, a golden fixture pinning the LLM-judge prompt, and a non-Docker regression test for the sandbox wrapper's missing-binary error path.
- `docker build -t kelvran-gateway:scaffold gateway/` — succeeded (multi-stage `golang:1.25-alpine` → `scratch`, ~5MB image); manually smoke-tested (401 on missing auth header) from both the local binary and the built container.
- Directly read (not just trusted a report on) five files across both passes: `identity.go`'s constant-time comparison, `cache/key.go`'s SHA-256 fabrication, `stats.py`'s Wilson formula, `.golangci.yml`'s exclusion-rule justification, and `test_stats_properties.py`'s Hypothesis invariants — all genuinely correct, not hand-waved.
- `.github/workflows/ci.yml` exists and mirrors `make verify` exactly, but has not yet run against a real push (no remote configured).
- OTel tracing, specifically: `cd gateway && go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./...` → all packages `ok`, `0 issues`, race-clean. 20 new tests: `internal/telemetry` (10, including exporter construction for all 3 modes and a real Baggage round-trip), `internal/gateway/controlplane` (1, optional-section proof), `internal/gateway/dataplane` (5 span-assertion tests — success, auth-failure-without-identity, cache-hit, agent_run_id-from-baggage, streaming mirror), `cmd/gateway` (1 real end-to-end HTTP→baggage-header→span integration test). Two real bugs found and fixed in test infrastructure (not production code) while building this — see `DECISIONS.md`'s OTel line and `docs/agents/LOGS.md`'s latest entry for both (OTel's global TracerProvider only re-delegates a Tracer's first real provider; a test binary that never calls `run()` never gets the propagator `Init` would normally set).
- Decimal-precision cost accounting, specifically: `cd gateway && go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./...` → all packages `ok`, `0 issues`, race-clean, zero regressions across the whole module. Two load-bearing precision-proof tests reproduce a real, empirically-verified `float64` drift (10,000 additions of a realistic per-request cost fragment drifting to `0.07499999999999333589` instead of exactly `0.075`) and confirm the decimal path doesn't drift over the same accumulation. One real, more severe bug found and fixed during this pass's own grounding research (not assumed, not a subagent's report) — see `DECISIONS.md`'s Decimal line and `docs/agents/LOGS.md`'s latest entry: `budget_usd: 1` previously collided with the parser's overly-permissive boolean detection and silently fell back to unlimited budget.
- Budget persistence, specifically: `cd gateway && go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./...` → all packages `ok`, `0 issues`, race-clean, zero regressions (persistence is fully opt-in via a new, previously-absent config field — the entire pre-existing integration suite passes unmodified). The load-bearing proof, `TestIntegrationBudgetPersistsAcrossRestart`, closes one real `*dataplane.Pipeline`, opens a second independent one against the same `persist_path`, and confirms the second instance's very first request is already budget-rejected — a genuine close-and-reopen cycle through the full real HTTP stack. `internal/budget/boltstore`'s own `TestPersistsAcrossReopen` proves the same thing at the storage layer in isolation.

## Last Completed Task

Restart-durable budget-spend persistence for `gateway`, implemented end-to-end (new `internal/budget/boltstore` package → `budget.Tracker`/`dataplane.Pipeline` wiring → controlplane config → `cmd/gateway` wiring + the real restart-survival integration test → docs/changelog) per `docs/plans/2026-09-03-budget-persistence.md`. See `docs/agents/LOGS.md`'s latest entry for full detail, including how research changed the storage-backend choice mid-flight (SQLite → bbolt).

## Next Action

Branch strategy reaffirmed a third time (2026-09-03, `docs/development/BRANCHES.md`) — no branch-topology change, but the research surfaced a concrete precondition gap: CI has never run against a real push, which is the exact thing the "solo maintainer may commit direct to trunk" exception assumes is already true. The remaining roadmap items are the open candidates: distributed (Redis) rate limiting, release readiness (tag/remote/real CI run — needs the founder's direct involvement), the `api/` cross-language contract, Cache L2/L3 completion, and the evals skeptic-panel — pending the founder's explicit choice.

## Release Runbook

See `RELEASE.md` — not restated here.

## Decisions Made

See `DECISIONS.md` and `docs/decisions/` — not restated here.

## Active Blockers

- Project name **Kelvran** still needs a manual USPTO TESS / WHOIS trademark clearance before public registration of the domain/GitHub org/packages — tracked in `DECISIONS.md`, not yet done.
- ~~No git remote configured~~ — resolved 2026-09-03: repo is now public at `github.com/kelvran/gateway`. First real push immediately caught a genuine latent bug: `.github/workflows/ci.yml` pinned `golangci-lint-action@v6`, which does not support golangci-lint v2's version string at all — the `gateway` lint job failed outright on its first real run. Fixed by bumping to `@v7`; `evals`'s job passed on the first try. Exactly the CI-trustworthiness gap the third branch-strategy research pass (2026-09-03) flagged as more urgent than any branching change.

*(Nothing here is vulnerability-shaped; if a future blocker ever is, it routes to `SECURITY.md`'s private channel instead of appearing on this public page.)*

## Context for Next Session

Read `REPO_LAYOUT.md` first for the current directory map, then `docs/agents/MEMORY.md` and the tail of `docs/agents/LOGS.md` per `AGENTS.md`'s own "Always" rule. The two source-of-truth planning documents for everything created so far live outside this repo, in the parent workspace: `Not-Humans-World/ai-infra-research/naming-and-docs-plan.md` and `Not-Humans-World/ai-infra-research/kelvran-docs-addition-plan.md`.

## Last Updated

2026-09-03
