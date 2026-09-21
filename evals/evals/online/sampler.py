"""Sampling-rate and rule-filter decisions for a continuous stream of
`GatewayDecisionEvent`s — see this package's own doc comment for why
LLM-judge scoring of a sampled event is explicitly out of scope here.

Deliberately a flat, third-layer module (per `.importlinter`'s `layers`
contract), sibling to `evals.auto_flag` on that same `|`-joined layer
bullet: imports only `evals.contracts.gatewayevents.v1` (a generated
protobuf module, not one of this project's own layered packages) and
stdlib `random` — never `evals.models`/`evals.ingestion.*`, since this
module operates on the RAW event, one stage before any `EvalCase`/`Run`
conversion or object-storage listing/decoding exists at all. The CLI
(`evals ingest`) is what wires this together with
`evals.ingestion.object_store`/`decode`.
"""

from __future__ import annotations

import random
from collections.abc import Callable
from dataclasses import dataclass

from evals.contracts.gatewayevents.v1.gatewayevents_pb2 import GatewayDecisionEvent


def should_sample(
    event: GatewayDecisionEvent,
    sample_rate: float,
    rng: random.Random | None = None,
) -> bool:
    """Reports whether `event` is selected by a uniform-random sample at
    `sample_rate` — the same "configurable sampling rate for cost
    control" every real online-eval platform surveyed (Arize/LangSmith/
    Braintrust) ships. `event` itself is currently unused (the decision
    is content-independent, a pure coin flip) but is a real parameter,
    not decoration — a future rate that varies by `event.outcome` (e.g.
    "always sample failures, 1% sample successes") slots in here without
    changing this function's own call sites, mirroring
    `evals.auto_flag.FlagRule.matches`'s identical "the event/case is
    always available to the decision, even when today's rule doesn't
    need every field" convention.

    `sample_rate <= 0` always returns `False` (never sample) and
    `sample_rate >= 1` always returns `True` (always sample) — exact,
    deterministic boundaries, never left to `rng` to resolve by chance,
    so `sample_rate=1.0` (the default `evals ingest --sample-rate`
    ships, reproducing that command's pre-existing "ingest everything"
    behavior exactly) can never spuriously reject an event under any
    `rng` state.

    `rng` defaults to a fresh, unseeded `random.Random()` per call when
    `None` — real production sampling has no reproducibility
    requirement. Callers doing anything deterministic/testable (this
    module's own test suite, or an operator's `--sample-seed` dry run)
    must pass an explicit, seeded `random.Random()` instance instead.
    """
    if sample_rate <= 0:
        return False
    if sample_rate >= 1:
        return True
    active_rng = rng if rng is not None else random.Random()  # noqa: S311 -- online-eval sampling, never cryptographic material
    return active_rng.random() < sample_rate


@dataclass(frozen=True)
class RuleFilter:
    """One named rule matching a `GatewayDecisionEvent` on its real,
    already-populated fields — mirrors `evals.auto_flag.FlagRule`'s
    exact shape (name + a predicate), applied one stage earlier: against
    the raw event, before any `EvalCase`/`Run` conversion exists.
    """

    name: str
    matches: Callable[[GatewayDecisionEvent], bool]


DEFAULT_RULE_FILTERS: tuple[RuleFilter, ...] = (
    RuleFilter(
        name="gateway-failure-outcome",
        matches=lambda event: (
            event.outcome
            in (
                GatewayDecisionEvent.Outcome.OUTCOME_UPSTREAM_ERROR,
                GatewayDecisionEvent.Outcome.OUTCOME_RATE_LIMITED,
                GatewayDecisionEvent.Outcome.OUTCOME_GUARDRAIL_BLOCKED,
                GatewayDecisionEvent.Outcome.OUTCOME_DEPLOYMENT_CAPACITY,
            )
        ),
    ),
)
"""Mirrors `evals.auto_flag.DEFAULT_RULES`' own single outcome-based
rule and its exact 4-outcome set exactly — the two modules independently
converged on "failures are the interesting thing to flag" for their own
respective inputs (an ingested `EvalCase` there, a raw event here), so
this reuses that same, already-reasoned-about set rather than inventing
a second one. Kept as a SEPARATE tuple, not imported from `auto_flag`,
since the two operate on genuinely different input types
(`GatewayDecisionEvent` vs. `EvalCase.task_spec["outcome"]`'s string
form) — see `evals.auto_flag`'s own module doc comment for why
`Run.status` alone cannot distinguish these outcomes on already-ingested
data, the reason that module reads the string form at all; this module,
operating on the event before that lossy collapse, uses the real enum
values directly instead.
"""


def passes_rule_filters(
    event: GatewayDecisionEvent,
    filters: tuple[RuleFilter, ...] = DEFAULT_RULE_FILTERS,
) -> bool:
    """Reports whether `event` matches AT LEAST ONE filter in `filters`
    — mirrors `evals.auto_flag.flag_candidates`'s own "matched by at
    least one rule" semantics. An EMPTY `filters` tuple means no rule
    constraint at all (every event passes) — an explicit, non-magic way
    for a caller to opt out of rule filtering entirely, matching
    `should_sample`'s own "sample_rate=1.0 means always" convention:
    `evals ingest --filter-rules` (the opt-in flag) passes
    `DEFAULT_RULE_FILTERS`; omitting it passes `()`.
    """
    if not filters:
        return True
    return any(rule_filter.matches(event) for rule_filter in filters)
