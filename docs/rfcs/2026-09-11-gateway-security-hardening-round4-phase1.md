# RFC: Round-4 upgrade, Phase 1 — gateway security hardening

## Status

Accepted, implemented 2026-09-11.

## Context

After `gateway/v0.8.0`, the user asked for 11 parallel cross-disciplinary `/deep-research` passes
covering gateway/cache/evals, saved at `docs/upgrade-research/*-round4-2026-09-11.md`, then asked
for the findings turned into a phased plan. This is Phase 1 of that plan (gateway security
hardening), the highest-priority phase since it's the cheapest, highest-value work.

The research's own top "build now" finding — that Guardrails don't scan tool-call RESULTS fed
back into a later turn (the InjecAgent/indirect-prompt-injection threat shape) — was re-verified
directly against the current code before being trusted, per this project's own repeated
doc-vs-code staleness gotcha (`AGENTS.md`'s Gotchas section). The claim is **false**:
`serializeMessages` (`gateway/internal/gateway/dataplane/dataplane.go`) does a full,
role-agnostic `json.Marshal` of the entire `req.Messages` slice, and `guardrail.Engine.Check`
(`gateway/internal/guardrail/engine.go`) is pure text/regex scanning with zero role awareness —
so a `role:"tool"` message is already scanned identically to any other message, by accident of
how the pre-call check is implemented, not because anyone designed it with that threat in mind.
This changed the item from "build a scanner" to "add the missing regression test, correct any
documentation that doesn't already say so."

Separately, wiring `govulncheck`/`pip-audit` into CI (both previously absent) surfaced a real,
independent finding: `gateway/go.mod`'s `go 1.25.0` directive matched CI's `setup-go@v5
go-version: "1.25"` pin, and Go 1.25 is now end-of-life (Go supports only the latest 2 major
releases; Go 1.27.0 shipped 2026-08-19, pushing 1.25 out of the supported window per
`https://go.dev/doc/devel/release`) — `govulncheck` found 6 real reachable stdlib CVEs
(GO-2026-5972, GO-2026-5026, and 4 others) against the resulting 1.26.5 toolchain, all fixed in
go1.26.6+.

## Design

**1. Regression test for tool-result scanning.** New
`TestHandleChatCompletionPreCallScansToolResultMessagesFedBackFromAnEarlierTurn`
(`gateway/internal/gateway/dataplane/guardrail_test.go`), sibling to the existing
`TestPromptVariablesInjectedPIIIsCaughtByPreCallGuardrail` (`prompt_test.go`). Constructs a
request whose `Messages` includes a `role:"tool"` message carrying a PII-shaped string as if it
were a tool-call result the caller's own application fed back from a prior turn (Kelvran never
executes tools itself), and asserts `HandleChatCompletion` returns `ErrGuardrailBlocked` with
zero upstream calls. No production code change — the test exists to prove an existing property,
not add a new one.

**2. `govulncheck` wired into gateway CI**, immediately after `golangci-lint`, matching the exact
`go run <module>@<pinned version>` pattern `go-arch-lint` already established:
`go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...`.

**3. `pip-audit` wired into evals CI**, immediately after `ruff`/`import-linter`. Deliberately
**not** `uvx pip-audit` (the pattern `ruff`/`import-linter` use) — `uvx` runs in a fully isolated
ephemeral environment with nothing else installed, so it would silently audit zero real
dependencies. Used `uv run --with pip-audit==2.9.0 pip-audit` instead, which overlays pip-audit
onto the project's own auto-synced venv.

**4. Go toolchain bumped `1.25.0` → `1.26.8`** across `gateway/go.mod`, both `setup-go@v5` pins in
`.github/workflows/ci.yml`, `gateway/Dockerfile`'s base image tag, and `gateway/ARCHITECTURE.md`/
`docs/operations/DEPLOY.md`'s prose (both said "1.25+", now structurally require 1.26.8 minimum).
Go's own `GOTOOLCHAIN=auto` mechanism downloads and switches to the exact required toolchain
automatically once `go.mod`'s directive is bumped.

**5. NIST AI 600-1 Crosswalk added to `THREAT_MODEL.md`**, mirroring the existing OWASP LLM Top
10 crosswalk's structure and "name what's real" discipline. Surfaced two real, previously-
undocumented gaps with no equivalent OWASP category and no existing mitigation: Dangerous/
Violent/Hateful Content and Obscene/Degrading/Abusive Content — Kelvran's guardrail detector set
(`policy.go`'s 3 Block-tier + 3 Warn-tier categories) was scoped exclusively to PII/secrets/
prompt-injection and was never intended as general content moderation. Named, not built — that
capability already exists upstream (provider moderation endpoints, safety-tuned models) and
duplicating it at the gateway layer is a large, separately-scoped effort.

## Alternatives considered

**Building a new tool-result-specific scanner** — rejected once direct code verification showed
the coverage already exists structurally; would have been redundant, cross-cutting work solving
an already-solved problem.

**`@latest` for `govulncheck`/`pip-audit`** — rejected, matching this project's own precedent
(`go-arch-lint@v1.18.0`) of pinning exact tool versions in CI for reproducibility.

**Content-moderation detectors for the two new NIST gaps found** — rejected for this phase: a
large, separately-scoped effort duplicating provider-side safety tuning, not a natural extension
of the existing PII/injection detector set. Named as a real gap, not silently left off the
crosswalk.

## Verification

New guardrail test passes; sanity-checked-by-breaking (temporarily made `serializeMessages` skip
`role:"tool"` messages, confirmed the test failed for the exact predicted reason, reverted).
`govulncheck` run locally post-bump: "No vulnerabilities found." `pip-audit` run locally via `uv
run --with pip-audit==2.9.0 pip-audit`: exit code 0, real `cachecontrol` warnings confirming a
real (not empty) environment was audited. Full gateway suite (`go build ./... && go vet ./... &&
gofmt -l . && go test ./... -race && golangci-lint run ./... && go run
github.com/fe3dback/go-arch-lint@v1.18.0 check && go mod tidy`) clean except the two pre-existing,
already-documented rootless-Docker failures. Full evals suite (`.venv/bin/python -m pytest
tests/ -q && ruff check . && uvx --from import-linter lint-imports`): 399 passed, 11 skipped,
ruff clean, 3/3 import-linter contracts kept. Docker base image tag verified real via a live
`docker pull golang:1.26.8-alpine`.
