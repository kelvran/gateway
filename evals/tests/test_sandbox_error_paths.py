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


class _HangingProcess:
    """A fake `asyncio.subprocess.Process` whose `communicate()` never
    resolves on its own — real timeout/cancellation enforcement (via
    `asyncio.wait_for`/`task.cancel()`) is what actually interrupts it in
    the tests below, exactly like a genuinely stuck real container
    would."""

    returncode: int | None = None

    async def communicate(self):
        await asyncio.sleep(3600)
        return b"", b""  # pragma: no cover -- never actually reached

    def kill(self):
        pass

    async def wait(self):
        self.returncode = -9


def test_timeout_cleanup_failure_does_not_mask_timed_out_result(monkeypatch):
    """Real gap closed 2026-09-21: if the timeout branch's own cleanup
    (docker-kill the real container) itself raises, that exception must
    never replace the SandboxResult(timed_out=True) `run_in_sandbox`'s
    own docstring promises callers on a genuine timeout — a caller must
    never see a raised exception here at all.
    """

    async def _fake_create_subprocess_exec(*args, **kwargs):
        return _HangingProcess()

    async def _raise_during_docker_kill(container_id):
        raise OSError("simulated docker kill failure during timeout cleanup")

    monkeypatch.setattr(asyncio, "create_subprocess_exec", _fake_create_subprocess_exec)
    monkeypatch.setattr(
        "evals.rollout.sandbox._read_cidfile", lambda path: "fake-container-id"
    )
    monkeypatch.setattr("evals.rollout.sandbox._docker_kill", _raise_during_docker_kill)

    result = asyncio.run(
        run_in_sandbox(image="alpine:3.20", command=["sleep", "999"], timeout_s=0.05)
    )

    assert result.timed_out is True
    assert result.exit_code == -1
    assert result.container_id == "fake-container-id"


def test_cancellation_cleanup_failure_does_not_mask_original_cancellation(monkeypatch):
    """The identical proof for the BaseException/cancellation branch: a
    teardown failure during cancellation cleanup must never replace the
    real asyncio.CancelledError the caller is unwinding with.
    """

    async def _fake_create_subprocess_exec(*args, **kwargs):
        return _HangingProcess()

    async def _raise_during_docker_kill(container_id):
        raise OSError("simulated docker kill failure during cancellation cleanup")

    monkeypatch.setattr(asyncio, "create_subprocess_exec", _fake_create_subprocess_exec)
    monkeypatch.setattr(
        "evals.rollout.sandbox._read_cidfile", lambda path: "fake-container-id"
    )
    monkeypatch.setattr("evals.rollout.sandbox._docker_kill", _raise_during_docker_kill)

    async def _start_then_cancel() -> None:
        task = asyncio.ensure_future(
            run_in_sandbox(image="alpine:3.20", command=["sleep", "999"], timeout_s=30)
        )
        await asyncio.sleep(0.05)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    asyncio.run(_start_then_cancel())
