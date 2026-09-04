"""Sequential Rollout Scheduler.

v1 contract, per docs/rfcs/2026-09-04-evals-rollout-scheduler.md: one
`EvalCase` maps to exactly one `run_in_sandbox()` call, executed one at a
time (no `asyncio.gather`, no worker pool, no Ray) — per
evals/ARCHITECTURE.md's own Tech Stack table, sequential `asyncio` is the
stated v1 posture; concurrency is a later upgrade, not a shortcut taken
here.

`EvalCase.task_spec` for a case run through this scheduler must contain
`image` and `command` (a list of strings); `timeout_s` is optional and
defaults to `DEFAULT_SANDBOX_TIMEOUT_S`. This is a distinct convention from
`evals.cli`'s `run` command, whose `task_spec.output` is a value already
baked into the fixture file rather than produced by real execution.

A single case's failure never aborts the suite: any exception raised by
`run_in_sandbox` itself (e.g. `FileNotFoundError` when the `docker` binary
is missing) is caught and turned into a `Run` with `status="error"`, and
the scheduler moves on to the next case.

Two additional, both off-by-default mechanisms, per
docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md: an opt-in result
cache (`cached_runs`) and a statistically-sound two-checkpoint
early-stopping rule (`early_stop`) for repeated trials of the same
`(eval_case_id, eval_case_revision)`. Both default to `None`, in which case
`run_suite`'s behavior is byte-for-byte identical to before this RFC.
"""

from __future__ import annotations

import time
import uuid
from collections.abc import Awaitable, Callable
from dataclasses import dataclass

from evals import tracing
from evals.models import EvalCase, Run, Span
from evals.rollout.cache import compute_run_cache_key
from evals.rollout.sandbox import run_in_sandbox
from evals.stats import two_checkpoint_early_stop

DEFAULT_SANDBOX_TIMEOUT_S = 30


@dataclass(frozen=True)
class EarlyStopConfig:
    """Config for two-checkpoint early stopping within one repeated-trial
    group. See `evals.stats.two_checkpoint_early_stop` for the statistical
    mechanism and why it is not a continuous per-trial recheck.

    `score_fn` is async (`run_suite` is already running inside an event
    loop by the time it's called, so a synchronous callable that itself
    needs to await a coroutine — e.g. an LLM-judge call — would raise
    "cannot be called from a running event loop" the moment it tried to
    drive its own nested `asyncio.run()`; a real bug caught by this
    project's own test suite before shipping, not a hypothetical). It
    must never raise and must return `False` for any `Run` whose
    `status != "completed"` — this mirrors the existing
    `evals.cli._score_run_deterministic` convention exactly, not a new
    judgment call: an errored/timed-out trial still counts as one real
    trial in the group's tally (so a run of transient infra errors can
    still reach a checkpoint decision instead of the group never
    accumulating evidence at all), but never as a success.
    """

    min_trials: int
    max_trials: int
    baseline_pass_rate: float
    score_fn: Callable[[EvalCase, Run], Awaitable[bool]]
    confidence: float = 0.95


def _stop_after(early_stop: EarlyStopConfig, successes: int, trials_run: int) -> bool:
    return two_checkpoint_early_stop(
        successes,
        trials_run,
        early_stop.min_trials,
        early_stop.max_trials,
        early_stop.baseline_pass_rate,
        early_stop.confidence,
    )


