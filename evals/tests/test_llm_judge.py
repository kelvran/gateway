import asyncio

import pytest

from evals.judge.llm_judge import build_judge_prompt, judge, reduce_panel_votes
from evals.models import PanelVote


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


def test_judge_a_real_verbatim_quote_is_grounded():
    fake_response = (
        "REASONING: The candidate output states the capital correctly.\n"
        "QUOTE: Paris\n"
        "VERDICT: PASS\n"
    )
    result = asyncio.run(
        judge(
            output="Paris",
            reference="Paris",
            call_model=_make_fake_call_model(fake_response),
        )
    )

    assert result.passed is True
    assert result.quote_grounded is True


def test_judge_a_fabricated_quote_is_not_grounded_but_does_not_affect_passed():
    # The QUOTE is real text, but it appears nowhere in the actual output
    # or reference -- a fabricated citation. quote_grounded must be
    # False, but this is measurement-only: passed still reflects the
    # judge's own VERDICT line, unaffected.
    fake_response = (
        "REASONING: The candidate output states the capital correctly.\n"
        "QUOTE: This sentence never appeared anywhere in the real inputs.\n"
        "VERDICT: PASS\n"
    )
    result = asyncio.run(
        judge(
            output="Paris",
            reference="Paris",
            call_model=_make_fake_call_model(fake_response),
        )
    )

    assert result.passed is True
    assert result.quote_grounded is False


def test_judge_missing_quote_line_is_not_grounded_and_still_parses_reasoning():
    # An older-format or QUOTE-instruction-ignoring response -- REASONING
    # must still parse correctly up to VERDICT, and quote_grounded must
    # be False (an empty quote is never grounded), never a parse error.
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
    assert result.quote_grounded is False


def test_judge_panel_populates_trigger_quote_and_quote_grounded_per_vote():
    grounded_response = "REASONING: matches.\nQUOTE: Paris\nVERDICT: PASS\n"
    fabricated_response = (
        "REASONING: matches.\nQUOTE: not in either text\nVERDICT: PASS\n"
    )
    result = asyncio.run(
        judge(
            output="Paris",
            reference="Paris",
            call_model=[
                _make_fake_call_model(grounded_response),
                _make_fake_call_model(fabricated_response),
            ],
        )
    )

    assert result.panel_votes is not None
    assert len(result.panel_votes) == 2
    assert result.panel_votes[0].trigger_quote == "Paris"
    assert result.panel_votes[0].quote_grounded is True
    assert result.panel_votes[1].quote_grounded is False


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


def test_judge_accepts_a_length_one_list_identically_to_a_bare_call_model():
    # Backward-compatibility proof for docs/rfcs/2026-09-07-evals-judge-
    # panel-interface.md's widened call_model type: a length-1 list must
    # score exactly like the pre-existing bare-callable shape every real
    # caller (evals.cli's _judge_with_cache) still uses unchanged.
    fake_response = "REASONING: matches.\nVERDICT: PASS\n"

    bare_result = asyncio.run(
        judge(
            output="Paris",
            reference="Paris",
            call_model=_make_fake_call_model(fake_response),
        )
    )
    list_result = asyncio.run(
        judge(
            output="Paris",
            reference="Paris",
            call_model=[_make_fake_call_model(fake_response)],
        )
    )

    assert bare_result == list_result


def test_judge_panel_of_two_passes_only_when_both_agree_pass():
    # Real behavior change from the interface RFC's own NotImplementedError
    # placeholder, per docs/rfcs/2026-09-08-evals-judge-panel-reducer.md --
    # a panel now actually scores, majority-reduced.
    pass_response = "REASONING: matches.\nVERDICT: PASS\n"
    panel = [_make_fake_call_model(pass_response), _make_fake_call_model(pass_response)]

    result = asyncio.run(judge(output="Paris", reference="Paris", call_model=panel))

    assert result.passed is True
    assert result.quorum_reached is True
    assert result.panel_votes is not None
    assert len(result.panel_votes) == 2
    assert all(v.passed for v in result.panel_votes)


def test_judge_panel_of_two_fails_when_both_agree_fail():
    fail_response = "REASONING: wrong.\nVERDICT: FAIL\n"
    panel = [_make_fake_call_model(fail_response), _make_fake_call_model(fail_response)]

    result = asyncio.run(judge(output="Paris", reference="London", call_model=panel))

    assert result.passed is False
    assert result.quorum_reached is True
    assert all(not v.passed for v in result.panel_votes)


