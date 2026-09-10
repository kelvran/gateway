"""Benchmark-corpus static audit tooling, per docs/rfcs/2026-09-11-evals-
audit-corpus.md: an LLM-audited opinion about a regression-corpus case's
OWN DESIGN — ambiguous task framing, wrong/unverifiable ground truth, or
an environment/tooling conflict inside its `task_spec` — never a judge
verdict scoring a candidate output against a reference. `audit_case`
deliberately never calls `evals.judge.llm_judge.judge()`: auditing the
design of a corpus with the same mechanism that will later judge cases
drawn from it would make the audit's own blind spots identical to the
thing being audited (see docs/upgrade-research/evals-next-upgrade-
round2-2026-09-09.md's Finding 3 for the motivating research).

Report-only, always: an LLM auditing the design of the corpus it will
later also judge is itself a fallible opinion, not a correctness oracle —
findings are for a human to review and act on, mirroring quote-
grounding's own "instrument first, act once there's real data"
precedent (docs/rfcs/2026-09-09-evals-quote-grounded-verdict.md). Never
wired to any --fail-under-style gate.

Static-mode only, per that RFC's own scope note: `evals run --suite`
(what every `regression_corpus_*.json` file uses) never executes a
sandbox or produces a `Span` — trajectory-mode auditing (using execution
traces) would need these cases run through real `rollout` execution
first, a materially larger, separate change, out of scope here.

Deliberately a flat, third-layer module (per .importlinter's `layers`
contract) rather than nested under `evals.judge` — it defines its own
local `AuditCallModel` type alias rather than importing
`evals.judge.llm_judge.CallModel`, the same isolation cost `judge()`
itself already pays to stay independent of `evals.judge.providers`
(a `layers`-contract sibling-independence rule, not just a style
choice: `evals.judge.llm_judge`, `evals.judge.providers`, and
`evals.judge.deterministic` are already mutually independent, and this
module sits on the identical `|`-joined layer bullet as them).
"""

from __future__ import annotations

import re
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from decimal import Decimal
from typing import Literal

from evals.models import EvalCase

AuditCallModel = Callable[[str], Awaitable[str]]

# "error" is not a design-defect opinion at all -- it means the audit
# CALL itself failed (a real, live-discovered case: Claude Sonnet 5
# exhausting its entire max_tokens budget on internal extended-thinking
# reasoning without ever emitting a visible text block, against a
# dense, technical audit prompt -- confirmed empirically 2026-09-15 via
# a direct Converse call reproducing the exact crash: `stopReason:
# "max_tokens"`, `content: [{"reasoningContent": ...}]`, zero text
# blocks; also covers any other real audit_case failure, e.g. a
# genuine content-safety refusal or a network error). A single case
# hitting this must become a scoreable, distinguishable AuditFinding,
# never an exception that aborts the entire multi-case batch -- see
# evals.cli.audit_corpus_cmd's own per-case try/except, which mirrors
# evals.rollout.scheduler.run_suite's identical "a single case's
# failure never aborts the suite" precedent.
AuditSeverity = Literal["no_defect", "minor", "major", "error"]

_AUDIT_PROMPT_TEMPLATE = """\
You are auditing one test case from an automated-evaluation regression \
corpus for design defects — NOT scoring a candidate output against a \
reference. Look specifically for three defect classes:

1. Ambiguous task design: the task_spec's intent, or what counts as a \
correct output, is genuinely unclear or open to multiple reasonable \
interpretations.
2. Wrong or unverifiable ground truth: the reference answer is \
factually incorrect, internally inconsistent, or cannot actually be \
verified from the information given.
3. Environment/tooling conflict: the task_spec assumes tooling, data, \
or an execution environment that contradicts itself or is described \
inconsistently.

Case id: {case_id} (revision {revision})
Tier: {tier}
Tags: {tags}

task_spec:
{task_spec}

Reference answer:
{reference}

Think step by step about whether any of the three defect classes above \
apply to this case. Write out your reasoning BEFORE giving your final \
severity — do not state the severity first.

Respond in exactly this format, with no other text:
REASONING: <your step-by-step reasoning>
SEVERITY: <no_defect, minor, or major>
"""

_SEVERITY_PATTERN = re.compile(r"SEVERITY:\s*(no_defect|minor|major)", re.IGNORECASE)
_REASONING_PATTERN = re.compile(
    r"REASONING:\s*(.*?)\s*SEVERITY:", re.IGNORECASE | re.DOTALL
)


@dataclass(frozen=True)
class AuditFinding:
    """One LLM-audited opinion about a single `EvalCase`'s own design.

    `cost_usd` is always `None` from `audit_case` itself — the CLI
    orchestration layer fills it in after the call, mirroring
    `evals.cli`'s own `_last_judge_call_cost_usd`/`_JudgeOutcome` split
    (a "read the real per-call cost off `call_model` after awaiting it"
    pattern this module deliberately reuses rather than reinventing).

    A `severity="error"` finding is never produced by `audit_case`/
    `parse_audit_response` itself (the regex-based parser only ever
    recognizes `no_defect`/`minor`/`major` in a real model response) --
    it is constructed directly by `evals.cli.audit_corpus_cmd`'s own
    per-case exception handler when the audit CALL itself fails, so
    `reason` on an `"error"` finding is the caught exception's own
    message, never LLM-generated text.
    """

    eval_case_id: str
    eval_case_revision: int
    severity: AuditSeverity
    reason: str
    cost_usd: Decimal | None = None


def build_audit_prompt(case: EvalCase) -> str:
    """Build the audit prompt for `case`. Pure, no I/O — mirrors
    `evals.judge.llm_judge.build_judge_prompt`'s own isolation.
    """
    return _AUDIT_PROMPT_TEMPLATE.format(
        case_id=case.id,
        revision=case.revision,
        tier=case.tier,
        tags=", ".join(case.tags) if case.tags else "(none)",
        task_spec=case.task_spec,
        reference=case.reference if case.reference is not None else "(none)",
    )


def parse_audit_response(case: EvalCase, raw_response: str) -> AuditFinding:
    """Parse `raw_response` into an `AuditFinding` for `case`. Mirrors
    `evals.judge.llm_judge.parse_judge_response`'s own error-handling
    convention exactly: a response missing the required `SEVERITY:` line
    is a real parse failure (`ValueError`), never silently defaulted to
    `"no_defect"` — an audit tool that can't parse its own judge's
    response must surface that as a tool-reliability problem, not a
    finding.
    """
    severity_match = _SEVERITY_PATTERN.search(raw_response)
    if severity_match is None:
        raise ValueError(
            "audit response missing a SEVERITY: no_defect|minor|major "
            f"line: {raw_response!r}"
        )
    severity = severity_match.group(1).lower()

    reasoning_match = _REASONING_PATTERN.search(raw_response)
    reason = reasoning_match.group(1).strip() if reasoning_match else ""

    return AuditFinding(
        eval_case_id=case.id,
        eval_case_revision=case.revision,
        severity=severity,  # type: ignore[arg-type]
        reason=reason,
    )


async def audit_case(case: EvalCase, call_model: AuditCallModel) -> AuditFinding:
    """Audit `case`'s own design via one real LLM call. Never calls
    `evals.judge.llm_judge.judge()` -- see this module's own docstring
    for why.
    """
    raw_response = await call_model(build_audit_prompt(case))
    return parse_audit_response(case, raw_response)
