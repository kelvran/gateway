# Dev Scripts

Index of the dev scripts this project uses. As of the test-expansion pass (see `docs/agents/LOGS.md`), all four `Makefile` targets below are real, run against real toolchains (`golangci-lint` 2.12.2, `ruff` 0.15.9), and are wired into `.github/workflows/ci.yml` — the promise this file made when the targets were placeholders ("`make verify` will run exactly what CI runs, in the same order") is now kept, not aspirational.

| Script | `Makefile` target | Status | Notes |
|---|---|---|---|
| Bootstrap both toolchains | `make setup` | **Real** | `go mod download` (gateway) + `uv sync` (evals) |
| Lint `gateway/` + `evals/` | `make lint` (or `lint-gateway`/`lint-evals` individually) | **Real** | `golangci-lint run ./...` against `gateway/.golangci.yml`; `ruff check .` against `evals/pyproject.toml`'s `[tool.ruff]` |
| Run all tests | `make test` (or `test-gateway`/`test-evals` individually) | **Real** | `go build ./... && go test ./...`; `uv run pytest tests/` — includes unit, integration, regression/golden, fuzz, and property-based (Hypothesis) tests. Docker-requiring sandbox integration tests are skip-by-default (`RUN_DOCKER_TESTS=1` to opt in), per `docs/testing/TESTING.md`. |
| Local CI-equivalent | `make verify` | **Real**, matches CI | `lint` + `test` for both deployables. CI (`.github/workflows/ci.yml`) runs the same checks per-deployable in parallel jobs, plus `go test -race` (not run locally by `make verify` by default — run `cd gateway && go test ./... -race` directly if you want the race detector before pushing). |

**Still not real, deliberately:** `buf breaking` — no `api/*.proto` files exist yet (see `api/README.md`); this gets wired into `make verify`/CI the moment the first real contract file lands, not before.

**Not scripted at all, deliberately:** release/publish steps (`RELEASE.md` documents these as a manual runbook — automating them is a reasonable future step, but not before the runbook itself has been executed by hand at least once, so the steps are proven before they're encoded).
