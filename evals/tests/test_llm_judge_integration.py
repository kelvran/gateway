"""Integration test for evals.judge.providers.make_anthropic_call_model.

This requires a live Anthropic API key and makes a real, billed network
call, so it is skipped by default and only runs when RUN_LIVE_LLM_TESTS=1
is set — per docs/testing/TESTING.md's integration-layer guidance and this
task's explicit "never blocks the default pytest run" requirement, mirroring
tests/test_sandbox_integration.py's exact shape for the Docker equivalent.

Run explicitly with:
    RUN_LIVE_LLM_TESTS=1 uv run pytest tests/test_llm_judge_integration.py -v
"""

from __future__ import annotations

import asyncio
import os

import pytest

from evals.judge.llm_judge import judge
from evals.judge.providers import make_anthropic_call_model

pytestmark = [
    pytest.mark.llm_integration,
    pytest.mark.skipif(
        os.environ.get("RUN_LIVE_LLM_TESTS") != "1",
        reason="requires a live Anthropic API key; set RUN_LIVE_LLM_TESTS=1 to run",
    ),
]


def test_judge_against_a_real_anthropic_call_scores_a_trivially_correct_answer():
    call_model = make_anthropic_call_model()

    result = asyncio.run(
        judge(output="Paris", reference="Paris", call_model=call_model)
    )

    assert result.passed is True
    assert result.rationale != ""
