from __future__ import annotations

from evals.rollout.cache import compute_run_cache_key


def test_stable_across_dict_key_insertion_order():
    task_spec = {"image": "alpine:3.20", "command": ["echo", "hi"]}
    harness_config_a = {
        "image": "alpine:3.20",
        "command": ["echo", "hi"],
        "timeout_s": 5,
    }
    harness_config_b = {
        "timeout_s": 5,
        "command": ["echo", "hi"],
        "image": "alpine:3.20",
    }

    assert compute_run_cache_key(task_spec, harness_config_a) == compute_run_cache_key(
        task_spec, harness_config_b
    )


def test_changes_when_task_spec_image_changes():
    harness_config = {"image": "alpine:3.20", "command": ["echo", "hi"], "timeout_s": 5}
    key_a = compute_run_cache_key({"image": "alpine:3.20"}, harness_config)
    key_b = compute_run_cache_key({"image": "alpine:3.21"}, harness_config)
    assert key_a != key_b


def test_changes_when_command_changes():
    task_spec = {"image": "alpine:3.20"}
    key_a = compute_run_cache_key(
        task_spec, {"image": "alpine:3.20", "command": ["echo", "hi"], "timeout_s": 5}
    )
    key_b = compute_run_cache_key(
        task_spec, {"image": "alpine:3.20", "command": ["echo", "bye"], "timeout_s": 5}
    )
    assert key_a != key_b


def test_changes_when_timeout_s_changes():
    task_spec = {"image": "alpine:3.20"}
    key_a = compute_run_cache_key(
        task_spec, {"image": "alpine:3.20", "command": ["echo"], "timeout_s": 5}
    )
    key_b = compute_run_cache_key(
        task_spec, {"image": "alpine:3.20", "command": ["echo"], "timeout_s": 30}
    )
    assert key_a != key_b


def test_returns_a_hex_sha256_digest():
    key = compute_run_cache_key({"image": "alpine"}, {"command": ["true"]})
    assert len(key) == 64
    assert all(c in "0123456789abcdef" for c in key)
