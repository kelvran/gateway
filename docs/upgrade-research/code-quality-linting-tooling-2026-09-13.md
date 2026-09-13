# Code-Quality & Linting Tooling — Research (2026-09-13)

> **Recovery note:** the synthesizing subagent correctly declined to write this file itself, citing a "do not create doc files unless explicitly requested" operating rule — the parent session (which explicitly requested this exact deliverable) wrote it instead, from the workflow's own returned JSON result.

**Scope:** Code-quality and linting tooling across both deployables (`gateway/` Go, `evals/` Python), benchmarked against 2026 production Go/Python monorepo practice.

**Already shipped (not re-litigated):** `golangci-lint` v2.12.2 (CI-wired), `go-arch-lint` (dependency-direction enforcement), `govulncheck` (Go dependency CVE scanning), `gofmt`/`go vet`/`go mod tidy`, `ruff check` (Python lint), `import-linter` (`.importlinter` layer-contract enforcement), `pip-audit` (Python dependency CVE scanning).

## Findings

1. **`gosec` is a real, currently-missing, zero-new-binary addition — closes a gap `govulncheck` structurally cannot close.** `gateway/.golangci.yml`'s enabled-linter list is exactly `[errcheck, govet, staticcheck, unused, ineffassign, errorlint]` — `gosec` is absent, despite shipping bundled inside `golangci-lint` itself (a one-line `- gosec` addition, no new tool/dependency). `govulncheck` only flags known-CVE dependency functions reachable via call-graph analysis; `gosec` performs AST/SSA static + taint analysis on Kelvran's **own** source for SQL/command injection, SSRF, hardcoded credentials (G101), and weak crypto/TLS (G4xx) — none of which `govulncheck`'s known-vulnerability-database model can ever detect. Directly relevant here: `CLAUDE.md`'s own Hooks section names `gateway/internal/cache/` and `gateway/internal/identity/` as `THREAT_MODEL.md`'s two highest-priority-for-review subsystems — exactly what `gosec`'s taint/credential/crypto rules would scan. **`build_now`.**

2. **`bodyclose` is a second real, zero-new-binary addition, and the gap is not hypothetical.** Also bundled in `golangci-lint`. A direct grep for `http.Client`/`.Do(`/`http.Get`/`http.Post` found three real non-test call sites: `cmd/gateway/main.go`, `internal/guardrail/bedrockguard/bedrockguard.go`, `internal/gateway/dataplane/dataplane.go` — this is gateway's literal job (proxying HTTP calls to model providers), so an unclosed-body bug here would directly cause the connection-pool-exhaustion failure mode `bodyclose` exists to catch. **`build_now`.**

3. **`sqlclosecheck` is the same zero-cost addition, but currently inert — no live gap to close.** A grep for `database/sql`/`sql.Open`/`sql.DB` found zero matches anywhere in `gateway/`. Costs nothing to enable now (future-proofing) but isn't correcting a currently-live gap. **`not_yet`** — trigger: gateway/ actually adds a `database/sql`-based store.

4. **`AGENTS.md`'s own stated Python type-hint convention is completely unenforced.** Grepped `evals/pyproject.toml` and `.github/workflows/ci.yml` directly for `mypy`/`pyright`: zero matches in both. The real `evals` CI job runs only `ruff check`, `import-linter`, `pip-audit`, `pytest` — no type checker at all, despite `AGENTS.md`'s Conventions section explicitly stating type hints on all public functions. mypy's own docs recommend wiring into CI "as soon as possible" during adoption, with a documented incremental mechanism (per-module `ignore_errors=True`, no big-bang strict pass required day one). **`build_now`** (mypy, incremental per-module adoption).

5. **Astral's `ty` is explicitly not a substitute for mypy yet.** Astral's own Dec 2025 announcement frames `ty` as Beta, states Astral only just began recommending it for production use, and targets Stable "next year" pending completion of the typing-spec long tail. The live `ty` README (checked against tag `0.0.80`, five days before this research) states verbatim: "ty does not yet have a stable API; breaking changes... may occur between any two versions." **`not_yet`** — trigger: a Stable/1.0 `ty` release.

6. **Ruff already natively reimplements `bandit` (its `S`-prefixed ruleset) — no new tool needed, but the rule family isn't enabled, and it surfaces real signal.** `evals/pyproject.toml`'s actual `select` list is `[E, F, I, UP, B]` — no `S`. Running `uvx ruff check --select S .` against the real repo found 1,305 hits: 1,285 are `S101` (assert-in-test, a well-known false-positive class ruff's own docs recommend suppressing via per-file-ignores, mirroring this same `pyproject.toml`'s existing `tests/test_llm_judge_prompt_golden.py` E501 exemption) — but 2 genuine non-test findings remain: `evals/cli.py:2969-2975` (`S310`, `urllib.request.urlopen` on a webhook URL — audit for permitted schemes) and `evals/corpus_staleness.py:148-149` (`S603`/`S607`, `subprocess.run(["git", *args], ...)` — untrusted-subprocess-input pattern). Note: suppressions must use ruff's own `# noqa: S###`, not bandit's `# nosec` — ruff only lexically recognizes `# nosec` as a generic token today, it doesn't act on it. **`build_now`.**

