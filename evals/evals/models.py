"""Data model for evaluation cases.

Matches evals/ARCHITECTURE.md's Data Model sketch:

    EvalCase { id, revision, task_spec, reference | null, tier: golden|regression|drift_sample, tags }

`EvalCase` instances are immutable once created — a stable `id` at a given
`revision` never changes shape out from under a dataset. Advancing a case to
a new revision is done via `with_revision()`, which returns a brand-new
instance rather than mutating the original in place, per the org-wide
immutability convention.
"""

from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field

EvalTier = Literal["golden", "regression", "drift_sample"]


class EvalCase(BaseModel):
    """A single, versioned evaluation case.

    `tier` is set at dataset-registration time (see THREAT_MODEL.md's Evals
    "Spoofing" row: a rollout must never be able to claim its own tier).
    """

    model_config = ConfigDict(frozen=True)

    id: str
    revision: int
    task_spec: dict
    reference: str | None = None
    tier: EvalTier
    tags: list[str] = Field(default_factory=list)

    def with_revision(self, revision: int) -> "EvalCase":
        """Return a new `EvalCase` at `revision`, leaving `self` untouched.

        Never mutates `self` — revisions are immutable once created.
        """
        return self.model_copy(update={"revision": revision})
