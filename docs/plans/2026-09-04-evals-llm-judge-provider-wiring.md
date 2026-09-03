> **For agentic executors:** Task 1 (`providers.py` + dependency) is independent and must land first — Task 2 (CLI wiring) depends on it. Task 3 (CI-safety gate + integration test) can land alongside Task 2. Task 4 is docs/changelog/wrap-up.

---

**Goal:** Wire `evals/evals/judge/llm_judge.py`'s `judge()` to a real Anthropic SDK call via a new `providers.py` module, exposed through a `--llm-judge` flag on both `evals run` and `evals rollout`, with a CI-safe opt-in integration test.

**Architecture:** New `evals/evals/judge/providers.py` (`make_anthropic_call_model`) — the sole file in `evals/` importing `anthropic`. `judge()`/`llm_judge.py` are unmodified. `evals/evals/cli.py`'s `run_cmd` gets its `--llm-judge` stub replaced with a real branch; `rollout_cmd` gains a matching new flag. A judge-call failure marks one case as errored and continues, mirroring `scheduler.py`'s existing per-case failure handling.

**Spec:** `docs/rfcs/2026-09-04-evals-llm-judge-provider-wiring.md` — the SDK choice, default model id, CI-safety env var, and failure-semantics decisions all live there; this plan implements them verbatim.

**Global Constraints** (inherited from the spec):
- Anthropic only — no OpenAI SDK, no multi-judge/skeptic panel.
- `judge()`'s signature and `llm_judge.py`'s zero-SDK-code property are never touched.
- No `asyncio.gather` — sequential judge calls only, matching `scheduler.py`'s v1 posture.
- `Run.cost_usd` is never populated by a judge call — stays `None`, unmodified by this pass.
- The new integration test is gated by a NEW `RUN_LIVE_LLM_TESTS` env var, never `RUN_DOCKER_TESTS` — default `make verify`/CI must never require `ANTHROPIC_API_KEY`.
- Default judge model is the pinned, dated snapshot id `claude-haiku-4-5-20251001` — never a bare alias.

---

## Task 1 — `providers.py` + `anthropic` dependency

**Files:**
- Create: `evals/evals/judge/providers.py`
- Modify: `evals/pyproject.toml`
- Create: `evals/tests/test_providers.py`

**Steps:**
- [ ] `uv add anthropic` from `evals/` (adds to `dependencies`, updates `uv.lock`) — confirm it lands in `[project].dependencies`, not `[dependency-groups.dev]`.
- [ ] `providers.py`: module docstring naming the lazy-construction guarantee ("importing this module never requires a key; only calling `make_anthropic_call_model()` or its returned closure does"). `_DEFAULT_JUDGE_MODEL = "claude-haiku-4-5-20251001"` constant. `make_anthropic_call_model(model: str = _DEFAULT_JUDGE_MODEL, client: AsyncAnthropic | None = None) -> Callable[[str], Awaitable[str]]`: constructs `client or AsyncAnthropic()` inside the function body (never at module scope); returns an inner `async def call_model(prompt: str) -> str` closure that calls `anthropic_client.messages.create(model=model, max_tokens=1024, messages=[{"role": "user", "content": prompt}])` and returns `response.content[0].text`.
- [ ] `test_providers.py`: unit tests using a fake/mock `AsyncAnthropic`-shaped client passed via the `client=` param (no real key, no real network call) — proving: (a) the returned closure calls `client.messages.create` with the given model and the prompt text; (b) the closure extracts and returns `response.content[0].text`; (c) importing `evals.judge.providers` alone (no instantiation) never raises even without `ANTHROPIC_API_KEY` set (assert via `monkeypatch.delenv`).

**Verify:** `cd evals && uv run pytest tests/test_providers.py -v && ruff check evals/judge/providers.py`

## Task 2 — CLI wiring: `run_cmd` real branch, `rollout_cmd` new flag

**Files:**
- Modify: `evals/evals/cli.py`
- Modify: `evals/tests/test_cli_integration.py`

