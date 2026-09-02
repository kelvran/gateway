"""Integration tests for evals.rollout.sandbox.run_in_sandbox.

These require a live Docker daemon and actually pull/run a container, so
they are skipped by default and only run when RUN_DOCKER_TESTS=1 is set —
per docs/testing/TESTING.md's integration-layer guidance and this task's
explicit "never blocks the default pytest run" requirement.

Run explicitly with:
    RUN_DOCKER_TESTS=1 uv run pytest tests/test_sandbox_integration.py -v
"""

from __future__ import annotations

import asyncio
import os

import pytest

from evals.rollout.sandbox import run_in_sandbox

pytestmark = [
    pytest.mark.integration,
    pytest.mark.skipif(
        os.environ.get("RUN_DOCKER_TESTS") != "1",
        reason="requires a live Docker daemon; set RUN_DOCKER_TESTS=1 to run",
    ),
]

_IMAGE = "alpine:3.20"


def test_run_in_sandbox_returns_stdout_on_success():
    result = asyncio.run(
        run_in_sandbox(image=_IMAGE, command=["echo", "hello-sandbox"], timeout_s=30)
    )
    assert result.timed_out is False
    assert result.exit_code == 0
    assert "hello-sandbox" in result.stdout


def test_run_in_sandbox_reports_nonzero_exit_code():
    result = asyncio.run(run_in_sandbox(image=_IMAGE, command=["false"], timeout_s=30))
    assert result.timed_out is False
    assert result.exit_code != 0


def test_run_in_sandbox_enforces_timeout():
    result = asyncio.run(
        run_in_sandbox(image=_IMAGE, command=["sleep", "30"], timeout_s=2)
    )
    assert result.timed_out is True
    assert result.exit_code == -1


def test_run_in_sandbox_blocks_network_egress():
    # --network=none means DNS resolution itself should fail; wget's exit
    # code will be nonzero rather than the command succeeding.
    result = asyncio.run(
        run_in_sandbox(
            image=_IMAGE,
            command=["wget", "-T", "5", "-O", "/dev/null", "http://example.com"],
            timeout_s=15,
        )
    )
    assert result.timed_out is False
    assert result.exit_code != 0
