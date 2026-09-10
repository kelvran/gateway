"""Rule-based auto-flag pass over ingested `drift_sample`-tier `EvalCase`s,
surfacing promotion CANDIDATES for the existing, still-fully-human-triggered
`evals promote` command. This module never constructs an `EvalCase`, never
appends to a suite file, and never itself promotes anything -- it only
reports candidates and leaves the actual `evals promote` invocation to a
human, per THREAT_MODEL.md's Evals Spoofing row (tier is set at dataset-
registration time by a human, never derived from a rollout's own outcome).

Reads `EvalCase.task_spec["outcome"]`, never `Run.status` --
`evals.ingestion.mapping.gateway_decision_event_to_eval_case_and_run`
collapses all nine non-OK `GatewayDecisionEvent.Outcome` values into the
single `Run.status="error"` string, so `Run.status` cannot distinguish
WHICH outcome fired. Only `task_spec["outcome"]` (the enum name, e.g.
`"OUTCOME_UPSTREAM_ERROR"`) carries that distinction on already-ingested
data -- this module exists specifically to read it instead of the lossier
`Run.status`.

v1 scope is outcome-based rules only. `GatewayDecisionEvent` fields decoded
during ingestion but currently discarded before persistence (`occurred_at`,
`rate_limit_fail_open`, `fallback_happened`, `fallback_from_deployment`,
`budget_spent_usd`) are not available on already-ingested `EvalCase`/`Run`
data today, so no rule here can use latency, cost, or fallback signals.

Deliberately a flat, third-layer module (per .importlinter's `layers`
contract), sibling to `evals.audit_corpus` on that same `|`-joined layer
bullet -- imports only `evals.models`, never `evals.results_store` or
`evals.audit_corpus`, mirroring `evals.audit_corpus`'s own documented
reasoning for staying independent of its layer-mates: this is a
sibling-independence rule enforced by the `layers` contract itself, not
just a style choice.
"""

from __future__ import annotations

from dataclasses import dataclass

from evals.models import EvalCase, Run


@dataclass(frozen=True)
class FlagRule:
    """One named rule matching an `EvalCase` on its ingested outcome."""

    name: str
    flagged_outcomes: frozenset[str]

    def matches(self, case: EvalCase) -> bool:
        return case.task_spec.get("outcome") in self.flagged_outcomes


DEFAULT_RULES: tuple[FlagRule, ...] = (
    FlagRule(
        name="gateway-failure-outcome",
        flagged_outcomes=frozenset(
            {
                "OUTCOME_UPSTREAM_ERROR",
                "OUTCOME_RATE_LIMITED",
                "OUTCOME_GUARDRAIL_BLOCKED",
                "OUTCOME_DEPLOYMENT_CAPACITY",
            }
        ),
    ),
)


@dataclass(frozen=True)
class FlagCandidate:
    """One `drift_sample` `EvalCase` matched by at least one `FlagRule`,
    naming every rule that matched -- never split into one `FlagCandidate`
    per matching rule.
    """

    eval_case_id: str
    eval_case_revision: int
    run_id: str
    matched_rules: tuple[str, ...]


def flag_candidates(
    cases: list[EvalCase],
    runs: list[Run],
    rules: tuple[FlagRule, ...] = DEFAULT_RULES,
) -> list[FlagCandidate]:
    """Return one `FlagCandidate` per `drift_sample` case matched by at
    least one rule in `rules`.

    Re-checks `case.tier == "drift_sample"` here even though callers (the
    `evals flag-candidates` CLI command) already pre-filter -- never trust
    the caller alone. A case with no corresponding `Run` (joined by
    `run.eval_case_id == case.id and run.eval_case_revision ==
    case.revision`) is skipped silently, never a crash. A case matching
    zero rules produces no `FlagCandidate` at all.
    """
    runs_by_case_key = {(r.eval_case_id, r.eval_case_revision): r for r in runs}

    candidates: list[FlagCandidate] = []
    for case in cases:
        if case.tier != "drift_sample":
            continue
        run = runs_by_case_key.get((case.id, case.revision))
        if run is None:
            continue
        matched_rules = tuple(rule.name for rule in rules if rule.matches(case))
        if not matched_rules:
            continue
        candidates.append(
            FlagCandidate(
                eval_case_id=case.id,
                eval_case_revision=case.revision,
                run_id=run.id,
                matched_rules=matched_rules,
            )
        )
    return candidates