async def run_suite(
    cases: list[EvalCase],
    *,
    cached_runs: dict[str, Run] | None = None,
    early_stop: EarlyStopConfig | None = None,
    span_sink: list[Span] | None = None,
) -> list[Run]:
    """Execute every case in `cases` sequentially, returning one `Run` each.

    `cached_runs`/`early_stop`/`span_sink` are all keyword-only and all
    default to `None` — omitting all three reproduces the exact behavior
    this function had before docs/rfcs/2026-09-04-evals-rollout-cost-
    mitigation.md.

    `span_sink`, per docs/rfcs/2026-09-04-evals-trace-span-model.md: when
    given, exactly one `Span` is appended for every real
    `run_in_sandbox()` attempt (success or exception) -- never for a cache
    hit or an early-stop skip, neither of which represents a real
    execution to trace.
    """
    runs: list[Run] = []
    group_tallies: dict[tuple[str, int], tuple[int, int]] = {}
    stopped_groups: set[tuple[str, int]] = set()

    async def tally_and_maybe_stop(case: EvalCase, run: Run) -> None:
        if early_stop is None:
            return
        group_key = (case.id, case.revision)
        prior_successes, prior_trials = group_tallies.get(group_key, (0, 0))
        passed = await early_stop.score_fn(case, run)
        tally = (prior_successes + (1 if passed else 0), prior_trials + 1)
        group_tallies[group_key] = tally
        if _stop_after(early_stop, tally[0], tally[1]):
            stopped_groups.add(group_key)

    for case in cases:
        group_key = (case.id, case.revision)
        harness_config = {
            "image": case.task_spec["image"],
            "command": case.task_spec["command"],
            "timeout_s": case.task_spec.get("timeout_s", DEFAULT_SANDBOX_TIMEOUT_S),
        }

        if early_stop is not None and group_key in stopped_groups:
            runs.append(
                Run(
                    id=uuid.uuid4().hex,
                    eval_case_id=case.id,
                    eval_case_revision=case.revision,
                    harness_config=harness_config,
                    status="skipped",
                    latency_ms=0.0,
                    skip_reason=(
                        "early-stopped: this group already reached a stopping decision"
                    ),
                )
            )
            continue

        cache_key = compute_run_cache_key(case.task_spec, harness_config)
        cached = (
            cached_runs.get(cache_key)
            if cached_runs is not None and case.tier != "drift_sample"
            else None
        )
        # Defense in depth, not just a CLI-layer convention: an
        # error/timed_out prior Run carries no real output, so treating
        # it as a hit would fabricate a fake completed result. Enforced
        # here, at the lowest layer, regardless of whether the caller's
        # own cached_runs construction already filtered for this.
        if cached is not None and cached.status != "completed":
            cached = None

        if cached is not None:
            run = Run(
                id=uuid.uuid4().hex,
                eval_case_id=case.id,
                eval_case_revision=case.revision,
                harness_config=harness_config,
                status="completed",
                exit_code=cached.exit_code,
                stdout=cached.stdout,
                stderr=cached.stderr,
                latency_ms=0.0,
                cost_usd=cached.cost_usd,
                cache_key=cache_key,
                from_cache=True,
                cache_source_run_id=cached.id,
            )
            runs.append(run)
        else:
            run_id = uuid.uuid4().hex
            otel_span = (
                tracing.start_sandbox_span(
                    image=harness_config["image"], command=harness_config["command"]
                )
                if span_sink is not None
                else None
            )
            start = time.monotonic()
            try:
                result = await run_in_sandbox(
                    image=harness_config["image"],
                    command=harness_config["command"],
                    timeout_s=harness_config["timeout_s"],
                )
            except Exception as exc:
                # Deliberately broad: any sandbox-launch failure must become a
                # scoreable Run, not an aborted suite.
                latency_ms = (time.monotonic() - start) * 1000
                run = Run(
                    id=run_id,
                    eval_case_id=case.id,
                    eval_case_revision=case.revision,
                    harness_config=harness_config,
                    status="error",
                    latency_ms=latency_ms,
                    error=str(exc),
                    cache_key=cache_key,
                )
                runs.append(run)
                if otel_span is not None:
                    span_sink.append(
                        tracing.finish_sandbox_span(
                            otel_span,
                            run_id=run_id,
                            image=harness_config["image"],
                            command=harness_config["command"],
                            exit_code=None,
                            container_id=None,
                            error=str(exc),
                        )
                    )
                await tally_and_maybe_stop(case, run)
                continue

            latency_ms = (time.monotonic() - start) * 1000
            run = Run(
                id=run_id,
                eval_case_id=case.id,
                eval_case_revision=case.revision,
                harness_config=harness_config,
                status="timed_out" if result.timed_out else "completed",
                exit_code=result.exit_code,
                stdout=result.stdout,
                stderr=result.stderr,
                latency_ms=latency_ms,
                cache_key=cache_key,
            )
            runs.append(run)
            if otel_span is not None:
                span_sink.append(
                    tracing.finish_sandbox_span(
                        otel_span,
                        run_id=run_id,
                        image=harness_config["image"],
                        command=harness_config["command"],
                        exit_code=result.exit_code,
                        container_id=result.container_id,
                        error=None,
                    )
                )

        await tally_and_maybe_stop(case, run)

    return runs
