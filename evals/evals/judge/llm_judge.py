"""LLM-as-judge scorer.

Per evals/ARCHITECTURE.md's Rollout Lifecycle footnote and THREAT_MODEL.md's
Evals "Tampering" row (judge manipulation via adversarial prompts), the v1
LLM-judge ships with two bias mitigations by default:

  - CoT-forcing: the judge must produce its reasoning before its verdict,
    not the reverse — this is enforced by the prompt template's requested
    output order, not just requested informally.
  - Reference-guided grading: the judge is always given the reference
    answer, never asked to grade from first principles alone.

`judge()` takes the model-calling function as a dependency-injected
parameter (`call_model`) specifically so it is testable without a live
provider API key: production code passes a real provider SDK call, tests
pass a scripted fake. This module makes zero network calls itself.

`judge()`'s optional `axis` parameter (added 2026-09-05, per
docs/rfcs/2026-09-05-evals-multi-axis-judging.md) scopes one call's
verdict to a single named rubric dimension instead of one holistic
judgment — `evals.cli` calls `judge()` once per configured axis (never
one call trying to cover several axes at once), so this is the first
change to this module's own scoring logic since it shipped, not a
provider/caching concern leaking in from outside.
"""

from __future__ import annotations

import re
from collections.abc import Awaitable, Callable

from pydantic import BaseModel

_BIAS_MITIGATIONS_APPLIED = ["cot_forcing", "reference_guided_grading"]

_JUDGE_PROMPT_TEMPLATE = """\
You are an impartial grader comparing a candidate output against a reference answer.

Reference answer:
{reference}

Candidate output:
{output}

Think step by step about whether the candidate output is correct relative to \
the reference answer. Consider partial correctness, phrasing differences that \
don't change meaning, and any factual discrepancies. Write out your reasoning \
BEFORE giving your final verdict — do not state the verdict first.

Respond in exactly this format, with no other text:
REASONING: <your step-by-step reasoning>
VERDICT: <PASS or FAIL>
"""

# Same structure as _JUDGE_PROMPT_TEMPLATE, with one added sentence scoping
# the verdict to a single named rubric axis (e.g. "correctness", "safety")
# instead of a holistic judgment — per docs/rfcs/2026-09-05-evals-multi-
# axis-judging.md's one-call-per-axis design: each axis gets its own
# focused prompt and its own PASS/FAIL, never one call trying to cover
# every axis at once.
_JUDGE_PROMPT_TEMPLATE_WITH_AXIS = """\
You are an impartial grader comparing a candidate output against a reference answer.

Reference answer:
{reference}

Candidate output:
{output}

Grade specifically on this dimension: {axis}. Consider only this dimension when \
forming your verdict — a candidate output may be correct on other dimensions and \
still fail this one, or vice versa; do not let other dimensions influence this \
verdict.

Think step by step about whether the candidate output passes on this dimension \
relative to the reference answer. Write out your reasoning BEFORE giving your \
final verdict — do not state the verdict first.

Respond in exactly this format, with no other text:
REASONING: <your step-by-step reasoning, scoped to {axis} only>
VERDICT: <PASS or FAIL>
"""

_VERDICT_PATTERN = re.compile(r"VERDICT:\s*(PASS|FAIL)", re.IGNORECASE)
_REASONING_PATTERN = re.compile(
    r"REASONING:\s*(.*?)\s*VERDICT:", re.IGNORECASE | re.DOTALL
)


class JudgeResult(BaseModel):
    """Result of a single LLM-judge call.

    Mirrors the relevant subset of evals/ARCHITECTURE.md's `Score` data
    model: `rationale` is the judge's CoT text, `bias_mitigations_applied`
    records which defenses (per THREAT_MODEL.md) were in effect for this
    call.
    """

    passed: bool
    rationale: str
    bias_mitigations_applied: list[str]


def build_judge_prompt(output: str, reference: str, axis: str | None = None) -> str:
    """Build the CoT-forcing, reference-guided judge prompt.

    `axis`, when given, scopes the verdict to a single named rubric
    dimension (e.g. "correctness", "safety") instead of one holistic
    judgment — see `_JUDGE_PROMPT_TEMPLATE_WITH_AXIS`. `None` (the
    default) reproduces the exact original holistic prompt, unchanged.
    """
    if axis is None:
        return _JUDGE_PROMPT_TEMPLATE.format(reference=reference, output=output)
    return _JUDGE_PROMPT_TEMPLATE_WITH_AXIS.format(
        reference=reference, output=output, axis=axis
    )


def _parse_judge_response(raw_response: str) -> tuple[bool, str]:
    verdict_match = _VERDICT_PATTERN.search(raw_response)
    if verdict_match is None:
        raise ValueError(
            f"judge response missing a VERDICT: PASS|FAIL line: {raw_response!r}"
        )
    passed = verdict_match.group(1).upper() == "PASS"

    reasoning_match = _REASONING_PATTERN.search(raw_response)
    rationale = reasoning_match.group(1).strip() if reasoning_match else ""

    return passed, rationale


async def judge(
    output: str,
    reference: str,
    call_model: Callable[[str], Awaitable[str]],
    axis: str | None = None,
) -> JudgeResult:
    """Score `output` against `reference` using an LLM judge.

    `call_model` is an async dependency-injected callable that takes the
    fully-built judge prompt and returns the judge model's raw text
    response. Production code wires this to a real provider SDK call;
    tests wire it to a scripted fake, so this function is fully unit
    testable with zero network calls and zero API keys.

    `axis`, when given, scopes this one call's verdict to a single named
    rubric dimension — see `build_judge_prompt`. `None` (the default)
    reproduces the exact original holistic-verdict behavior; the returned
    `JudgeResult` itself carries no `axis` field, since the caller already
    knows which axis it asked for and is responsible for recording it
    (e.g. as `Score.rubric_axis`) — not duplicated here.
    """
    prompt = build_judge_prompt(output=output, reference=reference, axis=axis)
    raw_response = await call_model(prompt)
    passed, rationale = _parse_judge_response(raw_response)

    return JudgeResult(
        passed=passed,
        rationale=rationale,
        bias_mitigations_applied=list(_BIAS_MITIGATIONS_APPLIED),
    )
