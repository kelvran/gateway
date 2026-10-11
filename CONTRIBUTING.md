# Contributing

## Dev Setup

**`gateway/` (Go):**
```
cd gateway
go build ./...
go test ./...
```

**`evals/` (Python):**
```
cd evals
uv sync
uv run pytest
```

**Cross-language contract (`api/`):** `api/gatewayevents/v1/gatewayevents.proto` is the contract (since 2026-09-03). Run `make lint-proto` — `buf lint` and `buf breaking --against '../.git#branch=main,subdir=api'` from `api/` — before opening a PR that touches anything under `api/`.

*(Commands above re-verified against `.github/workflows/ci.yml` and the root `Makefile` on 2026-10-10.)*

## Branching / PR Conventions

- Branch from `main`: `feat/description` or `fix/description`.
- Keep branches short-lived — merge within days, not weeks.
- Full branch/tagging strategy, including per-deployable release mechanics: `docs/development/BRANCHES.md`.
- Conventional commit format: `<type>(<scope>): <description>` (types: feat, fix, refactor, docs, test, chore, perf, ci, build, revert). Scope should generally be `gateway`, `evals`, `cache`, `api`, or a specific subsystem within one of those.
- PR description: summary bullets + a test plan. Link to a `docs/rfcs/` entry if one exists for the change; link to a `docs/decisions/` ADR if the PR implements or revisits a foundational decision.

## Design-Review Gate

Not every change needs the same ceremony:

- **Major feature or architectural change** (a new subsystem, a change to the `api/` contract, anything that would move a decision already recorded in `docs/decisions/`): open a tracking issue first, and write a `docs/rfcs/` entry (copy `docs/rfcs/TEMPLATE.md`) before implementation starts.
- **Smaller change**: an issue with an appropriately-sized label is enough — no RFC file required.
- **Trivial fix** (typo, obvious bug, single-line change): no issue required, just open the PR.

## Code Style

See `AGENTS.md` for the authoritative Go/Python conventions — this file doesn't duplicate them, only points here.

## CI Gates

Real and running (`.github/workflows/ci.yml`, green on every push to `main` — see `STATUS.md`): `golangci-lint run ./...` + `go build ./... && go test ./...` for `gateway`; `ruff check .` + `uv run pytest tests/` for `evals`; `buf lint`/`buf breaking` + generated-code drift check for any `api/` change; `cd gateway/compat && go test ./...` for the SDK compatibility matrix (the `compat` job, since 2026-10-11: the official OpenAI and Anthropic SDKs, Go and Python, against a gateway built from the tree). Root `make verify` runs the fast local subset; `AGENTS.md`'s Testing section lists the eight CI steps it deliberately omits (and, since 2026-10-11, the `compat` module). Full test-pyramid strategy (unit/integration/contract/e2e/load/chaos/fuzz): `docs/testing/TESTING.md`.

## Tests Are Part of the Change

Every pull request that adds or changes behaviour adds or updates the tests that pin it; a bug fix includes a regression test that fails on the unfixed code. Reviewers may ask for the failing-first run as evidence. This is the repo's standing practice (see `docs/testing/TESTING.md`), written down here so the OpenSSF Best Practices `test_policy` criterion has something to point at.

## Developer Certificate of Origin

Every commit must be signed off (`git commit -s`), certifying the [Developer Certificate of Origin 1.1](https://developercertificate.org/): you wrote the change or have the right to submit it under this repository's Apache-2.0 licence. There is no CLA. Pull requests with unsigned commits are rejected by the DCO check once it is enabled on the repository (tracked in `docs/agents/LOGS.md`'s 2026-10-08 entry).
