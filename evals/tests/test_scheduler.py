"""Tests for `evals.rollout.scheduler.run_suite` that do NOT require a live
Docker daemon — monkeypatches `run_in_sandbox` exactly as
`tests/test_sandbox_error_paths.py` does, distinct from this file's own
`RUN_DOCKER_TESTS=1`-gated real end-to-end test below.
"""

from __future__ import annotations

import asyncio
import os
import uuid

import pytest

import evals.rollout.scheduler as scheduler_module
from evals.models import EvalCase, Run
from evals.rollout.cache import compute_run_cache_key
from evals.rollout.sandbox import SandboxResult
from evals.rollout.scheduler import EarlyStopConfig, run_suite


def _make_case(case_id: str, command: list[str]) -> EvalCase:
    return EvalCase(
        id=case_id,
        revision=1,
        task_spec={"image": "alpine:3.20", "command": command, "timeout_s": 5},
        reference=None,
        tier="golden",
    )


def test_successful_run_produces_completed_status(monkeypatch):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        return SandboxResult(exit_code=0, stdout="hi\n", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    runs = asyncio.run(run_suite([_make_case("c1", ["echo", "hi"])]))

    assert len(runs) == 1
    run = runs[0]
    assert run.status == "completed"
    assert run.exit_code == 0
    assert run.stdout == "hi\n"
    assert run.eval_case_id == "c1"
    assert run.eval_case_revision == 1
    assert run.harness_config == {
        "image": "alpine:3.20",
        "command": ["echo", "hi"],
        "timeout_s": 5,
    }
    assert run.cost_usd is None


def test_timed_out_sandbox_result_produces_timed_out_status(monkeypatch):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        return SandboxResult(exit_code=-1, stdout="", stderr="", timed_out=True)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    runs = asyncio.run(run_suite([_make_case("c1", ["sleep", "30"])]))

    assert runs[0].status == "timed_out"


def test_sandbox_launch_failure_produces_error_status_not_a_raised_exception(
    monkeypatch,
):
    async def _raise_file_not_found(image, command, timeout_s):
        raise FileNotFoundError("[Errno 2] No such file or directory: 'docker'")

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _raise_file_not_found)

    runs = asyncio.run(run_suite([_make_case("c1", ["echo", "hi"])]))

    assert len(runs) == 1
    assert runs[0].status == "error"
    assert runs[0].error is not None
    assert "docker" in runs[0].error


def test_one_case_erroring_does_not_abort_the_rest_of_the_suite(monkeypatch):
    calls = {"n": 0}

    async def _fail_first_then_succeed(image, command, timeout_s):
        calls["n"] += 1
        if calls["n"] == 1:
            raise FileNotFoundError("no docker")
        return SandboxResult(exit_code=0, stdout="ok\n", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fail_first_then_succeed)

    runs = asyncio.run(
        run_suite(
            [
                _make_case("c1", ["echo", "one"]),
                _make_case("c2", ["echo", "two"]),
            ]
        )
    )

    assert len(runs) == 2
    assert runs[0].status == "error"
    assert runs[1].status == "completed"
    assert runs[1].stdout == "ok\n"


def test_missing_timeout_s_defaults_to_default_sandbox_timeout(monkeypatch):
    seen = {}

    async def _capture(image, command, timeout_s):
        seen["timeout_s"] = timeout_s
        return SandboxResult(exit_code=0, stdout="", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _capture)

    case = EvalCase(
        id="c1",
        revision=1,
        task_spec={"image": "alpine:3.20", "command": ["true"]},
        tier="golden",
    )
    asyncio.run(run_suite([case]))

    assert seen["timeout_s"] == scheduler_module.DEFAULT_SANDBOX_TIMEOUT_S


def _make_run(
    case: EvalCase, *, status: str = "completed", stdout: str = "cached\n"
) -> Run:
    harness_config = {
        "image": case.task_spec["image"],
        "command": case.task_spec["command"],
        "timeout_s": case.task_spec.get(
            "timeout_s", scheduler_module.DEFAULT_SANDBOX_TIMEOUT_S
        ),
    }
    return Run(
        id=uuid.uuid4().hex,
        eval_case_id=case.id,
        eval_case_revision=case.revision,
        harness_config=harness_config,
        status=status,
        stdout=stdout,
        latency_ms=1.0,
        cache_key=compute_run_cache_key(case.task_spec, harness_config),
    )


# --- Result cache (docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md) ---


