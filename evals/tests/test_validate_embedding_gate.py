"""Unit tests for scripts/validate_embedding_gate.py's pure logic (entity
fingerprint port, cosine similarity, prompt/response parsing, precision/
recall scoring) -- none of these require a live AWS call. A separate,
`RUN_LIVE_LLM_TESTS=1`-gated integration test proves the real end-to-end
path (LLM paraphrase/negative generation + real Bedrock embeddings)
actually works, mirroring test_llm_judge_integration.py's exact shape.

scripts/ is deliberately outside the `evals` package (see that module's
own docstring for why) and is therefore not importable as `evals.scripts.*`
-- loaded here by file path instead.
"""

from __future__ import annotations

import importlib.util
import os
import sys
from pathlib import Path

import pytest

_MODULE_PATH = Path(__file__).parent.parent / "scripts" / "validate_embedding_gate.py"
_spec = importlib.util.spec_from_file_location("validate_embedding_gate", _MODULE_PATH)
assert _spec is not None and _spec.loader is not None
veg = importlib.util.module_from_spec(_spec)
sys.modules["validate_embedding_gate"] = veg
_spec.loader.exec_module(veg)


def test_entity_fingerprint_extracts_numbers_dates_and_capitalized_sequences():
    fp = veg.entity_fingerprint(
        ["What is the rate for Golden Gate Bridge on 2026-09-11 at 92.50%?"]
    )
    assert "92.50%" in fp
    assert "2026-09-11" in fp
    assert "Golden Gate Bridge" in fp


def test_entity_fingerprint_excludes_each_turns_own_first_word():
    # "What" is sentence-initial (capitalized only by grammar position,
    # not because it's an entity) -- must be excluded, matching the Go
    # original's own documented rationale.
    fp = veg.entity_fingerprint(["What is the capital of France?"])
    assert "What" not in fp
    assert "France" in fp


def test_entity_fingerprint_differs_between_drug_x_and_drug_y():
    fp_x = veg.entity_fingerprint(
        ["The sponsor circulated guidance for Drug X ahead of the visit."]
    )
    fp_y = veg.entity_fingerprint(
        ["The sponsor circulated guidance for Drug Y ahead of the visit."]
    )
    assert fp_x != fp_y


def test_entity_fingerprint_same_for_true_paraphrase_with_no_entity_change():
    fp_a = veg.entity_fingerprint(["What is the capital of France?"])
    fp_b = veg.entity_fingerprint(["Which city is the capital of France?"])
    assert fp_a == fp_b


def test_cosine_similarity_identical_vectors_is_one():
    assert veg.cosine_similarity([1.0, 2.0, 3.0], [1.0, 2.0, 3.0]) == pytest.approx(1.0)


def test_cosine_similarity_orthogonal_vectors_is_zero():
    assert veg.cosine_similarity([1.0, 0.0], [0.0, 1.0]) == pytest.approx(0.0)


def test_cosine_similarity_zero_vector_returns_zero_not_a_divide_by_zero_error():
    assert veg.cosine_similarity([0.0, 0.0], [1.0, 2.0]) == 0.0


def test_parse_paraphrase_negative_response_extracts_both_labeled_lines():
    raw = (
        "PARAPHRASE: Which city is the capital of France?\n"
        "NEGATIVE: What is the capital of Germany?"
    )
    parsed = veg.parse_paraphrase_negative_response(raw)
    assert parsed.paraphrase == "Which city is the capital of France?"
    assert parsed.negative == "What is the capital of Germany?"


def test_parse_paraphrase_negative_response_missing_label_raises():
    with pytest.raises(ValueError, match="PARAPHRASE"):
        veg.parse_paraphrase_negative_response("just some text with no labels at all")


def test_build_paraphrase_negative_prompt_embeds_the_original_query():
    prompt = veg.build_paraphrase_negative_prompt("What is the capital of France?")
    assert "What is the capital of France?" in prompt
    assert "PARAPHRASE:" in prompt
    assert "NEGATIVE:" in prompt


def _pair(should_match: bool, sim: float) -> tuple[veg.EvalPair, dict]:
    pair = veg.EvalPair(
        scenario="test",
        kind="test",
        text_a=["a"],
        text_b=["b"],
        should_match=should_match,
    )
    # Fabricate embeddings whose cosine similarity is exactly `sim`,
    # using orthonormal basis vectors -- avoids any floating-point
    # surprise from a more "realistic"-looking embedding.
    return pair, {"a": [1.0, 0.0], "b": [sim, (1 - sim**2) ** 0.5]}


