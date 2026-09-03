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
"""

from __future__ import annotations

import time
import uuid

from evals.models import EvalCase, Run
from evals.rollout.sandbox import run_in_sandbox

DEFAULT_SANDBOX_TIMEOUT_S = 30


async def run_suite(cases: list[EvalCase]) -> list[Run]:
    """Execute every case in `cases` sequentially, returning one `Run` each."""
    runs: list[Run] = []
    for case in cases:
        harness_config = {
            "image": case.task_spec["image"],
            "command": case.task_spec["command"],
            "timeout_s": case.task_spec.get("timeout_s", DEFAULT_SANDBOX_TIMEOUT_S),
        }
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
            runs.append(
                Run(
                    id=uuid.uuid4().hex,
                    eval_case_id=case.id,
                    eval_case_revision=case.revision,
                    harness_config=harness_config,
                    status="error",
                    latency_ms=latency_ms,
                    error=str(exc),
                )
            )
            continue

        latency_ms = (time.monotonic() - start) * 1000
        runs.append(
            Run(
                id=uuid.uuid4().hex,
                eval_case_id=case.id,
                eval_case_revision=case.revision,
                harness_config=harness_config,
                status="timed_out" if result.timed_out else "completed",
                exit_code=result.exit_code,
                stdout=result.stdout,
                stderr=result.stderr,
                latency_ms=latency_ms,
            )
        )
    return runs
