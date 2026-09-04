"""Result-cache key fabrication for the Rollout Scheduler.

Deliberately a pure function with no I/O and no opinion on whether caching
is a good idea for a given `EvalCase` — that policy (opt-in only, never for
`tier="drift_sample"`) lives in `evals.rollout.scheduler.run_suite`, per
docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md.
"""

from __future__ import annotations

import hashlib
import json


def compute_run_cache_key(task_spec: dict, harness_config: dict) -> str:
    """SHA-256 hex digest over the canonical JSON of `{task_spec, harness_config}`.

    Canonicalization (`sort_keys`, no whitespace, ASCII-only, NaN/Infinity
    rejected) follows RFC 8785 (JSON Canonicalization Scheme)'s real,
    documented correctness concerns for stable hashing — not an ad hoc
    choice.

    Deliberately does NOT include a harness/runner code version or Docker
    image digest — only the image *tag* already inside `harness_config`. A
    mutable tag rebuilt with different content could produce a silent
    stale hit; pinning to an immutable digest (in `task_spec["image"]`
    itself) is the operator's responsibility, named as a real, deliberately
    unaddressed limitation in the RFC's Unresolved Questions, not solved
    with an unrequested new field here.
    """
    canonical = json.dumps(
        {"task_spec": task_spec, "harness_config": harness_config},
        sort_keys=True,
        ensure_ascii=True,
        allow_nan=False,
        separators=(",", ":"),
    )
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()
