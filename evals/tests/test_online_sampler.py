"""Unit tests for `evals.online.sampler` — see that module's own doc
comment for why LLM-judge scoring of a sampled event is explicitly out
of scope here (GatewayDecisionEvent carries no prompt/completion
content to judge).
"""

from __future__ import annotations

import random

from evals.contracts.gatewayevents.v1.gatewayevents_pb2 import GatewayDecisionEvent
from evals.online.sampler import (
    DEFAULT_RULE_FILTERS,
    RuleFilter,
    passes_rule_filters,
    should_sample,
)


def _event(outcome: int) -> GatewayDecisionEvent:
    return GatewayDecisionEvent(outcome=outcome)


def test_should_sample_rate_zero_never_samples():
    event = _event(GatewayDecisionEvent.Outcome.OUTCOME_OK)
    rng = random.Random(1)
    assert all(not should_sample(event, 0.0, rng) for _ in range(50))


def test_should_sample_rate_one_always_samples():
    event = _event(GatewayDecisionEvent.Outcome.OUTCOME_OK)
    rng = random.Random(1)
    assert all(should_sample(event, 1.0, rng) for _ in range(50))


def test_should_sample_rate_point_one_converges_to_roughly_ten_percent():
    """The real, load-bearing statistical proof: at sample_rate=0.1
    against a large, seeded, reproducible run, roughly 10% of events
    should be selected -- a generous +/-5 percentage point band (5%-15%)
    for sampling noise at this sample size, matching this codebase's own
    established tolerance convention for statistical tests (see
    test_stats.py's own bootstrap/Beta-Binomial tests).
    """
    event = _event(GatewayDecisionEvent.Outcome.OUTCOME_OK)
    rng = random.Random(42)
    samples = 10_000
    sampled_count = sum(should_sample(event, 0.1, rng) for _ in range(samples))
    rate = sampled_count / samples
    assert 0.05 <= rate <= 0.15, (
        f"observed sample rate = {rate:.3f} over {samples} draws, "
        "want within [0.05, 0.15] for a configured sample_rate=0.1"
    )


def test_should_sample_is_deterministic_given_the_same_seeded_rng_state():
    """Two independently-seeded random.Random(SAME_SEED) instances must
    produce IDENTICAL sequences of should_sample decisions -- the real
    property --sample-seed depends on for a reproducible dry run.
    """
    event = _event(GatewayDecisionEvent.Outcome.OUTCOME_OK)
    rng_a = random.Random(7)
    rng_b = random.Random(7)
    decisions_a = [should_sample(event, 0.3, rng_a) for _ in range(200)]
    decisions_b = [should_sample(event, 0.3, rng_b) for _ in range(200)]
    assert decisions_a == decisions_b


def test_passes_rule_filters_with_empty_filters_always_passes():
    event = _event(GatewayDecisionEvent.Outcome.OUTCOME_OK)
    assert passes_rule_filters(event, ()) is True


def test_passes_rule_filters_default_rules_match_a_failure_outcome():
    event = _event(GatewayDecisionEvent.Outcome.OUTCOME_UPSTREAM_ERROR)
    assert passes_rule_filters(event, DEFAULT_RULE_FILTERS) is True


def test_passes_rule_filters_default_rules_reject_a_success_outcome():
    event = _event(GatewayDecisionEvent.Outcome.OUTCOME_OK)
    assert passes_rule_filters(event, DEFAULT_RULE_FILTERS) is False


def test_passes_rule_filters_matches_if_any_single_filter_matches():
    always_false = RuleFilter(name="never", matches=lambda e: False)
    always_true = RuleFilter(name="always", matches=lambda e: True)
    event = _event(GatewayDecisionEvent.Outcome.OUTCOME_OK)
    assert passes_rule_filters(event, (always_false, always_true)) is True
    assert passes_rule_filters(event, (always_false,)) is False


def test_rule_filtered_out_event_never_passes_regardless_of_sample_rate():
    """The real gap this test guards: a rule-filtered-out event must
    never be treated as sampled-in just because sample_rate is high --
    the two checks are independent AND-combined, never one substituting
    for the other. Mirrors this exact composition as
    `evals cli ingest_cmd` applies it: should_sample(...) and
    passes_rule_filters(...) both required.
    """
    event = _event(
        GatewayDecisionEvent.Outcome.OUTCOME_OK
    )  # never matches DEFAULT_RULE_FILTERS
    rng = random.Random(3)
    for _ in range(50):
        sampled_in = should_sample(event, 1.0, rng) and passes_rule_filters(
            event, DEFAULT_RULE_FILTERS
        )
        assert not sampled_in
