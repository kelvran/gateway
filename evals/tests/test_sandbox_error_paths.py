"""Tests for `evals.rollout.sandbox.run_in_sandbox`'s error paths that do
NOT require a live Docker daemon — distinct from
`tests/test_sandbox_integration.py`'s skip-by-default, real-Docker suite.

These run in the default suite (no `RUN_DOCKER_TESTS=1` needed) because
they monkeypatch `asyncio.create_subprocess_exec` to simulate the `docker`
binary itself being absent from `PATH`, which is exactly what happens on a
CI runner or a fresh dev machine that hasn't installed Docker yet — a
real, valuable regression to guard against without needing a daemon.
"""

from __future__ import annotations

import asyncio

import pytest

from evals.rollout.sandbox import run_in_sandbox


def test_run_in_sandbox_raises_clear_error_when_docker_binary_missing(monkeypatch):
    async def _raise_file_not_found(*args, **kwargs):
        raise FileNotFoundError("[Errno 2] No such file or directory: 'docker'")

    monkeypatch.setattr(asyncio, "create_subprocess_exec", _raise_file_not_found)

    with pytest.raises(FileNotFoundError):
        asyncio.run(
            run_in_sandbox(image="alpine:3.20", command=["echo", "hi"], timeout_s=5)
        )


def test_run_in_sandbox_docker_missing_error_names_the_binary(monkeypatch):
    # Regression check on the actual error content: whatever bubbles up
    # from a missing `docker` binary must be identifiable as such, not a
    # generic, unhelpful exception with no clue what failed.
    async def _raise_file_not_found(*args, **kwargs):
        raise FileNotFoundError(2, "No such file or directory")

    monkeypatch.setattr(asyncio, "create_subprocess_exec", _raise_file_not_found)

    with pytest.raises(FileNotFoundError) as exc_info:
        asyncio.run(run_in_sandbox(image="alpine:3.20", command=["true"], timeout_s=5))
    assert exc_info.value.errno == 2
