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

Real gap closed 2026-09-11 (a round-4 backlog-audit finding): this
module's own `docker run` invocation had `--network=none`/`--read-only`/
`--tmpfs` hardening but no CPU/memory/process-count bound at all — the
scheduler is deliberately sequential (`evals.rollout.scheduler.run_suite`'s
own module docstring: "one `EvalCase` maps to exactly one
`run_in_sandbox()` call, executed one at a time"), so a single sandboxed
command with an unbounded memory allocation or a fork bomb was a real,
completely unmitigated host-level DoS against the machine running `evals
rollout` itself — not merely a slow test. `EvalCase.task_spec`'s v1
contract (`image`/`command`/`timeout_s` only, per `Run.harness_config`'s
own doc comment) has no field to opt out of these limits either, matching
`--network=none`'s own "no allowlist, no opt-out" posture rather than
`timeout_s`'s per-case-configurable one — these are hard resource ceilings,
not a tunable knob a case author is expected to reach for.

Real gap closed 2026-09-21: the `TimeoutError`/`BaseException` cleanup
branches below each ran their own `_read_cidfile`/`_docker_kill`/
`process.kill()`/`process.wait()` sequence directly inside the `except`
block — an unexpected failure ANYWHERE in that sequence (the `docker`
binary vanishing mid-run, a transient `OSError` reading the cidfile)
propagated straight out, REPLACING the original timeout/cancellation
signal with an unrelated teardown exception. On the timeout path this
meant a caller could see a raised exception where `run_in_sandbox`'s own
docstring promises a normal `SandboxResult(timed_out=True)` return value
instead; on the cancellation path it meant the real interruption
(`asyncio.CancelledError`, or whatever else this function was unwinding
from) was silently masked by whatever the cleanup itself happened to
raise. Latent in already-shipped code, real today even at the current
concurrency level of 1 (a single sandboxed run can still be interrupted
mid-flight) — independent of whether a future Sandbox Pool ever exists.
Fixed by `_cleanup_after_interruption`, shared by both branches: every
step of the cleanup sequence is now caught and swallowed (never logged —
see that function's own doc comment for why), so the caller always sees
the ORIGINAL signal, never a teardown artifact.
"""

from __future__ import annotations

import asyncio
import os
import tempfile
from dataclasses import dataclass

# Hard resource ceilings for every real sandbox run -- see this module's
# own "Real gap closed 2026-09-11" docstring section for why these exist
# and why they are NOT case-configurable (unlike DEFAULT_SANDBOX_TIMEOUT_S
# in evals.rollout.scheduler, which a case's own task_spec CAN override).
# 512 MiB / 1 CPU / 128 processes is generous enough for the ordinary
# shell/script commands this harness targets while still bounding a
# memory allocation or fork bomb to a fraction of a typical host's real
# capacity.
DEFAULT_SANDBOX_MEMORY_MB = 512
DEFAULT_SANDBOX_CPUS = 1
DEFAULT_SANDBOX_PIDS_LIMIT = 128


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


async def _cleanup_after_interruption(
    cid_path: str, process: asyncio.subprocess.Process
) -> str | None:
    """Best-effort teardown shared by both the `TimeoutError` and
    `BaseException` branches below: read the cidfile, kill the real
    container, then kill+wait the local `docker run` CLI process.

    Real gap closed 2026-09-21: every step here already has its OWN
    best-effort posture (see `_docker_kill`'s docstring for the
    container-already-exited race) — but an UNEXPECTED failure anywhere
    in this sequence (the `docker` binary vanishing mid-run, a transient
    `OSError` reading the cidfile, `process.kill()` racing an
    already-reaped process) previously propagated straight out of the
    `except` block it ran inside, REPLACING the original
    timeout/cancellation signal the caller was already unwinding with —
    silently hiding that a timeout/cancellation ever happened at all.
    Every exception here is now caught and swallowed, never logged: this
    module has no logging dependency anywhere else (this codebase's own
    observability mechanism is `evals/tracing.py`'s OTel spans, built one
    layer above this function, around the whole `run_in_sandbox()` call —
    not inside it), and the caller's own `TimeoutError`-derived
    `SandboxResult`/re-raised original exception is already the
    complete, correct signal; a teardown failure adds no information a
    caller could act on differently.

    Deliberately `except Exception`, not `except BaseException` — a
    FRESH `asyncio.CancelledError`/`KeyboardInterrupt` raised DURING this
    cleanup itself is a genuinely NEW interruption, not the masking bug
    this closes, and must still propagate normally.
    """
    container_id = None
    try:
        container_id = _read_cidfile(cid_path)
        if container_id is not None:
            await _docker_kill(container_id)
        process.kill()
        await process.wait()
    # Deliberate silent swallow, not a logging omission -- see this
    # function's own doc comment: this module has no logging dependency
    # anywhere else, and the caller's own TimeoutError-derived result/
    # re-raised original exception is already the complete signal: a
    # teardown failure adds nothing actionable to it.
    except Exception:  # noqa: S110
        pass
    return container_id


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
        f"--memory={DEFAULT_SANDBOX_MEMORY_MB}m",
        f"--memory-swap={DEFAULT_SANDBOX_MEMORY_MB}m",
        f"--cpus={DEFAULT_SANDBOX_CPUS}",
        f"--pids-limit={DEFAULT_SANDBOX_PIDS_LIMIT}",
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
            container_id = await _cleanup_after_interruption(cid_path, process)
            return SandboxResult(
                exit_code=-1,
                stdout="",
                stderr="",
                timed_out=True,
                container_id=container_id,
            )
        except BaseException:
            # A round-4 backlog-audit finding: asyncio.CancelledError (a
            # real, ordinary trigger since Python 3.11's asyncio.run
            # installs a SIGINT handler that cancels the running task on
            # Ctrl-C -- landing exactly here if that's the current
            # suspension point) is a BaseException, not an Exception, so
            # only catching TimeoutError above left the real container
            # (already created before this point, per this function's
            # own docstring) running unbounded on ANY other interruption
            # -- not just a genuine timeout. Mirrors the TimeoutError
            # branch's own real docker-kill cleanup, then re-raises
            # unchanged (never swallowed) so the caller still sees the
            # real interruption/error -- see _cleanup_after_interruption's
            # own doc comment for why a teardown failure specifically
            # must never override THIS re-raise.
            await _cleanup_after_interruption(cid_path, process)
            raise

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
