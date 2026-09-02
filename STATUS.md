# Project Status

## Status

🟢 Initial code scaffolding complete and verified — `gateway` builds/vets/tests clean, `evals` test suite passes. Git now initialized (trunk-based, `main`-only per the reaffirmed `docs/development/BRANCHES.md`); no CI exists yet.

## IMPORTANT

Real source code now exists in `gateway/` and `evals/`, but it is a **deliberately narrow skeleton**, not a feature-complete Phase 0. `docs/rfcs/2026-09-02-initial-code-scaffolding.md` is the authoritative list of what's real vs. intentionally stubbed vs. not built at all — read it before assuming any capability works. Everything in `README.md`/`docs/users/USER_GUIDE.md`/`docs/operations/DEPLOY.md` describing behavior beyond that scaffolding's actual scope is still the *intended* shape, not a tested reality.

## Current Version

Unreleased. Neither `gateway` nor `evals` has a tagged version yet — both have unreleased Added entries in their respective `changelog/unreleased.md`.

## Current Phase

Initial code scaffolding, just landed. Both documentation batches (45 files) were completed first; then an RFC (`docs/rfcs/2026-09-02-initial-code-scaffolding.md`) and a task-by-task plan (`docs/plans/2026-09-02-initial-code-scaffolding.md`) were written before any code, per the user's explicit spec-to-plan-to-implementation request; then Gateway (9 tasks) and Evals (6 tasks) were implemented in parallel by two independent agents, each required to run real build/test commands after every task.

## Verification State (measured, not assumed)

- `cd gateway && go build ./... && go vet ./... && go test ./...` — all packages `ok`, `go vet` produces zero output. Independently re-run and confirmed, not just taken on the implementing agent's word.
- `cd evals && uv run pytest tests/` — **30 passed, 4 skipped** (the Docker-sandbox integration tests, skip-by-default unless `RUN_DOCKER_TESTS=1`; separately confirmed passing 4/4 against a real local Docker daemon).
- `docker build -t kelvran-gateway:scaffold gateway/` — succeeded (multi-stage `golang:1.25-alpine` → `scratch`, ~5MB image); manually smoke-tested (401 on missing auth header) from both the local binary and the built container.
- Directly read (not just trusted a report on) the three most correctness/security-sensitive files: `gateway/internal/identity/identity.go` (constant-time key comparison — correct), `gateway/internal/cache/key.go` (SHA-256 cache-key fabrication — correct, properly decoupled from the adapter package), `evals/evals/stats.py` (Wilson confidence-interval formula — correct, with a documented epsilon clamp).
- Cross-reference spot-checks on the doc set (from the prior batches) — no known regressions from this session, since no `.md` files were edited except the changelog/`DECISIONS.md`/`LOGS.md`/this file.

## Last Completed Task

Initial code scaffolding (spec → plan → implementation) for both deployables — see `docs/agents/LOGS.md`'s latest entry for full detail.

## Next Action

**Streaming (SSE) support** — confirmed as the next priority. The gateway currently only serves buffered, non-streaming responses; this is the next thing that needs its own RFC/plan pair under `docs/rfcs/`/`docs/plans/`, following the same spec→plan→implement pipeline used for the initial scaffolding — not built ad hoc directly on `main`.

## Release Runbook

See `RELEASE.md` — not restated here.

## Decisions Made

See `DECISIONS.md` and `docs/decisions/` — not restated here.

## Active Blockers

- Project name **Kelvran** still needs a manual USPTO TESS / WHOIS trademark clearance before public registration of the domain/GitHub org/packages — tracked in `DECISIONS.md`, not yet done.
- Linter choices (Go: likely `golangci-lint`; Python: likely `ruff`) are still not configured — blocking `scripts/README.md`'s real `lint`/`verify` script implementations and `Makefile`'s real targets. Go/Python runtime versions themselves ARE now pinned (see `DECISIONS.md`'s latest entry) — this blocker is narrower than it was.

*(Nothing here is vulnerability-shaped; if a future blocker ever is, it routes to `SECURITY.md`'s private channel instead of appearing on this public page.)*

## Context for Next Session

Read `REPO_LAYOUT.md` first for the current directory map, then `docs/agents/MEMORY.md` and the tail of `docs/agents/LOGS.md` per `AGENTS.md`'s own "Always" rule. The two source-of-truth planning documents for everything created so far live outside this repo, in the parent workspace: `Not-Humans-World/ai-infra-research/naming-and-docs-plan.md` and `Not-Humans-World/ai-infra-research/kelvran-docs-addition-plan.md`.

## Last Updated

2026-09-02