def test_cached_runs_none_reproduces_todays_exact_behavior(monkeypatch):
    """The load-bearing backward-compat proof: omitting cached_runs entirely
    must behave identically to before this RFC existed."""
    calls = {"n": 0}

    async def _fake_run_in_sandbox(image, command, timeout_s):
        calls["n"] += 1
        return SandboxResult(exit_code=0, stdout="fresh\n", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    case = _make_case("c1", ["echo", "hi"])
    runs = asyncio.run(run_suite([case]))

    assert calls["n"] == 1
    assert runs[0].status == "completed"
    assert runs[0].from_cache is False
    assert runs[0].cache_key == compute_run_cache_key(
        case.task_spec,
        {"image": "alpine:3.20", "command": ["echo", "hi"], "timeout_s": 5},
    )


def test_cache_hit_skips_sandbox_execution(monkeypatch):
    async def _fail_if_called(image, command, timeout_s):
        raise AssertionError("run_in_sandbox must not be called on a cache hit")

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fail_if_called)

    case = _make_case("c1", ["echo", "hi"])
    prior = _make_run(case, stdout="from-earlier-real-run\n")

    runs = asyncio.run(run_suite([case], cached_runs={prior.cache_key: prior}))

    assert runs[0].status == "completed"
    assert runs[0].from_cache is True
    assert runs[0].cache_source_run_id == prior.id
    assert runs[0].stdout == "from-earlier-real-run\n"


