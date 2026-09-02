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


def build_judge_prompt(output: str, reference: str) -> str:
    """Build the CoT-forcing, reference-guided judge prompt."""
    return _JUDGE_PROMPT_TEMPLATE.format(reference=reference, output=output)


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
) -> JudgeResult:
    """Score `output` against `reference` using an LLM judge.

    `call_model` is an async dependency-injected callable that takes the
    fully-built judge prompt and returns the judge model's raw text
    response. Production code wires this to a real provider SDK call;
    tests wire it to a scripted fake, so this function is fully unit
    testable with zero network calls and zero API keys.
    """
    prompt = build_judge_prompt(output=output, reference=reference)
    raw_response = await call_model(prompt)
    passed, rationale = _parse_judge_response(raw_response)

    return JudgeResult(
        passed=passed,
        rationale=rationale,
        bias_mitigations_applied=list(_BIAS_MITIGATIONS_APPLIED),
    )
