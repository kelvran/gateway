# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `evals` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.2.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) by default. Revisit CalVer (`YYYY.MM.PATCH`) once `evals` ships continuously without hard breaking changes — see `RELEASE.md`.

## Added

- Rollout Scheduler, `Run` model, and JSONL Results Store, per `docs/rfcs/2026-09-04-evals-rollout-scheduler.md`: the first genuinely end-to-end "case → real sandboxed execution → scored, persisted result" path in `evals/`. A new `Run` pydantic model (`evals/models.py`, same `frozen=True` convention as `EvalCase`) records `harness_config` (`{image, command, timeout_s}`), `status` (`completed`/`timed_out`/`error`), `exit_code`/`stdout`/`stderr`, `latency_ms`, and `cost_usd` — deliberately `None` by default, never `0.0`, since v1's sandbox-only harness makes no billed LLM call and reporting zero would falsely claim "measured." A new `evals.rollout.scheduler.run_suite` executes each `EvalCase` sequentially through the existing, unmodified `run_in_sandbox()` (no concurrency/pool this pass, per `evals/ARCHITECTURE.md`'s own stated "asyncio at v1; Ray Core once justified" posture); a launch failure (e.g. a missing `docker` binary) is caught and turned into a `status="error"` `Run` rather than aborting the whole suite. A new `evals.rollout.results_store` (`append_runs`/`load_runs`) persists every `Run` as one line of an append-only JSONL file — no new infrastructure, matching the grounding research's precedent finding that this exact filesystem-only shape is Inspect AI's and promptfoo's real default production mode, not a fallback. A new `evals rollout --suite <path> --results <path>` CLI command wires it together, scoring each completed `Run`'s captured `stdout` against the case's `reference` with the existing deterministic scorer and printing the existing Wilson-CI-bearing report — `evals run`'s own baked-in-`task_spec.output` convention and behavior are completely unchanged.

## Changed

- `evals/ARCHITECTURE.md`'s Tech Stack table's `Statistics`/`Judge SDKs` rows now distinguish what's real today (the Wilson-interval calculator, stdlib-only) from the future target (bootstrap resampling/Bayesian comparison/pass@k via `numpy`/`scipy`/`scikit-learn`; a real Anthropic/OpenAI SDK call for `judge()`) — neither package family is installed today, and the prior wording read as already-wired.

## Deprecated

## Removed

## Fixed

## Security
