"""Append-only JSONL Results Store — generic over any frozen pydantic model
this package produces (`Run`, `Score`).

Moved here from `evals/rollout/` (per docs/rfcs/2026-09-04-evals-score-
model.md): the append/load mechanism was never rollout-specific — `Run` was
just its first caller. `evals run` (no rollout, no sandbox) is a second,
equally valid caller for `Score`, so a module namespaced under `rollout/`
was no longer an honest home for it. `kelvran-evals` has never published to
PyPI, so there is no external consumer of the old import path to preserve.

No new infrastructure — a flat file. This matches Inspect AI's own real
precedent (run and score data both live in one flat log file by default, no
DB). promptfoo's actual persistence is a serverless *embedded SQLite*
database (via better-sqlite3/Drizzle), not flat files — a corrected
distinction from this module's prior docstring, which had attributed
"filesystem-only, no DB" to promptfoo as well.

One model per line; `append_*` never truncates or overwrites an existing
file, so repeated suite runs accumulate rather than clobber history.
"""

from __future__ import annotations

from pathlib import Path

from pydantic import BaseModel

from evals.models import Run, Score


def _append_models[ModelT: BaseModel](models: list[ModelT], path: Path) -> None:
    with path.open("a", encoding="utf-8") as f:
        for model in models:
            f.write(model.model_dump_json() + "\n")


def _load_models[ModelT: BaseModel](
    model_cls: type[ModelT], path: Path
) -> list[ModelT]:
    """Load every `model_cls` instance recorded at `path`, in append order.

    Returns an empty list if `path` doesn't exist yet — a fresh results
    file is a valid, unsurprising starting state, not an error.
    """
    if not path.exists():
        return []
    with path.open(encoding="utf-8") as f:
        return [model_cls.model_validate_json(line) for line in f if line.strip()]


def append_runs(runs: list[Run], path: Path) -> None:
    """Append each `Run` in `runs` to `path` as one JSON line."""
    _append_models(runs, path)


def load_runs(path: Path) -> list[Run]:
    """Load every `Run` recorded at `path`, in append order."""
    return _load_models(Run, path)


def append_scores(scores: list[Score], path: Path) -> None:
    """Append each `Score` in `scores` to `path` as one JSON line."""
    _append_models(scores, path)


def load_scores(path: Path) -> list[Score]:
    """Load every `Score` recorded at `path`, in append order."""
    return _load_models(Score, path)
