- **Status**: accepted
- **Date**: 2026-09-04
- **Author(s)**: project founder + Claude Code

## Summary

Build `evals/`'s first genuinely end-to-end path: a `Run` data model, a sequential Rollout Scheduler that executes an `EvalCase` through the existing real `run_in_sandbox()` sandbox wrapper, an append-only JSONL Results Store, and a new `evals rollout` CLI command that scores each `Run` with the existing real deterministic scorer and prints the existing real Wilson confidence interval. This is the first time a "case → real execution → scored, persisted result" path has existed anywhere in `evals/` — every piece it's built from (`run_in_sandbox`, `exact_match`/`regex_match`, `wilson_interval`) already exists and is already tested in isolation; none of them are wired together today.

## Motivation

Confirmed directly against the live tree, not assumed: `evals/ARCHITECTURE.md`'s Data Model sketch (lines 58-70) names five entities — `EvalCase`, `Run`, `Trace`, `Span`, `Score`. Only `EvalCase` exists as code (`evals/models.py:24-46`); `grep -rn "class Run\b\|class Trace\b\|class Span\b\|class Score\b\|harness_config" evals/evals/ evals/tests/` returns zero hits. `evals/evals/rollout/sandbox.py`'s `run_in_sandbox()` is a real, working, well-tested Docker wrapper — but it has exactly zero callers outside its own two test files (`test_sandbox_error_paths.py`, `test_sandbox_integration.py`); `cli.py` never imports `evals.rollout` at all. `PRD.md`'s Success Metrics section requires "cost savings attributable and explainable down to the individual agent run" — there is no code path today, for anything `evals` itself executes, that produces a `Run` to attribute anything to.

This gap was named honestly at scaffolding time (`docs/rfcs/2026-09-02-initial-code-scaffolding.md:45`: "Rollout scheduler, trace collector, full CI/CD gate tiers — no code yet; Phase 1+") — it is not a live documentation-vs-code integrity violation the way Guardrails was for `gateway`. But it has since fallen off the *operative* backlog: `STATUS.md`'s "Next Action" candidate list, refreshed after six shipped gateway features, never mentions it. This RFC re-surfaces it as the next real `evals` feature, grounded via a dynamic-workflow research pass (4 parallel angles — a full code-audit of current `evals/`, industry precedent for this exact orchestration layer, a doc-vs-code gap scan, and a scope/risk assessment of the candidate slices — plus a synthesis that independently re-verified every load-bearing claim against the live repo, including re-running the test suite itself). `PRD.md`'s v1 scope-out list (Scope section) does not name any of {Rollout Scheduler, Trace Collector, real LLM-judge provider wiring, Results Store, dataset-promotion} — this remains in-scope by omission, just unbuilt.

The research also surfaced, and this RFC deliberately does **not** attempt to fix (see Alternatives Considered and Unresolved Questions): a real, live doc-vs-code gap in `THREAT_MODEL.md`'s Evals STRIDE table (Denial of Service / Information Disclosure / Elevation of Privilege rows claim mitigations — tiered CI gates, network egress allowlisting, scoped per-tool credentials, five-layer sandbox defense-in-depth — none of which have any code behind them), structurally identical to the Guardrails-row gap already found and fixed in the same document for `gateway`. That is a separate, larger security-hardening unit of work, not a byproduct of building a scheduler.

## Detailed Design

### Grounding

Grounded via a dynamic-workflow research pass (task `wnejahal7`, run `wf_18c6938f-277`): a code-audit angle (full call-graph trace of `evals/`, confirming `EvalCase → deterministic scorer → Wilson CI` is the only wired path today, and that `run_in_sandbox`/`decode_gateway_decision_event`/`judge()` are each real-but-disconnected islands), a precedent angle (Inspect AI and promptfoo both ship exactly Kelvran's filesystem-only, no-DB/no-MQ posture as their *default production mode*, not a fallback — promptfoo's SQLite `evals`/`evalResultsTable` schema is architecturally the closest precedent to `ARCHITECTURE.md`'s own "trace store distinct from dataset store, joinable by ID" data model, translated from embedded-DB rows into flat files), a docs-gap-scan angle (classified every evals-related doc claim into deferred-with-reason / live-gap / genuinely-next-in-line), and a shape-and-risk angle (design the minimal, one-RFC-sized slice, naming the scope traps to cut). The synthesis independently re-read every file cited, re-ran `uv run pytest tests/ -q` itself (confirmed `45 passed, 4 skipped`, matching the code-audit's number exactly), and re-confirmed `PRD.md`'s scope-out list has no overlap with this feature.

### v1 scope: `Run` model, sequential scheduler, JSONL results store, one new CLI command

**`Run` model** (`evals/evals/models.py`, alongside `EvalCase`, same `frozen=True`/no-in-place-mutation convention):

