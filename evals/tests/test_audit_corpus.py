import asyncio

import pytest

from evals.audit_corpus import (
    AuditFinding,
    audit_case,
    build_audit_prompt,
    parse_audit_response,
)
from evals.models import EvalCase


def _make_fake_call_model(response: str):
    async def call_model(prompt: str) -> str:
        return response

    return call_model


def _make_case(**overrides) -> EvalCase:
    defaults = dict(
        id="audit-test-case",
        revision=1,
        task_spec={"output": "hello", "match": "exact"},
        reference="hello",
        tier="regression",
        tags=["category:test"],
    )
    defaults.update(overrides)
    return EvalCase(**defaults)


def test_build_audit_prompt_includes_case_id_revision_and_task_spec():
    case = _make_case()
    prompt = build_audit_prompt(case)

    assert "audit-test-case" in prompt
    assert "revision 1" in prompt
    assert "regression" in prompt
    assert "category:test" in prompt
    assert "'output': 'hello'" in prompt or '"output": "hello"' in prompt
    assert "hello" in prompt


def test_build_audit_prompt_never_scores_output_against_reference():
    # A real, distinguishing assertion: this is an AUDIT prompt, not a
    # judge prompt -- it must never ask for a PASS/FAIL verdict comparing
    # output to reference the way build_judge_prompt does.
    case = _make_case()
    prompt = build_audit_prompt(case)

    assert "VERDICT" not in prompt
    assert "SEVERITY" in prompt


def test_build_audit_prompt_handles_no_reference():
    case = _make_case(reference=None)
    prompt = build_audit_prompt(case)

    assert "(none)" in prompt


def test_parse_audit_response_no_defect():
    raw = (
        "REASONING: The task is clear and the reference is correct.\n"
        "SEVERITY: no_defect\n"
    )
    case = _make_case()
    finding = parse_audit_response(case, raw)

    assert finding.eval_case_id == "audit-test-case"
    assert finding.eval_case_revision == 1
    assert finding.severity == "no_defect"
    assert "clear" in finding.reason
    assert finding.cost_usd is None


def test_parse_audit_response_minor():
    raw = "REASONING: A minor wording ambiguity exists.\nSEVERITY: minor\n"
    finding = parse_audit_response(_make_case(), raw)
    assert finding.severity == "minor"


def test_parse_audit_response_major():
    raw = "REASONING: The reference answer is factually wrong.\nSEVERITY: major\n"
    finding = parse_audit_response(_make_case(), raw)
    assert finding.severity == "major"


def test_parse_audit_response_is_case_insensitive():
    raw = "reasoning: fine.\nseverity: MAJOR\n"
    finding = parse_audit_response(_make_case(), raw)
    assert finding.severity == "major"


def test_parse_audit_response_raises_on_missing_severity_line():
    raw = "REASONING: I looked at this case carefully.\n"
    with pytest.raises(ValueError, match="SEVERITY"):
        parse_audit_response(_make_case(), raw)


def test_audit_case_end_to_end_with_scripted_fake():
    case = _make_case(id="audit-e2e-case")
    fake = _make_fake_call_model("REASONING: Looks fine.\nSEVERITY: no_defect\n")

    finding = asyncio.run(audit_case(case, fake))

    assert isinstance(finding, AuditFinding)
    assert finding.eval_case_id == "audit-e2e-case"
    assert finding.severity == "no_defect"
    assert finding.cost_usd is None
