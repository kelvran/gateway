import asyncio

import pytest

from evals.judge.llm_judge import build_judge_prompt, judge


def _make_fake_call_model(response: str):
    async def call_model(prompt: str) -> str:
        return response

    return call_model


def test_judge_parses_pass_verdict_and_rationale():
    fake_response = (
        "REASONING: The candidate output states the capital correctly.\nVERDICT: PASS\n"
    )
    result = asyncio.run(
        judge(
            output="Paris",
            reference="Paris",
            call_model=_make_fake_call_model(fake_response),
        )
    )

    assert result.passed is True
    assert "capital correctly" in result.rationale
    assert result.bias_mitigations_applied == [
        "cot_forcing",
        "reference_guided_grading",
    ]


def test_judge_parses_fail_verdict():
    fake_response = (
        "REASONING: The candidate output names the wrong city entirely.\n"
        "VERDICT: FAIL\n"
    )
    result = asyncio.run(
        judge(
            output="London",
            reference="Paris",
            call_model=_make_fake_call_model(fake_response),
        )
    )

    assert result.passed is False
    assert "wrong city" in result.rationale


def test_judge_verdict_is_case_insensitive():
    fake_response = "REASONING: fine.\nVERDICT: pass\n"
    result = asyncio.run(
        judge(
            output="x", reference="x", call_model=_make_fake_call_model(fake_response)
        )
    )
    assert result.passed is True


def test_judge_raises_on_malformed_response_missing_verdict():
    fake_response = "REASONING: I have thoughts but no verdict.\n"
    with pytest.raises(ValueError):
        asyncio.run(
            judge(
                output="x",
                reference="y",
                call_model=_make_fake_call_model(fake_response),
            )
        )


def test_judge_passes_the_built_prompt_to_call_model():
    seen_prompts: list[str] = []

    async def capturing_call_model(prompt: str) -> str:
        seen_prompts.append(prompt)
        return "REASONING: ok.\nVERDICT: PASS\n"

    asyncio.run(
        judge(
            output="candidate-text",
            reference="reference-text",
            call_model=capturing_call_model,
        )
    )

    assert len(seen_prompts) == 1
    assert "candidate-text" in seen_prompts[0]
    assert "reference-text" in seen_prompts[0]


def test_build_judge_prompt_forces_reasoning_before_verdict():
    prompt = build_judge_prompt(output="out", reference="ref")
    reasoning_index = prompt.index("REASONING:")
    verdict_index = prompt.index("VERDICT:")
    assert reasoning_index < verdict_index
    assert "out" in prompt
    assert "ref" in prompt


def test_build_judge_prompt_with_no_axis_is_byte_identical_to_before():
    # The default (axis=None) path must reproduce the exact original
    # holistic prompt -- multi-axis judging is additive, never a change
    # in behavior for existing callers that never pass axis.
    with_default = build_judge_prompt(output="out", reference="ref")
    with_explicit_none = build_judge_prompt(output="out", reference="ref", axis=None)
    assert with_default == with_explicit_none
    assert "dimension" not in with_default


def test_build_judge_prompt_with_axis_names_the_axis_and_forces_reasoning_first():
    prompt = build_judge_prompt(output="out", reference="ref", axis="safety")
    reasoning_index = prompt.index("REASONING:")
    verdict_index = prompt.index("VERDICT:")
    assert reasoning_index < verdict_index
    assert "safety" in prompt
    assert "out" in prompt
    assert "ref" in prompt


def test_judge_with_axis_scopes_the_prompt_to_that_axis():
    seen_prompts: list[str] = []

    async def capturing_call_model(prompt: str) -> str:
        seen_prompts.append(prompt)
        return "REASONING: ok.\nVERDICT: PASS\n"

    asyncio.run(
        judge(
            output="candidate-text",
            reference="reference-text",
            call_model=capturing_call_model,
            axis="completeness",
        )
    )

    assert len(seen_prompts) == 1
    assert "completeness" in seen_prompts[0]


def test_judge_with_different_axes_can_disagree():
    # The same output/reference pair can genuinely pass on one axis and
    # fail on another -- proving the two calls are independently scoped,
    # not both silently reusing one cached holistic verdict.
    responses = iter(
        ["REASONING: fine.\nVERDICT: PASS\n", "REASONING: nope.\nVERDICT: FAIL\n"]
    )

    async def call_model(prompt: str) -> str:
        return next(responses)

    correctness_result = asyncio.run(
        judge(output="x", reference="x", call_model=call_model, axis="correctness")
    )
    safety_result = asyncio.run(
        judge(output="x", reference="x", call_model=call_model, axis="safety")
    )

    assert correctness_result.passed is True
    assert safety_result.passed is False