```python
RunStatus = Literal["completed", "timed_out", "error"]

class Run(BaseModel):
    model_config = ConfigDict(frozen=True)

    id: str
    eval_case_id: str
    eval_case_revision: int
    harness_config: dict  # v1 contract: {"image": str, "command": list[str], "timeout_s": int}
    status: RunStatus
    exit_code: int | None = None
    stdout: str = ""
    stderr: str = ""
    latency_ms: float
    cost_usd: float | None = None  # None = not measured — v1's sandbox harness makes no billed call
    error: str | None = None       # populated only when status == "error"
```

Deliberate, decided departures from `ARCHITECTURE.md`'s full sketch (`sandbox_id`, `scaffold_version`, `tool_budget`, `retry_policy`, `step_budget`, `token_usage`) — every field on `Run` above is either real today or an honest "not measured" `None`, never a fabricated placeholder:

- `cost_usd: float | None = None`, not `0.0`. `run_in_sandbox` runs a Docker command, not an LLM call — reporting `0.0` would read as "measured and confirmed zero," which is false. `None` is the honest signal, resolving this RFC's own open question decisively rather than leaving it ambiguous.
- `harness_config` carries exactly `{image, command, timeout_s}` — the literal, real sandbox invocation — not the full `scaffold_version`/`tool_budget`/`retry_policy`/`step_budget`/`sandbox_tier` set `ARCHITECTURE.md` sketches for a future pluggable multi-step agent harness that doesn't exist yet. Recording fields for behavior that isn't enforced (or doesn't exist) would be exactly the kind of "documented but not real" claim this project has been correcting all session.
- No `Trace`/`Span` field at all, not even an empty placeholder. `api/otel`'s transport is explicitly undecided (`evals/ARCHITECTURE.md:20`); adding a stub `Trace{spans: []}` field would be dead code with no consumer, contradicting the org-wide YAGNI convention. `Trace`/`Span` are added when real span capture lands, not before.

**Scheduler** (`evals/evals/rollout/scheduler.py`, new):

```python
async def run_suite(cases: list[EvalCase]) -> list[Run]:
    """Sequentially execute each EvalCase's {image, command} through
    run_in_sandbox(), producing one Run per case. Never raises on a single
    case's failure — an unexpected exception (e.g. the docker binary
    missing) produces a Run with status="error", so one broken case never
    aborts the rest of the suite."""
```

One `EvalCase` → one `run_in_sandbox()` call → one `Run`, in a plain `for` loop (`asyncio` only insofar as `run_in_sandbox` itself is a coroutine — no `asyncio.gather`, no concurrency, no pool). This matches `evals/ARCHITECTURE.md`'s own Tech Stack table, unmodified: *"Orchestration: `asyncio` at v1; Ray Core actors once concurrent-rollout count justifies it."* Sequential-only is the already-stated v1 posture, not a shortcut taken here.

**Results Store** (`evals/evals/rollout/results_store.py`, new): append-only JSONL — one `Run.model_dump_json()` per line, `load_runs()`/`append_runs()`. No new infrastructure, no schema migration story, matching the precedent research's finding that this exact shape (flat files, no DB/MQ) is Inspect AI's and promptfoo's real default production mode, not a toy fallback.

**CLI** (`evals/evals/cli.py`): new `evals rollout --suite <path> --results <path> [--confidence 0.95]` command. Loads `EvalCase`s via the existing `_load_cases`, runs them through `run_suite`, appends every `Run` to the results file via `append_runs`, scores each completed run's `stdout` against `EvalCase.reference` using the *same* `exact_match`/`regex_match` functions `evals run` already uses (factored into one shared `_score_output_deterministic(output, reference, match_kind, pattern)` helper called by both commands — this is the one refactor to existing code, behavior-preserving, covered by the existing `test_cli_integration.py` suite passing unmodified), and prints the existing `format_report` (pass rate + Wilson CI, never a bare percentage). `evals run` and its `task_spec.output`-is-already-baked-in convention are completely unchanged — `rollout`'s `task_spec` uses a distinct, documented convention (`{image, command, timeout_s, match, pattern?}`) where the output comes from real execution, never from the fixture file itself. This distinction is stated explicitly in both commands' `--help` text, resolving the RFC's own open question about the two conventions coexisting.

### Drawbacks

- Two different `task_spec` shapes now exist depending on which command reads a suite file (`run`'s baked-in-`output` convention vs. `rollout`'s `{image, command}` convention) — a real, if small, cognitive-load cost. Mitigated by explicit `--help` documentation on both commands and by keeping the shapes structurally distinct enough (`output` key vs. `image`/`command` keys) that a suite written for one command fails loudly, not silently, if pointed at the other.
- `cost_usd: None` on every `Run` this pass produces means `PRD.md`'s "cost savings... down to the individual agent run" metric still has no real numeric case to point to — this RFC makes the field exist and be honest, not populated. Genuinely populating it needs an LLM-invoking harness (the next, separate slice), not a sandbox-only one.
- `Run.harness_config`'s v1 narrowness (`{image, command, timeout_s}` only) means the "harness-transparency" requirement `ARCHITECTURE.md:76` states in absolute terms ("model, scaffold version, tool budget, retry policy, step budget, and sandbox tier are recorded on every `Run`") is still not fully true after this ships — only the fields that are honestly real today are recorded. Named plainly here, not smoothed over.
- Sequential-only execution means a suite of N cases takes N times one case's wall-clock time — accepted for v1, matching `ARCHITECTURE.md`'s own stated Ray-Core-later posture.

### Alternatives Considered

1. **Build the full Rollout Lifecycle diagram in one pass** (Scheduler + Sandbox Pool + Trace Collector + Task/Dataset Registry + Results Store + Dashboard + Online Eval Service) — rejected: far larger than any single Kelvran feature shipped this session, and several boxes (Trace Collector, Online Eval Service) are gated on the still-undecided `api/otel` transport and a live gateway instance respectively.
2. **Wire `judge/llm_judge.py` to a real Anthropic/OpenAI SDK call first** — rejected as the *next* pick, not rejected outright: it would be `evals`' first-ever runtime dependency, first live secret, and first source of nondeterminism, and — the disqualifying reason — there would still be nothing that calls it in production even after wiring it, producing a *second* orphaned, real-but-uncalled seam rather than closing the loop. Named explicitly as the natural, smaller follow-up once this scheduler exists to call it.
3. **Concurrent Sandbox Pool (asyncio.gather / a worker pool) instead of sequential** — rejected for v1: `ARCHITECTURE.md`'s own Tech Stack table already states the concurrency upgrade path (asyncio → Ray Core, "once concurrent-rollout count justifies it") — there is no rollout volume yet to justify it.
4. **Fix `THREAT_MODEL.md`'s Evals STRIDE-table gap in this same pass** (since it was surfaced by this RFC's own research) — rejected: it's a distinct, larger security-hardening unit of work (network egress allowlisting, scoped per-tool credentials, tiered CI gates), analogous in size to Guardrails itself, not a natural byproduct of building a scheduler. Named in Unresolved Questions below instead, for a future pass, the same way the Guardrails RFC named `PRD.md`'s silence on guardrails without fixing it there.
5. **Do nothing / leave the Scheduler deferred** — rejected: it is the single largest gap between `evals/ARCHITECTURE.md`'s committed prose and real code today, it requires zero new infrastructure or dependencies, and every piece it needs already exists and is already tested.

