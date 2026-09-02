# Project Status

## Status

🟢 Initial code scaffolding + a deepened test suite + real SSE streaming + real multi-tenant virtual keys/budgets — all complete and verified. `make verify` passes end-to-end for both deployables (build/vet/lint/test). Git initialized (trunk-based, `main`-only). CI now exists (`.github/workflows/ci.yml`), not yet run against a real push. Virtual keys + budgets work is implemented and verified but **not yet committed**.

## IMPORTANT

Real source code now exists in `gateway/` and `evals/`, but it is a **deliberately narrow skeleton**, not a feature-complete Phase 0. `docs/rfcs/2026-09-02-initial-code-scaffolding.md` is the authoritative list of what's real vs. intentionally stubbed vs. not built at all — read it before assuming any capability works. Everything in `README.md`/`docs/users/USER_GUIDE.md`/`docs/operations/DEPLOY.md` describing behavior beyond that scaffolding's actual scope is still the *intended* shape, not a tested reality.

## Current Version

Unreleased. Neither `gateway` nor `evals` has a tagged version yet — both have unreleased Added entries in their respective `changelog/unreleased.md`.

## Current Phase

Virtual keys + per-key budgets/rate-limits/allowed-models, just landed on top of streaming + the initial scaffolding. Same spec→plan→implement pipeline: `docs/rfcs/2026-09-02-virtual-keys-budgets.md` (spec) → `docs/plans/2026-09-02-virtual-keys-budgets.md` (7-task plan) → implementation. Real multi-tenancy for the first time: hash-matched `identity.VirtualKey`s (config holds `key_hash`, never the raw secret), a new `internal/budget.Tracker`, one `ratelimit.TokenBucket` per key, and `cache.Key()`'s new tenant dimension — shipped in the SAME pass as multi-tenancy itself, so there's no window where more than one tenant existed without cache isolation. Breaking config change: `api_key_env` → `virtual_keys:` (see `docs/users/USER_GUIDE.md` §3).

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

## Last Completed Task

Real multi-tenant virtual keys + per-key budgets/rate-limits/allowed-models for `gateway`, implemented end-to-end (identity rewrite → new budget package → cache tenant-namespacing → dataplane wiring → main.go + integration tests → docs/changelog) per `docs/plans/2026-09-02-virtual-keys-budgets.md`. See `docs/agents/LOGS.md`'s latest entry for full detail.

## Next Action

**Commit the virtual-keys-and-budgets work** — it is implemented and independently verified but still sitting uncommitted in the working tree. After that, no next feature has been decided: OTel/`agent_run_id` propagation, distributed (Redis-backed) rate limiting, and Cache's L2 layer remain open candidates, pending the founder's explicit choice.

## Release Runbook

See `RELEASE.md` — not restated here.

## Decisions Made

See `DECISIONS.md` and `docs/decisions/` — not restated here.

## Active Blockers

- Project name **Kelvran** still needs a manual USPTO TESS / WHOIS trademark clearance before public registration of the domain/GitHub org/packages — tracked in `DECISIONS.md`, not yet done.
- No git remote configured — repo is local-only, so `.github/workflows/ci.yml` has never actually run.

*(Nothing here is vulnerability-shaped; if a future blocker ever is, it routes to `SECURITY.md`'s private channel instead of appearing on this public page.)*

## Context for Next Session

Read `REPO_LAYOUT.md` first for the current directory map, then `docs/agents/MEMORY.md` and the tail of `docs/agents/LOGS.md` per `AGENTS.md`'s own "Always" rule. The two source-of-truth planning documents for everything created so far live outside this repo, in the parent workspace: `Not-Humans-World/ai-infra-research/naming-and-docs-plan.md` and `Not-Humans-World/ai-infra-research/kelvran-docs-addition-plan.md`.

## Last Updated

2026-09-02
