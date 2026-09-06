# RFC: Golden/Regression Dataset promotion (`evals promote`)

## Status

Accepted, implemented 2026-09-05.

## Context

`evals/ARCHITECTURE.md`'s architecture diagram has always drawn a "promote failing trace" arrow into a `[Golden/Regression Dataset]` box, with a stated rationale ("a production trace that reveals a bug is promoted directly into the regression dataset, not re-typed by hand") — but no mechanism for it ever existed; that box was diagram-only. The fresh backlog audit named this gap; a ground-truth research pass confirmed no `evals promote`-shaped command, and no `EvalCase`-suite append helper, existed anywhere.

## Design

### `evals promote --run-id <id> --tier {regression,drift_sample} ...`

A new CLI command. Given a `Run.id` (from a `--results` JSONL, produced by `evals rollout`) and the `--suite` file that Run's `EvalCase` originally came from, it:

1. Looks up the Run by ID (fails loudly if not found).
2. Looks up the source `EvalCase` by `(run.eval_case_id, run.eval_case_revision)` in `--suite` (fails loudly if not found — the Run must reference a real case in the given suite).
3. If `--tier regression` and `--scores` is given: requires at least one matching `Score` (by `run_id`) with `value=false`. Promoting a passing run to the regression tier has nothing to regression-test — a real, checked precondition, not just a naming convention.
4. Constructs a **new, distinct** `EvalCase` — `id=f"{original.id}-promoted-{run.id}"`, `revision=1` (a fresh identity, never a new revision of the original — this is a derived, separate case, not an edit to the one that ran), `task_spec`/`reference` copied verbatim from the original case at the exact revision the Run used (frozen at that point, unaffected by later edits to `--suite`), `tier` set to the given `--tier`, and `tags` extended with `promoted-from:<original.id>@<original.revision>` and `promoted-from-run:<run.id>` for provenance.
5. Appends the new case to `--output` via a new `_append_case_to_suite` helper.

### `--tier golden` is not a valid choice — enforced by Click itself, not a runtime check

`click.Choice(["regression", "drift_sample"])` simply excludes `"golden"` from the option's valid values entirely. This directly enforces `THREAT_MODEL.md`'s Evals Spoofing row ("`EvalCase.tier` is set at dataset-registration time, not by the rollout itself") at the CLI surface: there is no code path in `promote_cmd` that could ever produce a golden-tier case, however the Run/Score being promoted looked, because Click rejects the argument before the command body runs at all.

### `_append_case_to_suite`: the append counterpart to the existing `_load_cases`

`EvalCase` suites are plain JSON array files (`_load_cases` already established this format, used by `run`/`rollout`), a deliberately different persistence shape from `results_store.py`'s append-only JSONL for `Run`/`Score`/`Span` — a suite file is meant to stay small and human-reviewable, not an ever-growing results log. `_append_case_to_suite` reads the existing array (or starts empty if `--output` doesn't exist yet), appends, and rewrites the whole file — matching that existing shape rather than introducing a third persistence format.

## Alternatives considered

**Auto-promotion from `rollout`/`run` on a failure, no separate command** — rejected; this is the exact "rollout claims its own tier" pattern `THREAT_MODEL.md`'s Spoofing row exists to prevent. A human must decide a failure is regression-worthy, not the tool that produced it.

**Reusing the original case's own `id`/incrementing its `revision`** — rejected; a promoted case is a genuinely new, derived entity (this specific failure, pinned), not a new version of the case that already exists at its own current revision. Conflating the two would mean editing the original suite file's own entry, which `--output` (a separate target file) deliberately avoids.

**Hard-requiring `--scores` always** — rejected; `--tier drift_sample`'s purpose (general distribution sampling, not failure-tracking) doesn't need a pass/fail precondition, and requiring a file with no real use for `drift_sample` would be needless friction.

## Verification

`.venv/bin/python -m pytest tests/ -q` → **192 passed** (up from 186), 11 skipped. `ruff check .` → `All checks passed!`. `uvx --with-editable . --from import-linter lint-imports` → still `3 kept, 0 broken`. Separately confirmed against a real local Docker daemon (`RUN_DOCKER_TESTS=1 uv run pytest tests/test_scheduler.py tests/test_cli_integration.py tests/test_sandbox_integration.py -v` → **74 passed**, 0 skipped, 0 failed).

New `tests/test_promote_integration.py` (a separate file, not added to the already-800-line-capped `test_cli_integration.py`, per this project's own "many small files" convention): 6 tests covering the happy path, Click's own golden-tier rejection, the regression-tier failing-Score precondition (both the rejection and success cases), a missing-run-id failure, and append-not-overwrite across two separate invocations into the same `--output` file. Sanity-checked by temporarily disabling the regression-tier failing-Score check: the corresponding test failed with the promotion incorrectly succeeding, then restored.
