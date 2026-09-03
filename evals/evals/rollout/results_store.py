"""Append-only JSONL Results Store for `Run`s.

No new infrastructure — a flat file, per docs/rfcs/2026-09-04-evals-rollout-
scheduler.md's precedent research (Inspect AI and promptfoo both ship this
exact shape, filesystem-only, no DB/MQ, as their default production mode).
One `Run` per line; `append_runs` never truncates or overwrites an existing
file, so repeated suite runs accumulate rather than clobber history.
"""

from __future__ import annotations

from pathlib import Path

from evals.models import Run


def append_runs(runs: list[Run], path: Path) -> None:
    """Append each `Run` in `runs` to `path` as one JSON line."""
    with path.open("a", encoding="utf-8") as f:
        for run in runs:
            f.write(run.model_dump_json() + "\n")


def load_runs(path: Path) -> list[Run]:
    """Load every `Run` recorded at `path`, in append order.

    Returns an empty list if `path` doesn't exist yet — a fresh results
    file is a valid, unsurprising starting state, not an error.
    """
    if not path.exists():
        return []
    with path.open(encoding="utf-8") as f:
        return [Run.model_validate_json(line) for line in f if line.strip()]
