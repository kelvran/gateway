"""Golden-fixture test for `evals.judge.llm_judge.build_judge_prompt`.

`tests/test_llm_judge.py` mocks `call_model` and only checks loose
substring properties of the prompt (e.g. "REASONING appears before
VERDICT", "the output/reference text appears somewhere"). None of its
existing assertions pin down the *exact* prompt text sent to the model —
so a future accidental edit to `_JUDGE_PROMPT_TEMPLATE` (e.g. losing the
"do not state the verdict first" CoT-forcing instruction, or reordering
the reference/candidate sections) would sail through that file's mocked
tests untouched, because the mock in that file doesn't care what prompt
it was given.

This test builds the prompt for a fixed example input and asserts it
matches a checked-in golden string byte-for-byte, so any drift in the
CoT-forcing / reference-guided-grading prompt template is caught
explicitly.
"""

from __future__ import annotations

from evals.judge.llm_judge import build_judge_prompt

_GOLDEN_PROMPT = """\
You are an impartial grader comparing a candidate output against a reference answer.

Reference answer:
Paris

Candidate output:
The capital of France is Paris.

Think step by step about whether the candidate output is correct relative to the reference answer. Consider partial correctness, phrasing differences that don't change meaning, and any factual discrepancies. Write out your reasoning BEFORE giving your final verdict — do not state the verdict first.

Respond in exactly this format, with no other text:
REASONING: <your step-by-step reasoning>
VERDICT: <PASS or FAIL>
"""


def test_build_judge_prompt_matches_golden_fixture_exactly():
    prompt = build_judge_prompt(
        output="The capital of France is Paris.",
        reference="Paris",
    )
    assert prompt == _GOLDEN_PROMPT
