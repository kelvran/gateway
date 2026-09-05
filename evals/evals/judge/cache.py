"""Score-cache key fabrication for LLM-judge calls.

Deliberately a pure function with no I/O and no opinion on whether
caching is a good idea for a given case — that policy (opt-in via
`--use-score-cache`, built once from `--scores` before any new `Score` is
appended this run) lives in `evals.cli`, mirroring
`evals.rollout.cache.compute_run_cache_key`'s own exact precedent for
Run-level caching.
"""

from __future__ import annotations

import hashlib
import json


def compute_score_cache_key(output: str, reference: str, scorer_id: str) -> str:
    """SHA-256 hex digest over the canonical JSON of
    `{output, reference, scorer_id}` — a judge verdict is a function of
    exactly these three inputs, plus the judge prompt template baked into
    `evals.judge.llm_judge.judge()` itself. Like
    `compute_run_cache_key`'s own deliberate omission of a harness/runner
    code version, this key does NOT version the prompt template — a
    prompt change is a code change, not a config change, and invalidating
    stale cached verdicts after one is the operator's responsibility
    (e.g. by pointing `--scores` at a fresh file), not solved with an
    unrequested prompt-hash field here.

    Canonicalization matches `compute_run_cache_key`'s own RFC-8785-style
    choices (`sort_keys`, no whitespace, ASCII-only, NaN/Infinity
    rejected) for the same stable-hashing correctness reasons.
    """
    canonical = json.dumps(
        {"output": output, "reference": reference, "scorer_id": scorer_id},
        sort_keys=True,
        ensure_ascii=True,
        allow_nan=False,
        separators=(",", ":"),
    )
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()
