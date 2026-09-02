# Session Logs

Append-only. Newest entry at the bottom. Never edit a past entry — if something in it turns out to be wrong, add a new entry that says so.

**Entry format:**
```
## [YYYY-MM-DD] branch/PR — commit range
Files touched:
Intent/summary:
Decisions made: (link to DECISIONS.md/docs/decisions/ if any)
Next steps / resume point:
```

---

## [2026-09-02] pre-scaffolding — no branch/PR yet, direct working-tree changes

**Files touched:** entire initial documentation set — `LICENSE`, `PRD.md`, `DESIGN.md`, `ARCHITECTURE.md`, `docs/decisions/0001-0003`, `DECISIONS.md`, `gateway/ARCHITECTURE.md`, `evals/ARCHITECTURE.md`, `docs/PROVIDERS.md`, `REPO_LAYOUT.md`, `THREAT_MODEL.md`, `SECURITY.md`, `SECURITY-INSIGHTS.yml`, `AGENTS.md`, `CLAUDE.md`, `docs/agents/MEMORY.md` (this file's sibling), `api/README.md`, plus directory skeleton (`gateway/changelog/`, `evals/changelog/`, `docs/decisions/`, `docs/rfcs/`, `docs/agents/archive/`, `api/otel/`, `api/gatewayevents/`) and their `unreleased.md`/`TEMPLATE.md` contents.

**Intent/summary:** Created the complete pre-scaffolding documentation set for a new AI infrastructure project, before any source code exists. This followed extensive prior research (in the parent workspace's `Not-Humans-World/ai-infra-research/`) covering: (1) independent deep-dives into an AI Gateway, an AI Cache, and an AI Evals system; (2) a decision on whether to build them as one project or several (answer: one monorepo, two deployables); (3) two rounds of naming research; (4) a research pass on what documentation a serious infra project needs before scaffolding, reconciled against real conventions from Envoy/Kong/vLLM/Temporal/LiteLLM/TensorZero/Langfuse.

**Decisions made:** The three foundational architecture decisions were formalized as ADRs (`docs/decisions/0001-0003`) rather than made fresh in this session — see `DECISIONS.md` for the terse log and `DESIGN.md` for the full prose rationale. No new architectural decisions were made in this session beyond what was already researched; this was a documentation-authoring pass, not a design pass.

**Next steps / resume point:** Remaining phases per the approved plan (`/Users/sairamugge/.claude/plans/snuggly-exploring-finch.md`): Phase 7 (CONTRIBUTING.md, CODE_OF_CONDUCT.md, CODEOWNERS, RELEASE.md, UPGRADE.md, DEPRECATED.md) and Phase 8 (README.md, written last since it links everything above). Once all documentation is complete, actual code scaffolding (go.mod, pyproject.toml, first Phase-0 MVP code per each deployable's ARCHITECTURE.md) is the next real milestone — not started yet, and explicitly out of scope for this session per the user's own instruction not to scaffold code yet.

---

## [2026-09-02] pre-scaffolding — same session, completion

**Files touched:** Phase 7 (`CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `CODEOWNERS`, `RELEASE.md`, `UPGRADE.md`, `DEPRECATED.md`) and Phase 8 (`README.md`) — the two phases flagged as "next steps" above.

**Intent/summary:** Completed the full pre-scaffolding documentation set — all 30 planned files/folders now exist. Verified: file count matches plan (31 files including `docs/agents/archive/.gitkeep`), `api/otel/` and `api/gatewayevents/` are correctly empty placeholders, `docs/agents/MEMORY.md` is 18 lines (well under its 200-line cap), and every `.md` cross-reference spot-checked in `THREAT_MODEL.md`/`SECURITY.md`/`ARCHITECTURE.md` resolves to a real file in the tree.

**Decisions made:** None new — this entry is documentation-completion only.

**Next steps / resume point:** Documentation phase is complete. The next real milestone is code scaffolding: `go.mod`/`cmd/gateway` skeleton per `gateway/ARCHITECTURE.md`'s package layout, and `pyproject.toml`/initial `evals/` package per `evals/ARCHITECTURE.md` — neither has been started, and both were explicitly out of scope for this session. Before starting that work, read `AGENTS.md`'s Boundaries section and re-check `docs/agents/MEMORY.md` for anything added since this entry.

---

## [2026-09-02] second documentation batch — no branch/PR yet, direct working-tree changes

**Files touched:** relocated `docs/PROVIDERS.md` → `docs/operations/PROVIDERS.md` (+ 3 inbound reference fixes in `SECURITY.md`/`README.md`/`THREAT_MODEL.md`); created `docs/development/BRANCHES.md`, `Makefile`, `scripts/README.md`, `docs/agents/ETHOS.md`, `docs/agents/AGENTS_LEARNING.md`, `docs/plans/TEMPLATE.md`, `docs/research/RESEARCH.md`, `docs/testing/TESTING.md`, `docs/operations/DEPLOY.md`, `docs/operations/TELEMETRY.md`, `RELEASE_NOTES.md`, `docs/users/USER_GUIDE.md`, `SUPPORT.md`, `STATUS.md`; edited `AGENTS.md` (frontmatter, one new Boundaries "Never" bullet, two pointer links), `REPO_LAYOUT.md` (tree refresh + `CODE_MAP.md` deferral note), `CONTRIBUTING.md` (two pointer links), `README.md` (doc-index refresh), `DECISIONS.md` (two new one-line entries).

**Intent/summary:** Second documentation batch, following a dedicated deep-research pass (6 parallel workflow angles + synthesis, grounded against real local templates in the user's own MindForge and ContextOS projects plus the installed `obra/superpowers` plugin, saved to the parent workspace's `ai-infra-research/kelvran-docs-addition-plan.md`) covering ~22 additional doc/artifact types the user asked about. Every item resolved to one of three outcomes: already covered (no action — `agents.md`, `decisions.md`, `LICENSE`, `security.md`), folded into an existing/different file rather than a new standalone one (Context.md → AGENTS.md frontmatter; soul.md → toned-down `docs/agents/ETHOS.md` + one AGENTS.md Boundaries bullet; anti-patterns.md → §5 of the new AGENTS_LEARNING.md; code-base_map.md → deferred, one sentence in REPO_LAYOUT.md; "ADR for each" → zero new ADR files, two items get DECISIONS.md lines instead), or a genuinely new file (14 total, listed above).

**Decisions made:** Two new terse `DECISIONS.md` entries (branch strategy is main-only, not MindForge's develop→release→main model; contract-testing tooling is `buf breaking` + golden fixtures, not a full Pact Broker). No new ADRs — both real ADR-candidate topics (semantic-cache risk-gating, skeptic-panel protocol) were already deferred to `docs/rfcs/` by `PRD.md`/`DESIGN.md` before this session, and remain so.

**Verification performed:** full file listing confirmed 45 files (31 from batch 1 + 14 new in batch 2, `PROVIDERS.md` relocated not duplicated); confirmed no standalone `ANTI-PATTERNS.md`/`CONTEXT.md`/`CODE-BASE_MAP.md` exist and `docs/decisions/` still holds exactly 3 ADRs; cross-reference grep across the new/edited files found no real broken links (several flagged matches were false positives — intentional shorthand prose and cross-workspace pointers to the parent `ai-infra-research/` folder, not broken in-repo links).

**Next steps / resume point:** Both documentation batches are now complete — 45 files, zero source code. The next real milestone is code scaffolding, same as noted in the entry above: `go.mod`/`cmd/gateway` per `gateway/ARCHITECTURE.md`, `pyproject.toml`/`evals/` per `evals/ARCHITECTURE.md`. `STATUS.md` is now the canonical live dashboard — check it first when resuming, then `docs/agents/MEMORY.md` and this file's tail.

---

## [2026-09-02] initial code scaffolding — spec → plan → implementation, no branch/PR yet

**Files touched:** created `docs/rfcs/2026-09-02-initial-code-scaffolding.md` (spec) and `docs/plans/2026-09-02-initial-code-scaffolding.md` (task-by-task plan, superpowers-style, no `-design`/`-implementation` filename suffix per the verified convention); implemented Phase 1 (Gateway, Go — 9 tasks) and Phase 2 (Evals, Python — 6 tasks) in full; updated `gateway/changelog/unreleased.md`, `evals/changelog/unreleased.md`, and `DECISIONS.md` per the plan's Post-Implementation checklist.

**Intent/summary:** First real code in this repository. Followed the spec→plan→implement pipeline the user asked for explicitly: an RFC scoping exactly what's real vs. intentionally stubbed in this pass (narrower than `PRD.md`'s full Phase 0 feature list — a genuine skeleton, not a feature-complete Phase 0), a detailed plan with per-task file lists and verify commands, then two parallel implementation agents (one per deployable, fully independent — `gateway/` and `evals/` share no code, only the not-yet-real `api/` contract). Both agents were required to run real build/test commands after every task and report actual output, not claims.

**Decisions made:** One new `DECISIONS.md` entry resolving the RFC's version-pin open question (Go 1.25 language level / 1.26.5 toolchain; Python `>=3.12` / uv-resolved 3.13.12 venv). No architectural decisions were revisited — this session implemented what `gateway/ARCHITECTURE.md`/`evals/ARCHITECTURE.md` already specified, it didn't redesign anything.

**Verification performed:** independently re-ran both agents' final verification commands myself rather than trusting their reports alone — `cd gateway && go build ./... && go vet ./... && go test ./...` (all packages `ok`, vet clean) and `cd evals && uv run pytest tests/` (30 passed, 4 skipped — the Docker-sandbox integration tests, skip-by-default as designed). Also read the three most security/correctness-sensitive files directly (`identity.go`'s constant-time key comparison, `cache/key.go`'s SHA-256 key fabrication, `stats.py`'s Wilson interval formula) rather than trusting the agents' self-report — all three are genuinely correct, not hand-waved. The gateway agent additionally verified a real Docker build and a manual smoke test (401 on missing auth header, from both the local binary and the built container). The evals agent additionally verified the Docker-sandbox integration tests against a real local Docker daemon (network-egress blocking, timeout enforcement, exit-code propagation) — all 4 passed.

**What's real vs. stubbed (see `docs/rfcs/2026-09-02-initial-code-scaffolding.md` for the authoritative list):** Real — OpenAI + Anthropic adapters, Cache L1 exact-match, in-memory rate limiting, single static-key auth, non-streaming dataplane pipeline, structured JSON logging + cost calc (gateway); `EvalCase` model, Wilson CI, deterministic + LLM-judge scorers (judge logic real, provider call injected/mocked), Docker-sandboxed rollout wrapper, CLI (evals). Stubbed with typed errors, never fake success — Gemini/Bedrock/OpenAI-compat adapters, the dormant `grpcserver`/`grpcclient` cache-extraction seam. Not built at all yet — streaming, distributed rate limiting, Decimal cost accounting, MCP/A2A, guardrails, the `api/` contract itself, rollout scheduling, trace collection.

**Next steps / resume point:** `STATUS.md` needs updating to reflect this (done in this same session, check its "Last Updated"). The next natural `docs/plans/` entry is whichever of streaming support or distributed rate limiting gets picked first — flagged as an open question in the RFC, not decided here. Read `AGENTS.md`'s Boundaries and this entry's "What's real vs. stubbed" list before assuming any capability works.

---

## [2026-09-02] branch-strategy reopened + git init — first commit

**Files touched:** rewrote `docs/development/BRANCHES.md` (added "Why Not GitFlow" / "Why Not Pure Trunk-Based Either" sections, made the hotfix escape valve's Release-Flow/Branch-for-Release discipline explicit, split the single revisit trigger into two); appended one new `DECISIONS.md` line (did not edit the prior branch-strategy line, per that file's own append-only rule); created `.gitignore`; ran `git init` for the first time in this repository's life and made the first commit.

**Intent/summary:** The founder asked to reopen the branch-strategy decision, offering a `main`(release)+`develop`(integration)+feature-via-`develop` model as a candidate — explicitly framed as "a reference," with instructions to do real end-to-end research rather than adopt it or the prior decision by default. Ran a 3-angle research pass (GitFlow precedent, trunk-based/GitHub Flow precedent, hybrid/multi-component-monorepo precedent) plus synthesis, deliberately timed to land *before* `git init` so the repo starts with the right structure rather than needing a later migration. Full research trail saved to the parent workspace's `ai-infra-research/branch-strategy-trunk-based.md` and `branch-strategy-hybrid-models.md`.

**Decisions made:** The suggested GitFlow-style model was explicitly **not adopted** — reaffirmed trunk-based (`main`-only), with two concrete refinements: (1) the `release/<deployable>-vX.Y` hotfix escape valve now has real Release-Flow/Branch-for-Release discipline (cherry-pick forward only, never merge back) where it previously just existed as a named-but-unspecified branch; (2) the single revisit trigger became two distinct triggers (organizational — unchanged from ADR-0001; and a new support-policy trigger tied to concurrent multi-version production support). See the new `DECISIONS.md` line and the rewritten `docs/development/BRANCHES.md`.

**Verification performed:** confirmed via `git status` before this session that no `.git` directory existed anywhere in the tree (a genuine first init, not a re-init). `.gitignore` covers Go build artifacts, Python `.venv`/`__pycache__`/`.pytest_cache`, and local secrets (`.env*`, `config.yaml` while explicitly un-ignoring `config.example.yaml`).

**Next steps / resume point:** Same as the prior entry — streaming (SSE) support is the confirmed next priority, per the founder's explicit choice. It should get its own RFC/plan pair under `docs/rfcs/`/`docs/plans/`, following the same pipeline used for the initial scaffolding, not be built ad hoc directly on `main`.

---

## [2026-09-02] test suite deepened + real tooling wired — before moving to streaming

**Files touched:** two parallel agents (one per deployable) added: `gateway/cmd/gateway/integration_test.go` (real HTTP integration tests), `gateway/internal/adapter/{openai,anthropic}/testdata/` + `regression_test.go` (golden wire-format fixtures), `gateway/internal/cache/key_fuzz_test.go` + `gateway/internal/gateway/controlplane/config_fuzz_test.go` (fuzz targets), cache/ratelimit benchmarks, `gateway/.golangci.yml`; `evals/tests/test_cli_integration.py`, `evals/tests/test_stats_properties.py` (Hypothesis), `evals/tests/test_llm_judge_prompt_golden.py`, `evals/tests/test_sandbox_error_paths.py`, a real `[tool.ruff]` config in `evals/pyproject.toml`. I then made the `Makefile` targets real (`setup`/`lint`/`lint-gateway`/`lint-evals`/`test`/`test-gateway`/`test-evals`/`verify`), added `.github/workflows/ci.yml` running the same checks, updated `scripts/README.md` and `AGENTS.md`'s Testing section to describe reality instead of placeholders, and updated `DECISIONS.md`.

**Intent/summary:** The founder explicitly asked to test the previous scaffolding end-to-end (unit, integration, regression, "each and every script") before moving on to the next feature (streaming). Ran a 2-agent parallel workflow, each required to actually run every command and report real output — same discipline as the initial-scaffolding pass.

**Decisions made:** Lint tooling picked and wired for real (`golangci-lint` v2, `ruff`) — see the new `DECISIONS.md` line. No architectural decisions revisited.

**Verification performed:** independently re-ran `go build/vet/golangci-lint/test` and `uv sync/pytest/ruff` myself after both agents finished — both clean, matching their reports exactly. Directly read three of the more novel additions (`.golangci.yml`'s exclusion-rule justification, `FuzzKey`'s property documentation, `test_stats_properties.py`'s five Hypothesis invariants) rather than trusting the reports alone — all genuinely sound, not hand-waved. Ran `make verify` end-to-end myself after writing the real Makefile — passes cleanly for both deployables.

**Bugs found:** none in production behavior, in either deployable. Both agents' fixes were lint-driven idiom changes (explicit error-discards, a `TimeoutError` alias modernization, a test-clarity fix for a staticcheck false positive) with zero behavioral change — each explicitly confirmed via the full test suite before and after.

**Next steps / resume point:** Test/tooling deepening is complete and independently verified. Streaming (SSE) support is next — gets its own RFC/plan pair under `docs/rfcs/`/`docs/plans/`, per the founder's explicit sequencing request (test fully, then move on).

---

## [2026-09-02] streaming (SSE) support — spec → plan → implementation, no branch/PR yet

**Files touched:** `docs/rfcs/2026-09-02-streaming-support.md` (spec, pre-existing this session) and `docs/plans/2026-09-02-streaming-support.md` (task-by-task plan, pre-existing this session); implemented all 5 tasks: created `gateway/internal/streaming/{types,reader,writer}.go` + tests; created `gateway/internal/adapter/openai/stream.go` + tests/fixtures; created `gateway/internal/adapter/anthropic/stream.go` + tests/fixtures; modified `gateway/internal/adapter/openai/openai.go` (`stream_options.include_usage`); created `gateway/internal/gateway/dataplane/{streaming,streamaccumulator}.go` + tests; modified `gateway/internal/gateway/dataplane/dataplane.go` (optional `Config.UpstreamStream`, `NewHTTPUpstreamStreamCaller`); modified `gateway/cmd/gateway/main.go` (streaming branch, error-status mapping, updated package doc comment); modified `gateway/cmd/gateway/integration_test.go` (3 new real end-to-end streaming scenarios: OpenAI success, Anthropic success, cache-hit fake-stream, unsupported-provider 400 — 4 total, one shared across OpenAI/Anthropic); updated `gateway/ARCHITECTURE.md`, `gateway/changelog/unreleased.md`, `DECISIONS.md`, `STATUS.md`.

**Intent/summary:** Implemented real SSE streaming end-to-end for the gateway's two real provider adapters, following the same spec→plan→implement pipeline as the initial scaffolding. Task 1 (shared `streaming` package) was written directly rather than delegated, since it's the fixed interface both provider decoders implement against. Tasks 2/3 (OpenAI/Anthropic decoders) ran as a parallel dynamic-workflow pair. Task 4 (dataplane integration + integration tests) was implemented directly, including a bug found via my own direct code review rather than a subagent report (see below). Task 5 (this entry + doc/changelog wrap-up) closes out the pass.

**Decisions made:** See the new `DECISIONS.md` line above — unsupported-provider fails typed rather than silently degrading; the fallback-before-first-byte rule now applies identically to streaming.

**Bugs found:** One real bug, found by direct code review (not a subagent's self-report): the Anthropic `StreamDecoder`'s `decodeMessageDelta` parsed `usage` but silently discarded `delta.stop_reason` — every reconstructed Anthropic streamed response would have come back with an empty `FinishReason` regardless of how the model actually stopped. Verified this was real (not a false positive) by grepping the test fixtures for genuine `stop_reason` values and confirming zero existing test assertions on `FinishReason` anywhere in the anthropic stream test file. Fixed by adding a `finishReasonFromStopReason` mapping and emitting a `FinishReason`-carrying chunk from `message_delta`; this correctly broke 3 pre-existing hard-coded chunk-count assertions, all fixed and now passing, plus one new dedicated regression test (`TestStreamDecoder_MessageDeltaCarriesFinishReason`) added across all 4 fixtures.

**Verification performed:** `cd gateway && go build ./... && go vet ./... && go test ./... && golangci-lint run ./...` — all packages `ok`, `0 issues`, run independently by me after every file addition, not just at the end. Directly read (not just trusted) the riskiest new code: the Anthropic decoder's stateful block-index tracking, the dataplane's tee-to-client-and-accumulator loop, and the fallback-before-first-byte gating logic. The integration suite drives 8 tests total against real `httptest.Server`s with real net/http clients — including a real Anthropic-shaped SSE mock upstream (typed `event:`/`data:` frames, no `[DONE]` sentinel, matching the real API) proving the streaming wiring is genuinely provider-agnostic, not accidentally OpenAI-shaped.

**Next steps / resume point:** Streaming is complete, tested, and documented, but not yet committed (this entry's changes are still uncommitted working-tree state as of writing). After committing: `STATUS.md`'s "Next Action" is open again — distributed (Redis-backed) rate limiting was the other RFC-flagged candidate, but nothing beyond streaming has been decided as the next priority; check with the founder before starting new feature work.
