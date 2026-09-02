# Dev Scripts

Index of the dev scripts this project will need. Everything below is currently a `Makefile` placeholder (echoes a pointer, exits 0) rather than a real script — writing a functional script now, before the actual Go/Python toolchain versions and lint configs are chosen, would risk shipping a wrong stub, which is worse than shipping no stub. This file is the single place that tracks what's blocking each one.

| Script (intended) | `Makefile` target | Status | Blocked on |
|---|---|---|---|
| Bootstrap both toolchains | `make setup` | Placeholder only | Go version pin, `uv` Python version pin — neither chosen yet |
| Lint `gateway/` + `evals/` | `make lint` | Placeholder only | Go linter choice (likely `golangci-lint`), Python linter choice (likely `ruff`) — neither configured yet |
| Run all tests | `make test` | Placeholder only | First real test files don't exist yet — see `docs/testing/TESTING.md` |
| Local CI-equivalent | `make verify` | Placeholder only | Depends on `lint` + `test` above, plus `buf breaking` once `api/` has real `.proto` files |

**The promise this file makes**: once `make verify` is real, it runs exactly what CI runs, in the same order — not an approximation. That's a commitment for when it's written, not a description of today's placeholders.

**Not scripted at all, deliberately**: release/publish steps (`RELEASE.md` already documents these as a manual runbook — automating them is a reasonable future step, but not before the runbook itself has been executed by hand at least once, so the steps are proven before they're encoded).
