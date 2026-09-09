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


def compute_score_cache_key(
    output: str,
    reference: str,
    scorer_id: str,
    axis: str | None = None,
    debias: bool = False,
) -> str:
    """SHA-256 hex digest over the canonical JSON of
    `{output, reference, scorer_id, axis, debias}` — a judge verdict is a
    function of exactly these five inputs, plus the judge prompt template
    baked into `evals.judge.llm_judge.judge()` itself. Like
    `compute_run_cache_key`'s own deliberate omission of a harness/runner
    code version, this key does NOT version the prompt template — a
    prompt change is a code change, not a config change, and invalidating
    stale cached verdicts after one is the operator's responsibility
    (e.g. by pointing `--scores` at a fresh file), not solved with an
    unrequested prompt-hash field here.

    `axis` (added 2026-09-05, per docs/rfcs/2026-09-05-evals-multi-axis-
    judging.md) is included in the key, but ONLY when given, so a cached
    "correctness" verdict for a given output can never be mistakenly
    served for a "safety" query against that same output — two genuinely
    different judgments, even though `output`/`reference`/`scorer_id` are
    identical. Omitting `axis` (the default, `None`) reproduces the exact
    original 3-field canonical JSON byte-for-byte, so every
    `score_cache_key` computed before this parameter existed still
    matches — a real backward-compatibility property, not just an
    unenforced intention, verified by
    `test_omitting_axis_matches_the_original_pre_axis_key_exactly`.

    `debias` (added 2026-09-09, per docs/rfcs/2026-09-09-evals-judge-
    debiasing-position-swap.md) mirrors `axis`'s exact same pattern for
    the identical reason: a debiased (position-swapped, two-call)
    verdict and a non-debiased (single-call) verdict for the identical
    `(output, reference, scorer_id, axis)` are genuinely different
    judgments — a debiased-off run flipping `--judge-debias` on later
    must never reuse the other's cached verdict. Included in the key
    ONLY when `True` (the default `False` is omitted), so every
    `score_cache_key` computed before this parameter existed still
    matches byte-for-byte.

    Canonicalization matches `compute_run_cache_key`'s own RFC-8785-style
    choices (`sort_keys`, no whitespace, ASCII-only, NaN/Infinity
    rejected) for the same stable-hashing correctness reasons.
    """
    fields: dict[str, str | bool] = {
        "output": output,
        "reference": reference,
        "scorer_id": scorer_id,
    }
    if axis is not None:
        fields["axis"] = axis
    if debias:
        fields["debias"] = True
    canonical = json.dumps(
        fields,
        sort_keys=True,
        ensure_ascii=True,
        allow_nan=False,
        separators=(",", ":"),
    )
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()
