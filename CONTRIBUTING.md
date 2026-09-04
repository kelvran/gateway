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

**Cross-language contract (`api/`):** once `.proto` files exist, run `buf lint` and `buf breaking --against '.git#branch=main'` before opening a PR that touches anything under `api/`.

*(This section will be filled in with exact, verified commands once each deployable is actually scaffolded — right now it describes the intended shape per each deployable's `ARCHITECTURE.md`, not a tested setup.)*

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

Real and running (`.github/workflows/ci.yml`, green on every push to `main` — see `STATUS.md`): `golangci-lint run ./...` + `go build ./... && go test ./...` for `gateway`; `ruff check .` + `uv run pytest tests/` for `evals`; `buf lint`/`buf breaking` + generated-code drift check for any `api/` change. Mirrors root `make verify` exactly. Full test-pyramid strategy (unit/integration/contract/e2e/load/chaos/fuzz): `docs/testing/TESTING.md`.
