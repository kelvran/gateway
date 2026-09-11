"""Detects a specific, structural regression-corpus bug class: two DISTINCT
`EvalCase`s in the same file whose `reference` values got transposed with
each other -- the exact shape of the real guardrail-13/18 bug found and
fixed by a 2026-09-15 triage swarm (`DECISIONS.md`), where case 13's
`reference` held case 18's correct value and vice versa, silently making
both regression-guard cases fail regardless of real system behavior.

Deliberately NOT a task_spec-similarity check. The plan that scoped this
module described the bug class as "cases whose task_spec is near-identical
except output/reference look swapped" -- checked directly against the real
guardrail-13/18 cases before implementing, and that description doesn't
hold: their `task_spec["input"]`/`attack_class` are unrelated attack
scenarios (a cross-tenant-exfil case and a false-positive-benign-text
case), not near-duplicates. What actually made them a matched pair was
being authored back-to-back with each other's correct value, not content
similarity. This module instead detects the SWAP ITSELF, purely by value:

    A.reference != A.output   (A's own reference looks wrong/divergent)
    B.reference != B.output   (same for B)
    A.reference == B.output   (A's reference is exactly B's real output)
    B.reference == A.output   (B's reference is exactly A's real output)

This is deliberately narrower than "any two cases whose reference diverges
from their own output" -- that alone describes the legitimate "GAP"
convention (see `ARCHITECTURE.md`'s Regression Corpus section), which is
common and intentional, not a bug. Requiring the FULL crosswise match on
top of that is what makes this detector precise: two independently-authored
GAP cases essentially never have their long, case-specific `reference`
prose collide with another case's `output`, so the false-positive risk at
Kelvran's own corpus scale (~137 cases) is negligible without needing a
task_spec-similarity heuristic at all.

Deliberately a flat, third-layer module (per `.importlinter`'s `layers`
contract), sibling to `evals.audit_corpus`/`evals.auto_flag` on that same
`|`-joined layer bullet -- imports only `evals.models`.
"""

from __future__ import annotations

from dataclasses import dataclass

from evals.models import EvalCase


@dataclass(frozen=True)
class SwapCandidate:
    """One detected pair of cases whose `reference` values are each
    exactly the other's real `output` -- the field-swap bug's signature.
    """

    case_a_id: str
    case_b_id: str


def find_reference_swaps(cases: list[EvalCase]) -> list[SwapCandidate]:
    """Return one `SwapCandidate` per unordered pair of cases in `cases`
    whose `reference` values are transposed with each other.

    Only considers cases whose `reference` diverges from their own
    `task_spec["output"]` -- a case with no divergence at all can never
    participate in a swap (there is nothing to have been swapped away
    from). Case order in `cases` doesn't matter; each unordered pair is
    checked once, never both (A, B) and (B, A).
    """
    divergent = [
        case
        for case in cases
        if case.reference is not None and case.reference != case.task_spec.get("output")
    ]

    candidates: list[SwapCandidate] = []
    for i, case_a in enumerate(divergent):
        output_a = case_a.task_spec.get("output")
        for case_b in divergent[i + 1 :]:
            output_b = case_b.task_spec.get("output")
            if case_a.reference == output_b and case_b.reference == output_a:
                candidates.append(
                    SwapCandidate(case_a_id=case_a.id, case_b_id=case_b.id)
                )
    return candidates