**Steps:**
- [ ] Import `from evals.judge.llm_judge import judge` and `from evals.judge.providers import make_anthropic_call_model` in `cli.py`.
- [ ] `run_cmd`: replace the unconditional `raise click.ClickException(...)` (current lines ~127-135) with: if `llm_judge`, build `call_model = make_anthropic_call_model()` once, then for each case — if `case.reference is None`, raise `click.ClickException` naming the case id (mirrors `_score_output_deterministic`'s existing exact-match-requires-a-reference message); otherwise `asyncio.run(judge(output=case.task_spec["output"], reference=case.reference, call_model=call_model))`, wrapped in `try/except Exception` — on exception, print `f"{case.id}: JUDGE_ERROR"` and count toward `total` but not `successes`; on success, print `PASS`/`FAIL` per `result.passed` and increment `successes` accordingly. Same trailing `format_report(...)` call as today.
- [ ] `rollout_cmd`: add a `--llm-judge` `click.option` (`is_flag=True, default=False`, help text naming the real Anthropic wiring). When set, build `call_model` once, and for each `(case, run)` pair where `run.status == "completed"`, judge `run.stdout.strip()` against `case.reference` the same way as above (same `case.reference is None` check, same per-case exception handling, same `JUDGE_ERROR` label); `Run`s with `status != "completed"` are still never judged (existing `_score_run_deterministic` semantics preserved for that branch). When `--llm-judge` is NOT set, behavior is 100% unchanged from what's live today (`_score_run_deterministic`).
- [ ] `--help` text on both commands' `--llm-judge` option updated to reflect the real wiring (no longer "not implemented").
- [ ] New tests in `test_cli_integration.py`, monkeypatching `evals.cli.make_anthropic_call_model` (or passing a fake via dependency injection at the module level, whichever keeps the test from touching the real SDK) to return a scripted fake `call_model`: `evals run --suite <fixture> --llm-judge` prints judge-based PASS/FAIL and the Wilson CI line; `evals rollout --suite <fixture> --results <path> --llm-judge` (combined with the existing sandbox monkeypatch) judges real-execution `stdout` instead of scoring deterministically; a case with `reference=None` and `--llm-judge` fails with a clear `ClickException`, never a silent no-op; a judge call that raises produces a `JUDGE_ERROR` line and the invocation still completes (does not abort the rest of the suite) — mirror `test_scheduler.py`'s "one case's failure never aborts the suite" test shape.

**Verify:** `cd evals && uv run pytest tests/test_cli_integration.py -v` (no real API key needed — all default-suite tests use a fake `call_model`).

## Task 3 — CI-safety gate + real integration test

**Files:**
- Modify: `evals/pyproject.toml` (`[tool.pytest.ini_options] markers`)
- Create: `evals/tests/test_llm_judge_integration.py`

**Steps:**
- [ ] Register `llm_integration: requires a live Anthropic API key (skipped unless RUN_LIVE_LLM_TESTS=1)` in `pyproject.toml`'s `markers` list, alongside the existing `integration` entry.
- [ ] `test_llm_judge_integration.py`: module-level `pytestmark = [pytest.mark.llm_integration, pytest.mark.skipif(os.environ.get("RUN_LIVE_LLM_TESTS") != "1", reason="requires a live Anthropic API key; set RUN_LIVE_LLM_TESTS=1 to run")]`, mirroring `test_sandbox_integration.py`'s exact shape. One real test: `make_anthropic_call_model()()` (default model, real key from env) against a trivially judgeable prompt (e.g. `judge(output="Paris", reference="Paris", call_model=make_anthropic_call_model())`), asserting `result.passed is True` and `result.rationale` is non-empty — a genuine, live proof the wiring works end to end, not just against a fake.
- [ ] Confirm `cd evals && uv run pytest tests/` (no env var set) shows this new test as `skipped`, not `passed`/`failed`/collected-and-erroring — the default suite must never require the key.

**Verify:** default run shows the new test skipped; separately, with a real `ANTHROPIC_API_KEY` set, `RUN_LIVE_LLM_TESTS=1 uv run pytest tests/test_llm_judge_integration.py -v` passes for real.

## Task 4 — Docs, changelog, wrap-up

**Files:**
- Modify: `evals/ARCHITECTURE.md` (Tech Stack `Judge SDKs` row)
- Modify: `evals/changelog/unreleased.md`
- Modify: `DECISIONS.md`
- Modify: `docs/agents/LOGS.md`
- Modify: `STATUS.md`

**Steps:**
- [ ] `evals/ARCHITECTURE.md`'s Tech Stack `Judge SDKs` row: flip from "Not yet wired" to naming the real Anthropic wiring, the pinned default model id, and that a second provider remains a future, same-shaped follow-on.
- [ ] `evals/changelog/unreleased.md`: new `## Added` entry describing `providers.py`, the `--llm-judge` wiring on both commands, the `RUN_LIVE_LLM_TESTS` gate, and the deliberate `Run.cost_usd` non-change — matching the detail level of the Rollout Scheduler's own entry.
- [ ] `DECISIONS.md`: one new line at the true chronological end (re-check `tail` immediately before appending — this file's own append-only convention was nearly broken during the prior pass; verify placement before treating the edit as done) naming the Anthropic-only choice, the pinned model id, the new env-var gate, and the THREAT_MODEL.md Evals-gap sizing finding (doc-fix-now, 3-to-5 future RFCs for the rest) surfaced by this same research but not fixed in this pass.
- [ ] `docs/agents/LOGS.md`: new entry (Files touched / Intent-summary / Decisions made / Verification performed / Bugs found / Next steps).
- [ ] `STATUS.md`: update Current Phase / Last Completed Task / Next Action / Verification State — name the `Score` model + persistence and the `THREAT_MODEL.md` doc-correction as the next open candidates, plus the two newly-surfaced, unrelated `gateway/ARCHITECTURE.md` gaps (missing `internal/router`, false `go-arch-lint` CI-enforcement claim) for future attention.
- [ ] Full `make verify` from repo root — must pass clean before commit (no `RUN_LIVE_LLM_TESTS` set; `gateway` untouched by this pass, so its results must be identical to before).

**Verify:** `make verify` (root) passes end-to-end; `git diff` reviewed in full before committing.

## Scope Gate

This is architecturally scoped (new module + new runtime dependency + new CLI surface + new CI-safety gate), correctly warranting a plan file, not a one-line `DECISIONS.md` entry.
