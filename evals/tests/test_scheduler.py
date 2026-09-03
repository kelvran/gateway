"""Tests for `evals.rollout.scheduler.run_suite` that do NOT require a live
Docker daemon — monkeypatches `run_in_sandbox` exactly as
`tests/test_sandbox_error_paths.py` does, distinct from this file's own
`RUN_DOCKER_TESTS=1`-gated real end-to-end test below.
"""

from __future__ import annotations

import asyncio
import os

import pytest

import evals.rollout.scheduler as scheduler_module
from evals.models import EvalCase
from evals.rollout.sandbox import SandboxResult
from evals.rollout.scheduler import run_suite


def _make_case(case_id: str, command: list[str]) -> EvalCase:
    return EvalCase(
        id=case_id,
        revision=1,
        task_spec={"image": "alpine:3.20", "command": command, "timeout_s": 5},
        reference=None,
        tier="golden",
    )


def test_successful_run_produces_completed_status(monkeypatch):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        return SandboxResult(exit_code=0, stdout="hi\n", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    runs = asyncio.run(run_suite([_make_case("c1", ["echo", "hi"])]))

    assert len(runs) == 1
    run = runs[0]
    assert run.status == "completed"
    assert run.exit_code == 0
    assert run.stdout == "hi\n"
    assert run.eval_case_id == "c1"
    assert run.eval_case_revision == 1
    assert run.harness_config == {
        "image": "alpine:3.20",
        "command": ["echo", "hi"],
        "timeout_s": 5,
    }
    assert run.cost_usd is None


def test_timed_out_sandbox_result_produces_timed_out_status(monkeypatch):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        return SandboxResult(exit_code=-1, stdout="", stderr="", timed_out=True)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    runs = asyncio.run(run_suite([_make_case("c1", ["sleep", "30"])]))

    assert runs[0].status == "timed_out"


def test_sandbox_launch_failure_produces_error_status_not_a_raised_exception(
    monkeypatch,
):
    async def _raise_file_not_found(image, command, timeout_s):
        raise FileNotFoundError("[Errno 2] No such file or directory: 'docker'")

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _raise_file_not_found)

    runs = asyncio.run(run_suite([_make_case("c1", ["echo", "hi"])]))

    assert len(runs) == 1
    assert runs[0].status == "error"
    assert runs[0].error is not None
    assert "docker" in runs[0].error


def test_one_case_erroring_does_not_abort_the_rest_of_the_suite(monkeypatch):
    calls = {"n": 0}

    async def _fail_first_then_succeed(image, command, timeout_s):
        calls["n"] += 1
        if calls["n"] == 1:
            raise FileNotFoundError("no docker")
        return SandboxResult(exit_code=0, stdout="ok\n", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fail_first_then_succeed)

    runs = asyncio.run(
        run_suite(
            [
                _make_case("c1", ["echo", "one"]),
                _make_case("c2", ["echo", "two"]),
            ]
        )
    )

    assert len(runs) == 2
    assert runs[0].status == "error"
    assert runs[1].status == "completed"
    assert runs[1].stdout == "ok\n"


def test_missing_timeout_s_defaults_to_default_sandbox_timeout(monkeypatch):
    seen = {}

    async def _capture(image, command, timeout_s):
        seen["timeout_s"] = timeout_s
        return SandboxResult(exit_code=0, stdout="", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _capture)

    case = EvalCase(
        id="c1",
        revision=1,
        task_spec={"image": "alpine:3.20", "command": ["true"]},
        tier="golden",
    )
    asyncio.run(run_suite([case]))

    assert seen["timeout_s"] == scheduler_module.DEFAULT_SANDBOX_TIMEOUT_S


@pytest.mark.integration
@pytest.mark.skipif(
    os.environ.get("RUN_DOCKER_TESTS") != "1",
    reason="requires a live Docker daemon; set RUN_DOCKER_TESTS=1 to run",
)
def test_run_suite_against_a_real_docker_daemon():
    case = _make_case("real-c1", ["echo", "hello-scheduler"])
    runs = asyncio.run(run_suite([case]))

    assert len(runs) == 1
    assert runs[0].status == "completed"
    assert "hello-scheduler" in runs[0].stdout
