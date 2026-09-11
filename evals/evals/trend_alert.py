"""Threshold+window alerting over persisted `TrendSnapshot` history (see
`--record-trend` on `report`/`audit-corpus`, and `evals trend show`).

Per `docs/upgrade-research/evals-continuous-monitoring-2026-09-11.md`
Finding 3: every 2026 platform surveyed (Braintrust, Langfuse, Arize AX,
Weave) alerts via a simple tunable threshold (static, or an "automatic"
historical-baseline-derived one) over a rolling window -- never a
statistical/DDM-style drift detector. This module implements only the
static-threshold half: an operator-supplied numeric bound, compared
against the mean of the most recent `window` snapshots for a series. A
baseline-derived ("automatic") threshold is a real, separately-scoped
future extension, not built here -- Kelvran has no real production
traffic yet to calibrate a baseline against (the same reason
`docs/operations/TELEMETRY.md`'s own SLI/SLO section gives for shipping
zero concrete target numbers today), and fabricating one would be worse
than not having it.

Deliberately a flat, third-layer module (per `.importlinter`'s `layers`
contract), sibling to `evals.audit_corpus`/`evals.auto_flag`/
`evals.field_swap_lint`/`evals.corpus_staleness` on that same
`|`-joined layer bullet -- imports only `evals.models`, mirroring
`evals.auto_flag`'s own documented sibling-independence reasoning.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Literal

from evals.models import TrendSnapshot

AlertDirection = Literal["below", "above"]


@dataclass(frozen=True)
class TrendAlertRule:
    """One operator-supplied threshold for one `series`.

    `window` is the number of most-recent snapshots (by `recorded_at`,
    within that series/scorer_type group) averaged over -- never the
    single latest value alone, so one noisy data point can't trip an
    alert by itself. `severity` is a free-form label (e.g. "warning"/
    "critical") the caller chooses; this module never interprets it,
    only carries it through to the resulting `TrendAlert`.
    """

    series: str
    direction: AlertDirection
    threshold: float
    window: int = 5
    severity: str = "warning"


@dataclass(frozen=True)
class TrendAlert:
    """One triggered rule, scoped to the (series, scorer_type) group it
    fired for. `scorer_type` is `None` for series that aren't scoped by
    scorer (e.g. `audit_corpus_defect_rate`), mirroring
    `TrendSnapshot.scorer_type`'s own convention.
    """

    series: str
    scorer_type: str | None
    severity: str
    direction: AlertDirection
    threshold: float
    window_mean: float
    window_n: int


def _series_value(snap: TrendSnapshot) -> float | None:
    """The one active numeric value for `snap`, per `TrendSnapshot`'s own
    "exactly one of rate_value/cost_usd_value is active for this series"
    docstring -- `cost_usd_value` is a `Decimal`, cast to `float` only
    here, at the boundary, for averaging (this module has no cost-
    accounting precision requirement of its own, unlike `Score.cost_usd`).
    """
    if snap.series == "cost_usd":
        return float(snap.cost_usd_value) if snap.cost_usd_value is not None else None
    return snap.rate_value


def check_trend_alerts(
    snapshots: list[TrendSnapshot],
    rules: list[TrendAlertRule],
) -> list[TrendAlert]:
    """Evaluate every rule against `snapshots`, grouped by
    (rule.series, scorer_type) -- never blended across scorer_type, per
    `TrendSnapshot`'s own "never blended across deterministic and
    llm_judge" principle. A group with zero non-None values in its
    window (e.g. every recent `judge_accuracy_kappa` snapshot recorded a
    genuinely undefined kappa) produces no alert for that group, never a
    fabricated 0.0. Rules are independent -- a series absent from
    `snapshots` entirely simply never triggers, not an error.
    """
    alerts: list[TrendAlert] = []
    for rule in rules:
        by_scorer: dict[str | None, list[TrendSnapshot]] = {}
        for snap in snapshots:
            if snap.series != rule.series:
                continue
            by_scorer.setdefault(snap.scorer_type, []).append(snap)

        for scorer_type, group in by_scorer.items():
            group_sorted = sorted(group, key=lambda s: s.recorded_at, reverse=True)
            values = [
                v for v in (_series_value(s) for s in group_sorted) if v is not None
            ]
            window_values = values[: rule.window]
            if not window_values:
                continue
            window_mean = sum(window_values) / len(window_values)
            triggered = (
                window_mean < rule.threshold
                if rule.direction == "below"
                else window_mean > rule.threshold
            )
            if triggered:
                alerts.append(
                    TrendAlert(
                        series=rule.series,
                        scorer_type=scorer_type,
                        severity=rule.severity,
                        direction=rule.direction,
                        threshold=rule.threshold,
                        window_mean=window_mean,
                        window_n=len(window_values),
                    )
                )
    return alerts
