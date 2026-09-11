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
import subprocess
import time

import pytest

from evals.rollout.sandbox import (
    DEFAULT_SANDBOX_CPUS,
    DEFAULT_SANDBOX_MEMORY_MB,
    DEFAULT_SANDBOX_PIDS_LIMIT,
    run_in_sandbox,
)

pytestmark = [
    pytest.mark.integration,
    pytest.mark.skipif(
        os.environ.get("RUN_DOCKER_TESTS") != "1",
        reason="requires a live Docker daemon; set RUN_DOCKER_TESTS=1 to run",
    ),
]

_IMAGE = "alpine:3.20"


def _container_is_running(container_id: str) -> bool:
    """True only if `container_id` genuinely still exists and is running.

    A "No such container" result (already reaped by --rm) is treated as
    definitely-not-running, not an error -- that's the expected steady
    state once a container has exited or been killed.
    """
    result = subprocess.run(
        ["docker", "inspect", "-f", "{{.State.Running}}", container_id],
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        return False
    return result.stdout.strip() == "true"


def test_run_in_sandbox_returns_stdout_on_success():
    result = asyncio.run(
        run_in_sandbox(image=_IMAGE, command=["echo", "hello-sandbox"], timeout_s=30)
    )
    assert result.timed_out is False
    assert result.exit_code == 0
    assert "hello-sandbox" in result.stdout
    assert result.container_id is not None
    assert len(result.container_id) == 64


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


def test_run_in_sandbox_timeout_actually_stops_the_container():
    """The load-bearing regression test for the real bug fixed 2026-09-04:
    killing the local `docker run` CLI process alone does NOT stop the
    container -- confirmed empirically against a real Docker daemon
    before this fix (the container kept running for its full `sleep`
    duration after the CLI process was killed). This proves the real
    container is genuinely gone, not just that the Python call returned.
    """
    result = asyncio.run(
        run_in_sandbox(image=_IMAGE, command=["sleep", "30"], timeout_s=2)
    )
    assert result.timed_out is True
    assert result.container_id is not None
    assert not _container_is_running(result.container_id), (
        "container is still running after a reported timeout -- "
        "the timeout did not actually bound resource usage"
    )


def _running_container_ids_for_image(image: str) -> set[str]:
    """The set of currently-running container IDs whose image is `image`
    -- used (rather than a single known container_id, unavailable here
    since the call this test cancels never returns one) to detect ANY
    leaked container from the cancelled run_in_sandbox call.
    """
    result = subprocess.run(
        ["docker", "ps", "-q", "--filter", f"ancestor={image}"],
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        return set()
    return set(result.stdout.split())


def test_run_in_sandbox_stops_the_container_on_task_cancellation():
    """A round-4 backlog-audit finding: run_in_sandbox's own cleanup only
    ever caught `TimeoutError` -- `asyncio.CancelledError` (a real,
    ordinary trigger: asyncio.run's own SIGINT handler cancels the
    running task on Ctrl-C since Python 3.11) is a BaseException, not an
    Exception, and previously propagated straight through with the
    already-created container left running unbounded. Proves the real
    container is genuinely gone after the awaiting task is cancelled --
    not via timeout_s elapsing (already covered by
    test_run_in_sandbox_timeout_actually_stops_the_container above), but
    via an external task.cancel() landing while the container is
    genuinely running.
    """
    before = _running_container_ids_for_image(_IMAGE)

    async def _start_then_cancel() -> None:
        task = asyncio.ensure_future(
            run_in_sandbox(image=_IMAGE, command=["sleep", "30"], timeout_s=30)
        )
        # alpine's `sleep` starts almost instantly -- this is real
        # margin for the container to genuinely be running before
        # cancellation lands, not a race against container creation
        # itself.
        await asyncio.sleep(1.5)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    asyncio.run(_start_then_cancel())

    # A brief real margin for `docker kill` (issued inside
    # run_in_sandbox's own cancellation cleanup) to actually take effect
    # before this test's own assertion reads Docker's live state.
    time.sleep(1)
    leaked = _running_container_ids_for_image(_IMAGE) - before
    assert not leaked, (
        f"container(s) {leaked} still running after the awaiting task was "
        "cancelled -- cancellation must stop the real container, not just "
        "abandon the local docker run CLI process"
    )


def test_run_in_sandbox_applies_configured_resource_limits():
    """A round-4 backlog-audit finding: run_in_sandbox's own `docker run`
    invocation previously set no CPU/memory/process-count bound at all --
    a single sandboxed command with an unbounded memory allocation or a
    fork bomb was a real, unmitigated host-level DoS against the machine
    running `evals rollout` itself. Proves the configured limits reach
    the real container's own HostConfig (via `docker inspect`), not just
    that the flags exist in the constructed argument list -- inspects a
    genuinely still-running container (not a fabricated/hypothetical
    one), the same "prove it against real Docker state" discipline
    test_run_in_sandbox_timeout_actually_stops_the_container already
    established for the timeout fix.

    Deliberately does not attempt to trigger a real OOM or fork-bomb
    condition -- that would add real flakiness risk (host/platform-
    dependent OOM-killer timing) for marginal extra confidence beyond
    directly confirming Docker itself accepted and applied the exact
    values this module configures.
    """

    async def _inspect_while_running() -> None:
        task = asyncio.ensure_future(
            run_in_sandbox(image=_IMAGE, command=["sleep", "3"], timeout_s=15)
        )
        await asyncio.sleep(1)
        running = _running_container_ids_for_image(_IMAGE)
        assert running, "expected a running container to inspect"
        container_id = next(iter(running))

        inspect = subprocess.run(
            [
                "docker",
                "inspect",
                "-f",
                "{{.HostConfig.Memory}}|{{.HostConfig.MemorySwap}}|"
                "{{.HostConfig.NanoCpus}}|{{.HostConfig.PidsLimit}}",
                container_id,
            ],
            capture_output=True,
            text=True,
            check=False,
        )
        assert inspect.returncode == 0, inspect.stderr
        memory, memory_swap, nano_cpus, pids_limit = inspect.stdout.strip().split("|")

        expected_bytes = DEFAULT_SANDBOX_MEMORY_MB * 1024 * 1024
        assert int(memory) == expected_bytes, (
            f"HostConfig.Memory = {memory}, want {expected_bytes}"
        )
        # memory-swap == memory means Docker allows zero ADDITIONAL swap
        # beyond the memory limit itself -- confirms swap isn't a
        # loophole letting the container exceed the intended ceiling.
        assert int(memory_swap) == expected_bytes, (
            f"HostConfig.MemorySwap = {memory_swap}, want {expected_bytes} "
            "(swap disabled)"
        )
        assert int(nano_cpus) == DEFAULT_SANDBOX_CPUS * 1_000_000_000, (
            f"HostConfig.NanoCpus = {nano_cpus}"
        )
        assert int(pids_limit) == DEFAULT_SANDBOX_PIDS_LIMIT, (
            f"HostConfig.PidsLimit = {pids_limit}, want {DEFAULT_SANDBOX_PIDS_LIMIT}"
        )

        result = await task  # let the sandboxed command finish naturally
        assert result.exit_code == 0
        assert result.timed_out is False

    asyncio.run(_inspect_while_running())


def test_run_in_sandbox_root_filesystem_is_read_only():
    # Writing outside /tmp must fail -- the container's root filesystem is
    # immutable (--read-only), the real gap THREAT_MODEL.md's Evals
    # Information Disclosure row named until 2026-09-05.
    result = asyncio.run(
        run_in_sandbox(
            image=_IMAGE,
            command=["sh", "-c", "echo data > /etc/should-not-write"],
            timeout_s=15,
        )
    )
    assert result.timed_out is False
    assert result.exit_code != 0
    assert "Read-only file system" in result.stderr


def test_run_in_sandbox_tmp_is_still_writable():
    # /tmp must remain usable for ordinary scratch-file work despite the
    # read-only root filesystem -- proves --tmpfs=/tmp actually took
    # effect, not just that --read-only didn't break everything.
    result = asyncio.run(
        run_in_sandbox(
            image=_IMAGE,
            command=["sh", "-c", "echo data > /tmp/scratch && cat /tmp/scratch"],
            timeout_s=15,
        )
    )
    assert result.timed_out is False
    assert result.exit_code == 0
    assert "data" in result.stdout


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