## Unresolved Questions

- `THREAT_MODEL.md`'s Evals STRIDE-table gap (DoS/Information Disclosure/Elevation of Privilege rows claiming unbuilt mitigations) — real, found by this RFC's own research, deliberately not fixed here (see Alternatives Considered #4). Worth a dedicated future pass, the same size class as Guardrails was for `gateway`.
- When (if ever) `cost_usd`/real span capture become populatable depends on a future LLM-invoking harness and a decided `api/otel` transport, respectively — neither designed here.
- Results Store file-naming/retention convention (one JSONL per suite invocation vs. one growing file) is left to the operator for v1 — `--results <path>` is a plain, caller-chosen path with append semantics; no rotation/retention policy is built or designed here.
- Whether `evals/ARCHITECTURE.md`'s Tech Stack table's `numpy`/`scipy`/`scikit-learn` and "Native Anthropic/OpenAI SDKs" rows should be marked explicitly as future targets (mirroring how `gateway/ARCHITECTURE.md` already flags some rows as not-yet-built) is corrected as a small doc fix alongside this RFC's implementation, not a separate RFC.

## Research Trail

Grounded via a dynamic-workflow research pass (task `wnejahal7` / run `wf_18c6938f-277`): 4 parallel angles (code-audit, precedent, docs-gap-scan, shape-and-risk) plus a synthesis. The synthesis independently re-verified load-bearing claims against the live repo rather than trusting sub-agent prose — re-grepping for `Run`/`Trace`/`Span`/`Score` classes (zero hits confirmed independently), re-running `uv run pytest tests/ -q` (45 passed, 4 skipped, matching the code-audit exactly), and re-checking `PRD.md`'s scope-out list verbatim. One nuance the synthesis resolved rather than treated as a contradiction: the Scheduler was both "explicitly deferred with a stated reason" (true, at scaffolding time) and "a live, currently-unlisted gap" (also true, since `STATUS.md`'s refreshed Next-Action list never re-surfaced it) — both facts hold simultaneously; it is a stale deferral nobody picked back up, not a silent oversight.