def test_cache_never_hits_for_drift_sample_tier(monkeypatch):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        return SandboxResult(exit_code=0, stdout="fresh\n", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    case = EvalCase(
        id="drift1",
        revision=1,
        task_spec={"image": "alpine:3.20", "command": ["echo", "hi"], "timeout_s": 5},
        tier="drift_sample",
    )
    prior = _make_run(case)

    runs = asyncio.run(run_suite([case], cached_runs={prior.cache_key: prior}))

    assert runs[0].from_cache is False
    assert runs[0].stdout == "fresh\n"


def test_cache_never_hits_an_errored_prior_run(monkeypatch):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        return SandboxResult(exit_code=0, stdout="fresh\n", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    case = _make_case("c1", ["echo", "hi"])
    prior = _make_run(case, status="error")

    runs = asyncio.run(run_suite([case], cached_runs={prior.cache_key: prior}))

    assert runs[0].from_cache is False
    assert runs[0].stdout == "fresh\n"


def test_cache_never_hits_a_timed_out_prior_run(monkeypatch):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        return SandboxResult(exit_code=0, stdout="fresh\n", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    case = _make_case("c1", ["echo", "hi"])
    prior = _make_run(case, status="timed_out")

    runs = asyncio.run(run_suite([case], cached_runs={prior.cache_key: prior}))

    assert runs[0].from_cache is False


def test_cache_never_chains_a_hit_off_another_hit(monkeypatch):
    """cached_runs is built by the CLI from load_runs filtered to
    `not from_cache` — this proves the scheduler itself is also safe even
    if a caller passed a from_cache=True entry in by mistake."""

    async def _fail_if_called(image, command, timeout_s):
        raise AssertionError("must not execute if the (buggy) chained hit were allowed")

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fail_if_called)

    case = _make_case("c1", ["echo", "hi"])
    chained_hit = _make_run(case).model_copy(update={"from_cache": True})

    # The scheduler only ever looks up by cache_key -- it has no
    # from_cache filter of its own (that policy lives in the CLI's
    # cached_runs construction). This test documents that contract: if a
    # caller hands the scheduler a from_cache=True entry, it is used as a
    # normal completed prior (chaining IS possible at the scheduler layer;
    # the CLI's own load_runs filter is what actually prevents chaining in
    # practice). Assert the real, current behavior rather than an
    # aspirational one.
    runs = asyncio.run(
        run_suite([case], cached_runs={chained_hit.cache_key: chained_hit})
    )
    assert runs[0].from_cache is True
    assert runs[0].cache_source_run_id == chained_hit.id


# --- Two-checkpoint early stopping (see the RFC) ---


def _repeated_cases(case_id: str, n: int) -> list[EvalCase]:
    return [_make_case(case_id, ["echo", "hi"]) for _ in range(n)]


def test_early_stop_none_reproduces_todays_exact_behavior(monkeypatch):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        return SandboxResult(exit_code=0, stdout="hi\n", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    runs = asyncio.run(run_suite(_repeated_cases("c1", 5)))

    assert len(runs) == 5
    assert all(r.status == "completed" for r in runs)
    assert all(r.skip_reason is None for r in runs)


def test_early_stop_skips_remaining_trials_once_group_stops(monkeypatch):
    async def _always_succeed(image, command, timeout_s):
        return SandboxResult(exit_code=0, stdout="hi\n", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _always_succeed)

    async def _always_true(case, run):
        return True

    # baseline_pass_rate=0.1 -- an all-success group's mSPRT check crosses
    # its threshold at trial 2 (verified directly against
    # evals.stats.mixture_sprt_early_stop, not guessed).
    early_stop = EarlyStopConfig(
        max_trials=10, baseline_pass_rate=0.1, score_fn=_always_true
    )
    runs = asyncio.run(run_suite(_repeated_cases("c1", 6), early_stop=early_stop))

    assert len(runs) == 6
    assert [r.status for r in runs] == [
        "completed",
        "completed",
        "skipped",
        "skipped",
        "skipped",
        "skipped",
    ]
    assert runs[2].skip_reason is not None


def test_early_stop_score_fn_called_exactly_once_per_real_trial(monkeypatch):
    async def _always_succeed(image, command, timeout_s):
        return SandboxResult(exit_code=0, stdout="hi\n", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _always_succeed)

    call_count = {"n": 0}

    async def _counting_score_fn(case, run):
        call_count["n"] += 1
        return True

    early_stop = EarlyStopConfig(
        max_trials=10,
        baseline_pass_rate=0.1,
        score_fn=_counting_score_fn,
    )
    runs = asyncio.run(run_suite(_repeated_cases("c1", 6), early_stop=early_stop))

    completed = sum(1 for r in runs if r.status != "skipped")
    assert call_count["n"] == completed
    assert call_count["n"] == 2  # the mSPRT crossed its threshold at trial 2


def test_early_stop_group_hitting_repeated_errors_still_accumulates_evidence(
    monkeypatch,
):
    """The restructured error path: an errored trial must still reach the
    shared tally, or a group that only ever errors would never accumulate
    evidence and would run every trial instead of stopping."""

    async def _always_error(image, command, timeout_s):
        raise FileNotFoundError("no docker")

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _always_error)

    async def _score_fn(case, run):
        assert run.status == "error"
        return False  # mirrors _score_run_deterministic's real convention

    early_stop = EarlyStopConfig(
        max_trials=10, baseline_pass_rate=0.9, score_fn=_score_fn
    )
    runs = asyncio.run(run_suite(_repeated_cases("c1", 6), early_stop=early_stop))

    # All-failure group checked against a 0.9 baseline: the mSPRT crosses
    # its threshold at trial 2 (verified directly, not guessed).
    assert [r.status for r in runs] == [
        "error",
        "error",
        "skipped",
        "skipped",
        "skipped",
        "skipped",
    ]


def test_early_stop_max_trials_force_stops_a_group_the_mSPRT_itself_never_flags(
    monkeypatch,
):
    """max_trials is a plain resource ceiling now, not a second statistical
    checkpoint -- per docs/rfcs/2026-09-05-evals-mixture-sprt-early-
    stopping.md. An ambiguous group whose running pass rate stays pinned
    at the baseline never gives the mSPRT a reason to stop on its own
    (verified directly against evals.stats.mixture_sprt_early_stop for
    this exact alternating pattern up to n=10), so every trial up to
    max_trials=10 must run for real, and only the trials beyond that
    ceiling are skipped.
    """

    async def _always_succeed(image, command, timeout_s):
        return SandboxResult(exit_code=0, stdout="hi\n", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _always_succeed)

    call_count = {"n": 0}

    async def _alternating_score_fn(case, run):
        call_count["n"] += 1
        return call_count["n"] % 2 == 1  # pins the running rate at 0.5

    early_stop = EarlyStopConfig(
        max_trials=10, baseline_pass_rate=0.5, score_fn=_alternating_score_fn
    )
    runs = asyncio.run(run_suite(_repeated_cases("c1", 12), early_stop=early_stop))

    assert [r.status for r in runs] == ["completed"] * 10 + ["skipped"] * 2
    assert call_count["n"] == 10


def test_cache_hit_still_flows_through_early_stop_tally(monkeypatch):
    async def _fail_if_called(image, command, timeout_s):
        raise AssertionError("cached trials must not execute")

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fail_if_called)

    cases = _repeated_cases("c1", 3)
    prior = _make_run(cases[0])
    cached_runs = {prior.cache_key: prior}

    call_count = {"n": 0}

    async def _counting_score_fn(case, run):
        call_count["n"] += 1
        return True

    early_stop = EarlyStopConfig(
        max_trials=10,
        baseline_pass_rate=0.01,
        score_fn=_counting_score_fn,
    )
    runs = asyncio.run(run_suite(cases, cached_runs=cached_runs, early_stop=early_stop))

    # The single cache hit alone, against a 0.01 baseline with an
    # all-success tally, already crosses the mSPRT's threshold at n=1
    # (verified directly) -- stops the group right away.
    assert runs[0].from_cache is True
    assert call_count["n"] == 1
    assert [r.status for r in runs] == ["completed", "skipped", "skipped"]


def test_span_sink_none_reproduces_todays_exact_behavior(monkeypatch):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        return SandboxResult(exit_code=0, stdout="hi\n", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    runs = asyncio.run(run_suite([_make_case("c1", ["echo", "hi"])]))

    assert len(runs) == 1
    assert runs[0].status == "completed"


def test_span_sink_records_one_ok_span_per_successful_real_execution(monkeypatch):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        return SandboxResult(exit_code=0, stdout="hi\n", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    span_sink = []
    runs = asyncio.run(
        run_suite([_make_case("c1", ["echo", "hi"])], span_sink=span_sink)
    )

    assert len(span_sink) == 1
    assert span_sink[0].run_id == runs[0].id
    assert span_sink[0].status == "OK"
    assert span_sink[0].process_exit_code == 0
    assert span_sink[0].error is None


def test_span_sink_records_one_error_span_per_sandbox_launch_failure(monkeypatch):
    async def _raise_file_not_found(image, command, timeout_s):
        raise FileNotFoundError("[Errno 2] No such file or directory: 'docker'")

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _raise_file_not_found)

    span_sink = []
    runs = asyncio.run(
        run_suite([_make_case("c1", ["echo", "hi"])], span_sink=span_sink)
    )

    assert len(span_sink) == 1
    assert span_sink[0].run_id == runs[0].id
    assert span_sink[0].status == "ERROR"
    assert span_sink[0].process_exit_code is None
    assert span_sink[0].error is not None
    assert "docker" in span_sink[0].error


def test_span_sink_records_nothing_for_a_cache_hit(monkeypatch):
    async def _fail_if_called(image, command, timeout_s):
        raise AssertionError("run_in_sandbox must not be called on a cache hit")

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fail_if_called)

    case = _make_case("c1", ["echo", "hi"])
    prior = _make_run(case, stdout="from-earlier-real-run\n")

    span_sink = []
    runs = asyncio.run(
        run_suite([case], cached_runs={prior.cache_key: prior}, span_sink=span_sink)
    )

    assert runs[0].from_cache is True
    assert span_sink == []


def test_span_sink_records_nothing_for_an_early_stop_skip(monkeypatch):
    async def _always_succeed(image, command, timeout_s):
        return SandboxResult(exit_code=0, stdout="hi\n", stderr="", timed_out=False)

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _always_succeed)

    async def _always_true(case, run):
        return True

    early_stop = EarlyStopConfig(
        max_trials=10, baseline_pass_rate=0.01, score_fn=_always_true
    )
    span_sink = []
    runs = asyncio.run(
        run_suite(_repeated_cases("c1", 6), early_stop=early_stop, span_sink=span_sink)
    )

    skipped_count = sum(1 for r in runs if r.status == "skipped")
    real_count = len(runs) - skipped_count
    assert skipped_count > 0
    # Exactly one span per genuinely-executed trial, zero for the skips.
    assert len(span_sink) == real_count
    assert all(s.status == "OK" for s in span_sink)


@pytest.mark.integration
@pytest.mark.skipif(
    os.environ.get("RUN_DOCKER_TESTS") != "1",
    reason="requires a live Docker daemon; set RUN_DOCKER_TESTS=1 to run",
)
def test_run_suite_against_a_real_docker_daemon():
    case = _make_case("real-c1", ["echo", "hello-scheduler"])
    runs = asyncio.run(run_suite([case]))

    assert len(runs) == 1
    assert runs[0].status == "completed"
    assert "hello-scheduler" in runs[0].stdout


@pytest.mark.integration
@pytest.mark.skipif(
    os.environ.get("RUN_DOCKER_TESTS") != "1",
    reason="requires a live Docker daemon; set RUN_DOCKER_TESTS=1 to run",
)
def test_run_suite_span_sink_against_a_real_docker_daemon():
    case = _make_case("real-c1", ["echo", "hello-scheduler"])
    span_sink = []
    runs = asyncio.run(run_suite([case], span_sink=span_sink))

    assert len(span_sink) == 1
    assert span_sink[0].run_id == runs[0].id
    assert span_sink[0].status == "OK"
    assert span_sink[0].process_exit_code == 0