7. **Pre-commit framework: a mixed bag, not a uniform yes.** No `.pre-commit-config.yaml` exists today (confirmed — the CI-only, catches-it-after-push model is accurate). `golangci-lint`'s own maintainer-shipped hook (`golangci-lint`, diff-based via `--new-from-rev HEAD --fix`, only modified files) is genuinely low-maintenance and complements rather than duplicates the full-module CI job — the maintainers explicitly warn their *other* hook (`golangci-lint-full`) is "for use if you run pre-commit in CI," i.e. the wrong one to add locally. By contrast, `pre-commit/mirrors-mypy`'s hook runs mypy from an isolated virtualenv lacking the project's real dependencies, requiring manually-maintained `additional_dependencies` that drift from `evals/pyproject.toml` over time — real, recurring upkeep for a single-maintainer repo whose CI already gates every push. **`build_now`** (golangci-lint's diff-based hook only) / **`not_yet`** (mypy pre-commit hook — trigger: a multi-contributor team where local front-running is worth the drift-maintenance cost).

8. **Commit-message linting is `not_yet`, for a reason specific to this repo.** `git log --oneline -15` shows every one of the 15 most recent commits already follows `type(scope): subject` exactly, with zero violations to correct. `AGENTS.md`'s own Boundaries section requires asking first before adding a new third language/runtime, citing `docs/decisions/0003-go-python-split.md`'s explicit rejection of doing so preemptively — and `commitlint` is a Node package, which would cross that named boundary purely to formalize a convention already followed with 100% observed compliance. If mechanical enforcement is ever wanted, a same-language (shell/Go) commit-msg git hook is the lower-cost path that doesn't cross the boundary — but even that has no active trigger today. **`not_yet`.**

## Caveats

All repo-ground-truth claims were independently re-verified directly against the live files (`gateway/.golangci.yml`, `evals/pyproject.toml`, `.importlinter`, `.github/workflows/ci.yml`, `AGENTS.md`, `git log`), including two live commands beyond the base research (`grep` for `http.Client` usage; `uvx ruff check --select S .`) specifically to confirm the tools would fire on real code here, not just that they exist. `gateway/.go-arch-lint.yml` was out of scope for this pass. The `sqlclosecheck` and commit-linting findings rest on absence-of-evidence greps that a future change could invalidate.

## Open Questions

- Does `gateway/`'s cache layer use any SQL-backed store not matched by a plain `database/sql` grep (e.g. a vendored Postgres client), which would upgrade `sqlclosecheck` from `not_yet` to `build_now`?
- Should mypy's initial per-module allowlist start from `evals/models.py`/`evals/stats.py` (already-modern-idiom files per `AGENTS.md`'s own ruff-`UP`-rule comment), or from the newest/smallest modules to minimize first-pass friction?
- Are the `S310`/`S603`/`S607` findings genuinely benign, and if so should they get narrow `# noqa` suppressions with a comment (mirroring this repo's own existing narrow-suppression pattern) rather than a blanket ignore?
- Given the ask-first boundary on new languages/runtimes, is there appetite for a non-Node commit-msg hook if violations ever start occurring, or should this stay purely discipline-based indefinitely?

## Synthesis: build_now vs not_yet

| Item | Verdict | Why |
|---|---|---|
| `gosec` (golangci-lint) | **build_now** | Zero new binary; closes a real code-level security-scan gap `govulncheck` cannot |
| `bodyclose` (golangci-lint) | **build_now** | Zero new binary; 3 real HTTP call sites confirmed |
| `sqlclosecheck` (golangci-lint) | **not_yet** | No `database/sql` usage exists anywhere yet |
| mypy (incremental, evals) | **build_now** | Closes a documented-but-unenforced convention gap |
| Astral `ty` | **not_yet** | Beta, 0.0.x versioning, no stable API |
| ruff `S` ruleset | **build_now** | Native to ruff already; 2 real non-test findings confirmed |
| pre-commit: golangci-lint hook | **build_now** | Diff-based, low-maintenance, maintainer-recommended for local use |
| pre-commit: mypy hook | **not_yet** | Real ongoing virtualenv-drift maintenance cost, no multi-contributor trigger |
| commitlint / commit-msg linting | **not_yet** | Zero violations today; would cross the ask-first new-runtime boundary for no benefit |
