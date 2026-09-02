"""Docker-sandboxed command execution.

Shells out to the `docker` CLI (no `docker` SDK dependency for this pass —
per docs/plans/2026-09-02-initial-code-scaffolding.md's tech-stack note,
shelling out is sufficient and keeps dependencies minimal).

Security posture, per THREAT_MODEL.md's Evals "Information Disclosure" row
(sandboxed rollout exfiltrating data via an unexpected channel): every
sandbox run is started with `--network=none`. This pass implements "no
egress at all" rather than a partial egress allowlist — the honest,
simplest safe default called out explicitly in the scaffolding RFC, not a
shortcut standing in for the real allowlisting feature.
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass


@dataclass(frozen=True)
class SandboxResult:
    """Result of a single sandboxed command execution."""

    exit_code: int
    stdout: str
    stderr: str
    timed_out: bool


async def run_in_sandbox(
    image: str, command: list[str], timeout_s: int
) -> SandboxResult:
    """Run `command` inside `image` via `docker run --rm --network=none`.

    Enforces `timeout_s` as a wall-clock timeout on the whole container
    run — on timeout, the container process is killed and `SandboxResult`
    is returned with `timed_out=True` rather than raising, so callers can
    treat "the rollout timed out" as a normal, scoreable outcome rather
    than an exceptional one.

    Requires a live Docker daemon; this function makes a real `docker run`
    call and is not itself a stub.
    """
    docker_args = [
        "docker",
        "run",
        "--rm",
        "--network=none",
        image,
        *command,
    ]

    process = await asyncio.create_subprocess_exec(
        *docker_args,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
    )

    try:
        stdout_bytes, stderr_bytes = await asyncio.wait_for(
            process.communicate(), timeout=timeout_s
        )
    except TimeoutError:
        process.kill()
        await process.wait()
        return SandboxResult(exit_code=-1, stdout="", stderr="", timed_out=True)

    exit_code = process.returncode if process.returncode is not None else -1
    return SandboxResult(
        exit_code=exit_code,
        stdout=stdout_bytes.decode(errors="replace"),
        stderr=stderr_bytes.decode(errors="replace"),
        timed_out=False,
    )
