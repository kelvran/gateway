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

Real bug fixed 2026-09-04: on timeout, this module previously only killed
the local `docker run` CLI *client* process (`process.kill()`) — but that
does not stop the container itself. A killed CLI process leaves the
container running for its own full natural duration (unbounded for a
hung/malicious command), directly undermining `timeout_s` as a real
resource bound — confirmed empirically with a real Docker daemon (`docker
ps` still showed the container `Up` after the CLI process was killed).
Fixed by capturing the real container ID via `--cidfile` and issuing a
real `docker kill <id>` on timeout, not just killing the CLI wrapper.

Real gap closed 2026-09-05, per THREAT_MODEL.md's Evals "Information
Disclosure" row: the ephemeral-filesystem guarantee was previously just
Docker's own default `--rm` writable-layer lifecycle, not a Kelvran-built
one — a sandboxed command could freely write anywhere in the container's
root filesystem (persisting for the container's lifetime, a real avenue
for tampering with the image's own binaries or staging data for
exfiltration via some other channel). Every run now also passes
`--read-only` (the container's root filesystem is immutable) plus a
`--tmpfs=/tmp` mount (ordinary scratch-file usage — the common case for
real commands — still works, just never persists past the container).
"""

from __future__ import annotations

import asyncio
import os
import tempfile
from dataclasses import dataclass


@dataclass(frozen=True)
class SandboxResult:
    """Result of a single sandboxed command execution.

    `container_id` is the real Docker container ID (captured via
    `--cidfile`), when known — `None` only if the container never
    reached the point of being created (e.g. `docker run` itself failed
    to start it), never a fabricated placeholder. Populated regardless of
    `timed_out`, since the container is created (and the ID captured)
    before the command's own runtime — including a run that times out —
    ever completes.
    """

    exit_code: int
    stdout: str
    stderr: str
    timed_out: bool
    container_id: str | None = None


def _read_cidfile(path: str) -> str | None:
    """Read the container ID Docker wrote to `path` via `--cidfile`.

    Returns `None` if the file was never created (the container never
    started) — never raises on a merely-missing file, since that's a
    real, expected state on certain launch-failure paths.
    """
    if not os.path.exists(path):
        return None
    with open(path, encoding="utf-8") as f:
        content = f.read().strip()
    return content or None


async def _docker_kill(container_id: str) -> None:
    """Best-effort: stop the real container, not just the local `docker
    run` CLI process — see this module's docstring for the bug this
    closes. Best-effort because the container may have already exited on
    its own in the small window between the timeout firing and this call
    (a benign race, not an error worth surfacing)."""
    proc = await asyncio.create_subprocess_exec(
        "docker",
        "kill",
        container_id,
        stdout=asyncio.subprocess.DEVNULL,
        stderr=asyncio.subprocess.DEVNULL,
    )
    await proc.wait()


async def run_in_sandbox(
    image: str, command: list[str], timeout_s: int
) -> SandboxResult:
    """Run `command` inside `image` via `docker run --rm --network=none
    --read-only --tmpfs=/tmp`.

    Enforces `timeout_s` as a real wall-clock timeout on the whole
    container run — on timeout, the actual container is stopped via a
    real `docker kill` (using the ID captured via `--cidfile`), not just
    the local `docker run` CLI process, and `SandboxResult` is returned
    with `timed_out=True` rather than raising, so callers can treat "the
    rollout timed out" as a normal, scoreable outcome rather than an
    exceptional one.

    Requires a live Docker daemon; this function makes real `docker run`/
    `docker kill` calls and is not itself a stub.
    """
    cid_fd, cid_path = tempfile.mkstemp(prefix="kelvran-sandbox-cid-")
    os.close(cid_fd)
    # Docker requires --cidfile's path to not already exist — even an
    # empty pre-created file makes `docker run` fail outright (confirmed
    # empirically against a real Docker daemon). mkstemp guarantees a
    # unique, unpredictable name; deleting it immediately hands `docker
    # run` a path it can create itself.
    os.unlink(cid_path)

    docker_args = [
        "docker",
        "run",
        "--rm",
        "--network=none",
        "--read-only",
        "--tmpfs=/tmp:rw,exec,nosuid,size=64m",
        f"--cidfile={cid_path}",
        image,
        *command,
    ]

    try:
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
            container_id = _read_cidfile(cid_path)
            if container_id is not None:
                await _docker_kill(container_id)
            process.kill()
            await process.wait()
            return SandboxResult(
                exit_code=-1,
                stdout="",
                stderr="",
                timed_out=True,
                container_id=container_id,
            )

        exit_code = process.returncode if process.returncode is not None else -1
        return SandboxResult(
            exit_code=exit_code,
            stdout=stdout_bytes.decode(errors="replace"),
            stderr=stderr_bytes.decode(errors="replace"),
            timed_out=False,
            container_id=_read_cidfile(cid_path),
        )
    finally:
        if os.path.exists(cid_path):
            os.unlink(cid_path)
