from __future__ import annotations

import hashlib
import json

from evals.judge.cache import compute_score_cache_key


def test_stable_across_repeated_calls():
    key_a = compute_score_cache_key("the output", "the reference", "some-model")
    key_b = compute_score_cache_key("the output", "the reference", "some-model")
    assert key_a == key_b


def test_changes_when_output_changes():
    key_a = compute_score_cache_key("output A", "reference", "some-model")
    key_b = compute_score_cache_key("output B", "reference", "some-model")
    assert key_a != key_b


def test_changes_when_reference_changes():
    key_a = compute_score_cache_key("output", "reference A", "some-model")
    key_b = compute_score_cache_key("output", "reference B", "some-model")
    assert key_a != key_b


def test_changes_when_scorer_id_changes():
    key_a = compute_score_cache_key("output", "reference", "model-a")
    key_b = compute_score_cache_key("output", "reference", "model-b")
    assert key_a != key_b


def test_returns_a_hex_sha256_digest():
    key = compute_score_cache_key("output", "reference", "some-model")
    assert len(key) == 64
    assert all(c in "0123456789abcdef" for c in key)


def test_omitting_axis_matches_the_original_pre_axis_key_exactly():
    # A real backward-compatibility proof, not just an intention: every
    # score_cache_key computed before the axis parameter existed must
    # still match today, byte-for-byte, so a --scores file from before
    # this pass remains a valid --use-score-cache source.
    original_canonical = json.dumps(
        {"output": "out", "reference": "ref", "scorer_id": "model"},
        sort_keys=True,
        ensure_ascii=True,
        allow_nan=False,
        separators=(",", ":"),
    )
    original_key = hashlib.sha256(original_canonical.encode("utf-8")).hexdigest()

    assert compute_score_cache_key("out", "ref", "model") == original_key
    assert compute_score_cache_key("out", "ref", "model", axis=None) == original_key


def test_axis_changes_the_key_from_the_no_axis_version():
    no_axis = compute_score_cache_key("output", "reference", "some-model")
    with_axis = compute_score_cache_key(
        "output", "reference", "some-model", axis="correctness"
    )
    assert no_axis != with_axis


def test_different_axes_on_the_same_output_never_collide():
    key_a = compute_score_cache_key(
        "output", "reference", "some-model", axis="correctness"
    )
    key_b = compute_score_cache_key("output", "reference", "some-model", axis="safety")
    assert key_a != key_b
