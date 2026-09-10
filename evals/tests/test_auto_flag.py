from evals.auto_flag import DEFAULT_RULES, FlagRule, flag_candidates
from evals.models import EvalCase, Run


def _make_case(**overrides) -> EvalCase:
    defaults = dict(
        id="case-1",
        revision=1,
        task_spec={"outcome": "OUTCOME_UPSTREAM_ERROR"},
        reference=None,
        tier="drift_sample",
        tags=[],
    )
    defaults.update(overrides)
    return EvalCase(**defaults)


def _make_run(**overrides) -> Run:
    defaults = dict(
        id="run-1",
        eval_case_id="case-1",
        eval_case_revision=1,
        harness_config={"source": "gatewayevents_v1"},
        status="error",
        latency_ms=0.0,
    )
    defaults.update(overrides)
    return Run(**defaults)


def test_case_with_matching_outcome_produces_a_candidate():
    case = _make_case()
    run = _make_run()

    candidates = flag_candidates([case], [run])

    assert len(candidates) == 1
    assert candidates[0].eval_case_id == "case-1"
    assert candidates[0].eval_case_revision == 1
    assert candidates[0].run_id == "run-1"
    assert "gateway-failure-outcome" in candidates[0].matched_rules


def test_case_with_ok_outcome_produces_no_candidate():
    case = _make_case(task_spec={"outcome": "OUTCOME_OK"})
    run = _make_run(status="completed")

    candidates = flag_candidates([case], [run])

    assert candidates == []


def test_non_drift_sample_case_produces_no_candidate_even_with_matching_outcome():
    case = _make_case(tier="regression")
    run = _make_run()

    candidates = flag_candidates([case], [run])

    assert candidates == []


def test_case_with_no_corresponding_run_is_skipped_not_a_crash():
    case = _make_case()

    candidates = flag_candidates([case], [])

    assert candidates == []


def test_case_matching_two_rules_produces_one_candidate_with_both_rule_names():
    case = _make_case(task_spec={"outcome": "OUTCOME_UPSTREAM_ERROR"})
    run = _make_run()
    second_rule = FlagRule(
        name="also-matches",
        flagged_outcomes=frozenset({"OUTCOME_UPSTREAM_ERROR"}),
    )
    rules = (*DEFAULT_RULES, second_rule)

    candidates = flag_candidates([case], [run], rules=rules)

    assert len(candidates) == 1
    assert set(candidates[0].matched_rules) == {
        "gateway-failure-outcome",
        "also-matches",
    }
