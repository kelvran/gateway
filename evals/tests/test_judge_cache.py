from __future__ import annotations

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
