from datetime import UTC, datetime
from decimal import Decimal

import pytest

from evals.models import TrendSnapshot
from evals.trend_alert import TrendAlertRule, check_trend_alerts


def _snap(series, when, rate_value=None, cost_usd_value=None, scorer_type=None):
    return TrendSnapshot(
        series=series,
        recorded_at=datetime(2026, 9, *when, tzinfo=UTC),
        n=1,
        rate_value=rate_value,
        cost_usd_value=cost_usd_value,
        scorer_type=scorer_type,
        source_command="report",
    )


def test_window_mean_below_threshold_triggers_alert():
    snapshots = [
        _snap("judge_accuracy_kappa", (1, 1), rate_value=0.30),
        _snap("judge_accuracy_kappa", (1, 2), rate_value=0.35),
        _snap("judge_accuracy_kappa", (1, 3), rate_value=0.32),
    ]
    rule = TrendAlertRule(
        series="judge_accuracy_kappa", direction="below", threshold=0.4
    )

    alerts = check_trend_alerts(snapshots, [rule])

    assert len(alerts) == 1
    assert alerts[0].series == "judge_accuracy_kappa"
    assert alerts[0].window_n == 3
    assert alerts[0].window_mean < 0.4


def test_window_mean_on_the_safe_side_of_threshold_does_not_trigger():
    snapshots = [
        _snap("judge_accuracy_kappa", (1, d), rate_value=0.9) for d in (1, 2, 3)
    ]
    rule = TrendAlertRule(
        series="judge_accuracy_kappa", direction="below", threshold=0.4
    )

    assert check_trend_alerts(snapshots, [rule]) == []


def test_window_only_considers_the_most_recent_n_snapshots():
    # Five old, bad values outside a window of 2, then two recent, good
    # ones -- the alert must be evaluated only against the recent pair.
    old_bad = [
        _snap("cost_usd", (1, d), cost_usd_value=Decimal("999")) for d in range(1, 6)
    ]
    recent_good = [
        _snap("cost_usd", (2, 1), cost_usd_value=Decimal("1.00")),
        _snap("cost_usd", (2, 2), cost_usd_value=Decimal("1.50")),
    ]
    rule = TrendAlertRule(
        series="cost_usd", direction="above", threshold=50.0, window=2
    )

    alerts = check_trend_alerts(old_bad + recent_good, [rule])

    assert alerts == []


def test_none_rate_values_are_skipped_not_treated_as_zero():
    snapshots = [
        _snap("judge_accuracy_kappa", (1, 1), rate_value=None),
        _snap("judge_accuracy_kappa", (1, 2), rate_value=None),
    ]
    rule = TrendAlertRule(
        series="judge_accuracy_kappa", direction="below", threshold=0.4
    )

    # Every value in the window is None (a genuinely undefined kappa,
    # per TrendSnapshot's own docstring) -- must produce no alert, never
    # a fabricated window_mean=0.0 that would spuriously trigger "below".
    assert check_trend_alerts(snapshots, [rule]) == []


def test_series_with_no_matching_snapshots_produces_no_alert_not_a_crash():
    snapshots = [_snap("cost_usd", (1, 1), cost_usd_value=Decimal("1.0"))]
    rule = TrendAlertRule(
        series="judge_accuracy_kappa", direction="below", threshold=0.4
    )

    assert check_trend_alerts(snapshots, [rule]) == []


def test_different_scorer_types_are_evaluated_independently():
    snapshots = [
        _snap("quote_grounding_rate", (1, 1), rate_value=0.2, scorer_type="llm_judge"),
        _snap(
            "quote_grounding_rate",
            (1, 1),
            rate_value=0.95,
            scorer_type="llm_judge_panel",
        ),
    ]
    rule = TrendAlertRule(
        series="quote_grounding_rate", direction="below", threshold=0.5
    )

    alerts = check_trend_alerts(snapshots, [rule])

    assert len(alerts) == 1
    assert alerts[0].scorer_type == "llm_judge"


def test_cost_usd_series_reads_the_decimal_value_as_a_float():
    snapshots = [_snap("cost_usd", (1, 1), cost_usd_value=Decimal("75.50"))]
    rule = TrendAlertRule(series="cost_usd", direction="above", threshold=50.0)

    alerts = check_trend_alerts(snapshots, [rule])

    assert len(alerts) == 1
    assert alerts[0].window_mean == 75.50


def test_severity_and_threshold_are_carried_through_from_the_rule():
    snapshots = [_snap("judge_accuracy_kappa", (1, 1), rate_value=0.1)]
    rule = TrendAlertRule(
        series="judge_accuracy_kappa",
        direction="below",
        threshold=0.4,
        severity="critical",
    )

    alerts = check_trend_alerts(snapshots, [rule])

    assert len(alerts) == 1
    assert alerts[0].severity == "critical"
    assert alerts[0].threshold == 0.4
    assert alerts[0].direction == "below"


def test_zero_window_is_rejected():
    with pytest.raises(ValueError, match="window must be >= 1"):
        TrendAlertRule(
            series="judge_accuracy_kappa", direction="below", threshold=0.4, window=0
        )


def test_negative_window_is_rejected():
    # Python's own negative-slice semantics would otherwise reinterpret a
    # negative window as "drop the |window| most-recent values" -- the
    # exact opposite of "the number of most-recent snapshots averaged
    # over" -- silently diluting a real regression signal instead of
    # raising a usage error.
    with pytest.raises(ValueError, match="window must be >= 1"):
        TrendAlertRule(
            series="judge_accuracy_kappa", direction="below", threshold=0.4, window=-1
        )