def test_score_at_threshold_bare_counts_a_high_similarity_negative_as_false_positive():
    # This is exactly Finding 2's own critical failure mode: a
    # meaning-reversing pair with high cosine similarity, bare
    # thresholding alone approves it.
    pair, embeddings = _pair(should_match=False, sim=0.96)
    result = veg.score_at_threshold(
        [pair], embeddings, threshold=0.90, use_entity_gate=False
    )
    assert result.false_positives == 1
    assert result.true_positives == 0


def test_score_at_threshold_entity_gate_recovers_the_same_case_when_entities_differ():
    pair, embeddings = _pair(should_match=False, sim=0.96)
    # Override text_a/text_b with genuinely different entities so the
    # ported entity gate actually has something to disagree on.
    pair = veg.EvalPair(
        scenario="test",
        kind="test",
        text_a=["withhold the study drug"],
        text_b=["administer the study drug"],
        should_match=False,
    )
    embeddings = {
        "withhold the study drug": [1.0, 0.0],
        "administer the study drug": [0.96, (1 - 0.96**2) ** 0.5],
    }
    result = veg.score_at_threshold(
        [pair], embeddings, threshold=0.90, use_entity_gate=True
    )
    # The entity gate here can't catch a verb swap (neither "withhold"
    # nor "administer" match the number/date/capitalized-sequence
    # fingerprint), so this specific case is a real, known, disclosed
    # gap -- proving the SCORING mechanism reports it correctly (still a
    # false positive) is the point of this test, not that the gate saves
    # it. See the referent-shift case below for a scenario the entity
    # gate genuinely does recover.
    assert result.false_positives == 1


def test_score_at_threshold_entity_gate_recovers_a_referent_shift_case():
    pair = veg.EvalPair(
        scenario="test",
        kind="test",
        text_a=["The sponsor circulated guidance for Drug X ahead of the visit."],
        text_b=["The sponsor circulated guidance for Drug Y ahead of the visit."],
        should_match=False,
    )
    embeddings = {
        pair.text_a[0]: [1.0, 0.0],
        pair.text_b[0]: [0.99, (1 - 0.99**2) ** 0.5],
    }
    bare = veg.score_at_threshold(
        [pair], embeddings, threshold=0.90, use_entity_gate=False
    )
    gated = veg.score_at_threshold(
        [pair], embeddings, threshold=0.90, use_entity_gate=True
    )
    assert bare.false_positives == 1  # bare threshold wrongly approves it
    assert gated.true_negatives == 1  # the entity gate catches Drug X != Drug Y
    assert gated.false_positives == 0


def test_score_at_threshold_true_paraphrase_below_threshold_is_a_false_negative():
    pair, embeddings = _pair(should_match=True, sim=0.70)
    result = veg.score_at_threshold(
        [pair], embeddings, threshold=0.90, use_entity_gate=False
    )
    assert result.false_negatives == 1
    assert result.true_positives == 0


def test_load_seed_scenarios_reads_the_real_corpus_fixture():
    scenarios = veg.load_seed_scenarios()
    assert len(scenarios) == 4
    ids = [s[0] for s in scenarios]
    assert "adversarial-cache-negation-polarity-flip-drug-administration" in ids
    for _scenario_id, cached_turns, new_turns in scenarios:
        assert cached_turns  # every seed has at least one real user turn
        assert new_turns


@pytest.mark.llm_integration
@pytest.mark.skipif(
    os.environ.get("RUN_LIVE_LLM_TESTS") != "1",
    reason="requires a live AWS Bedrock credential; set RUN_LIVE_LLM_TESTS=1 to run",
)
def test_build_evaluation_set_against_a_real_bedrock_call():
    import asyncio

    from evals.judge.providers import BEDROCK_SONNET_5_MODEL_ID, make_bedrock_call_model

    call_model = make_bedrock_call_model(BEDROCK_SONNET_5_MODEL_ID)
    pairs = asyncio.run(veg.build_evaluation_set(call_model))

    assert len(pairs) == 12  # 4 seeds x 3 pairs each
    kinds = {p.kind for p in pairs}
    assert kinds == {"corpus_negative", "llm_paraphrase", "llm_negative"}
    for pair in pairs:
        if pair.kind == "llm_paraphrase":
            assert pair.should_match is True
        else:
            assert pair.should_match is False


@pytest.mark.llm_integration
@pytest.mark.skipif(
    os.environ.get("RUN_LIVE_LLM_TESTS") != "1",
    reason="requires a live AWS Bedrock credential; set RUN_LIVE_LLM_TESTS=1 to run",
)
def test_embed_text_sync_against_a_real_bedrock_titan_call():
    client = veg._resolve_bedrock_client()
    embedding = veg.embed_text_sync("What is the capital of France?", client)
    assert isinstance(embedding, list)
    assert len(embedding) == 1024
    assert all(isinstance(x, float) for x in embedding)