def test_judge_panel_of_two_is_fail_closed_when_judges_disagree():
    panel = [
        _make_fake_call_model("REASONING: fine.\nVERDICT: PASS\n"),
        _make_fake_call_model("REASONING: not fine.\nVERDICT: FAIL\n"),
    ]

    result = asyncio.run(judge(output="Paris", reference="Paris", call_model=panel))

    assert result.passed is False
    assert result.quorum_reached is False


def test_judge_panel_returns_panel_votes_and_quorum_reached_on_the_judge_result():
    pass_response = "REASONING: matches.\nVERDICT: PASS\n"
    panel = [_make_fake_call_model(pass_response), _make_fake_call_model(pass_response)]

    result = asyncio.run(judge(output="Paris", reference="Paris", call_model=panel))

    assert result.panel_votes is not None
    assert result.quorum_reached is not None
    assert {"panelist_0", "panelist_1"} == {v.scorer_id for v in result.panel_votes}


def test_judge_single_call_model_still_has_none_panel_votes_and_quorum_reached():
    # Backward-compatibility proof: a bare (non-panel) call must be
    # byte-for-byte unaffected by this module now supporting panels.
    result = asyncio.run(
        judge(
            output="Paris",
            reference="Paris",
            call_model=_make_fake_call_model("REASONING: matches.\nVERDICT: PASS\n"),
        )
    )

    assert result.panel_votes is None
    assert result.quorum_reached is None


def test_judge_panel_uses_identical_prompt_and_independent_refutation():
    # The concrete independent-refutation proof: neither panelist's
    # captured prompt may contain the other's raw response text.
    captured_prompts: list[str] = []

    async def call_model_a(prompt: str) -> str:
        captured_prompts.append(prompt)
        return "REASONING: model A reasoning unique-marker-AAA.\nVERDICT: PASS\n"

    async def call_model_b(prompt: str) -> str:
        captured_prompts.append(prompt)
        return "REASONING: model B reasoning unique-marker-BBB.\nVERDICT: FAIL\n"

    asyncio.run(
        judge(
            output="Paris", reference="Paris", call_model=[call_model_a, call_model_b]
        )
    )

    assert len(captured_prompts) == 2
    assert captured_prompts[0] == captured_prompts[1]
    assert "unique-marker-AAA" not in captured_prompts[1]
    assert "unique-marker-BBB" not in captured_prompts[0]


def test_judge_rejects_an_empty_call_model_list():
    with pytest.raises(ValueError):
        asyncio.run(judge(output="Paris", reference="Paris", call_model=[]))


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


def _vote(scorer_id: str, passed: bool) -> PanelVote:
    return PanelVote(scorer_id=scorer_id, passed=passed, rationale="")


def test_reduce_panel_votes_unanimous_pass_reaches_quorum():
    verdict = reduce_panel_votes([_vote("a", True), _vote("b", True)])

    assert verdict.passed is True
    assert verdict.quorum_reached is True


def test_reduce_panel_votes_unanimous_fail_reaches_quorum():
    verdict = reduce_panel_votes([_vote("a", False), _vote("b", False)])

    assert verdict.passed is False
    assert verdict.quorum_reached is True


def test_reduce_panel_votes_two_judge_tie_is_fail_closed_no_quorum():
    # The load-bearing case: a 2-judge panel's every possible disagreement
    # IS a 1-1 tie -- this must always fail closed, never fail open.
    verdict = reduce_panel_votes([_vote("a", True), _vote("b", False)])

    assert verdict.passed is False
    assert verdict.quorum_reached is False


def test_reduce_panel_votes_three_judge_strict_majority_reaches_quorum():
    verdict = reduce_panel_votes(
        [_vote("a", True), _vote("b", True), _vote("c", False)]
    )

    assert verdict.passed is True
    assert verdict.quorum_reached is True


def test_reduce_panel_votes_four_judge_even_split_is_fail_closed_no_quorum():
    # A larger, even-sized panel can also tie -- fail-closed generalizes
    # beyond the guaranteed 2-judge case.
    verdict = reduce_panel_votes(
        [_vote("a", True), _vote("b", True), _vote("c", False), _vote("d", False)]
    )

    assert verdict.passed is False
    assert verdict.quorum_reached is False


def test_reduce_panel_votes_raises_on_empty_votes():
    with pytest.raises(ValueError):
        reduce_panel_votes([])


def test_reduce_panel_votes_bias_mitigations_include_panel_level_mitigations():
    verdict = reduce_panel_votes([_vote("a", True), _vote("b", True)])

    assert "cot_forcing" in verdict.bias_mitigations_applied
    assert "reference_guided_grading" in verdict.bias_mitigations_applied
    assert "independent_refutation" in verdict.bias_mitigations_applied
    assert "disjoint_model_family_panel" in verdict.bias_mitigations_applied
