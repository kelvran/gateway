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
one call trying to cover several axes at once).

`judge()`'s `call_model` parameter (widened 2026-09-07, per
docs/rfcs/2026-09-07-evals-judge-panel-interface.md) accepts either a
single `CallModel` or a `list[CallModel]`, so the multi-judge panel
(built 2026-09-08, per docs/rfcs/2026-09-08-evals-judge-panel-reducer.md)
is additive to this signature rather than a retrofit of it. A panel of
more than one `call_model` is scored via independent refutation — every
judge is given the IDENTICAL prompt, none sees another's response or
verdict — then majority-reduced via `reduce_panel_votes`. Every existing
single-judge caller (a bare `call_model`, or a length-1 list) is scored
identically to before this module ever supported a panel at all; see the
interface RFC's own Verification section for that backward-compatibility
proof.
"""

from __future__ import annotations

import asyncio
import re
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Literal

from pydantic import BaseModel

from evals.models import PanelVote

CallModel = Callable[[str], Awaitable[str]]

# Public (no leading underscore) so evals.cli's Phase 6 position-swapped
# debiasing helper can extend this list rather than hand-duplicating it,
# per docs/rfcs/2026-09-09-evals-judge-debiasing-position-swap.md.
BIAS_MITIGATIONS_APPLIED = ["cot_forcing", "reference_guided_grading"]

# Additive to BIAS_MITIGATIONS_APPLIED for a panel score specifically —
# every panelist's own call already applies the two mitigations above;
# these two describe properties of the PANEL itself, not any one call.
_PANEL_BIAS_MITIGATIONS_ADDED = [
    "independent_refutation",
    "disjoint_model_family_panel",
]

_JUDGE_PROMPT_TEMPLATE = """\
You are an impartial grader comparing a candidate output against a reference answer.

Reference answer:
{reference}

Candidate output:
{output}

Think step by step about whether the candidate output is correct relative to \
the reference answer. Consider partial correctness, phrasing differences that \
don't change meaning, and any factual discrepancies. Write out your reasoning \
BEFORE giving your final verdict — do not state the verdict first. Then quote \
the exact, verbatim span (from either the reference answer or the candidate \
output above) that most directly grounds your verdict — do not paraphrase or \
summarize it.

Respond in exactly this format, with no other text:
REASONING: <your step-by-step reasoning>
QUOTE: <a verbatim quote from the reference answer or candidate output above>
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
final verdict — do not state the verdict first. Then quote the exact, verbatim \
span (from either the reference answer or the candidate output above) that most \
directly grounds your verdict — do not paraphrase or summarize it.

Respond in exactly this format, with no other text:
REASONING: <your step-by-step reasoning, scoped to {axis} only>
QUOTE: <a verbatim quote from the reference answer or candidate output above>
VERDICT: <PASS or FAIL>
"""

_VERDICT_PATTERN = re.compile(r"VERDICT:\s*(PASS|FAIL)", re.IGNORECASE)
# Terminates at whichever of QUOTE:/VERDICT: comes first, deliberately —
# a raw_response with no QUOTE line at all (an older-format response, a
# judge that ignores the new instruction, or a cached response from
# before this feature existed) must still parse REASONING correctly up
# to VERDICT:, exactly as it always has. This is what keeps the QUOTE
# addition backward-compatible rather than a breaking prompt-contract
# change.
_REASONING_PATTERN = re.compile(
    r"REASONING:\s*(.*?)\s*(?:QUOTE:|VERDICT:)", re.IGNORECASE | re.DOTALL
)
# QUOTE sits between REASONING and VERDICT in both prompt templates — see
# their own "quote the exact, verbatim span... before giving your final
# verdict" instruction, mirroring the existing REASONING-before-VERDICT
# anti-post-hoc-rationalization ordering. Missing entirely (an older-
# format or malformed response) falls back to "" via parse_judge_response,
# the same lenient fallback _REASONING_PATTERN already uses — this is a
# measurement-only signal (see quote_is_grounded), never a hard parse
# error, per docs/rfcs/2026-09-09-evals-quote-grounded-verdict.md.
_QUOTE_PATTERN = re.compile(r"QUOTE:\s*(.*?)\s*VERDICT:", re.IGNORECASE | re.DOTALL)


class JudgeResult(BaseModel):
    """Result of a single `judge()` call — one judge, or one reduced panel
    verdict.

    Mirrors the relevant subset of evals/ARCHITECTURE.md's `Score` data
    model: `rationale` is the judge's CoT text, `bias_mitigations_applied`
    records which defenses (per THREAT_MODEL.md) were in effect for this
    call. `panel_votes`/`quorum_reached` are `None` for a single-judge
    call (the overwhelmingly common case, and every call before this
    module supported a panel at all) — populated only when `call_model`
    was a panel of more than one, per
    docs/rfcs/2026-09-08-evals-judge-panel-reducer.md.

    `quote_grounded` (per docs/rfcs/2026-09-09-evals-quote-grounded-
    verdict.md) is whether the judge's own QUOTE cites a real, verbatim
    span of the output or reference — measurement-only in this round,
    recorded but never used to discard or reweight a verdict. `None` for
    a `JudgeResult` built before this field existed; a real bool for a
    single-judge call. Deliberately never a `list[bool]` for a panel
    call, unlike `panel_votes` — callers read each panelist's own
    `quote_grounded` there instead, since it's always present alongside
    and duplicating it here would just be `[v.quote_grounded for v in
    panel_votes]` restated.
    """

    passed: bool
    rationale: str
    bias_mitigations_applied: list[str]
    panel_votes: list[PanelVote] | None = None
    quorum_reached: bool | None = None
    quote_grounded: bool | None = None


class PanelVerdict(BaseModel):
    """The reduced result of a multi-judge panel's independent verdicts,
    per docs/rfcs/2026-09-08-evals-judge-panel-reducer.md. Stays in this
    module (unlike `PanelVote`) — nothing downstream needs to import it;
    `judge()` unpacks it into a `JudgeResult` before returning.
    """

    passed: bool
    quorum_reached: bool
    votes: list[PanelVote]
    bias_mitigations_applied: list[str]


def reduce_panel_votes(votes: list[PanelVote]) -> PanelVerdict:
    """Majority-reduce independent judge votes — Inspect AI's
    `majority_score` shape: strict `count * 2 > panel_size`, per
    docs/upgrade-research/evals-judge-panel-reducer-2026-09-07.md's
    "Concrete reducer design for Kelvran" section.

    Fail-closed on no strict majority (`quorum_reached=False,
    passed=False`) — the guaranteed outcome of every 1-1 split on a
    2-judge panel (Kelvran's v1 panel size), and possible on any even
    split of a larger panel. This is a deliberate policy choice, not a
    default picked by omission: it follows this codebase's own existing
    fail-closed convention (docs/rfcs/2026-09-03-guardrails-pii-regex-
    classifier.md) exactly — when a verdict mechanism can't produce a
    confident answer, the safe direction is to withhold a pass, not grant
    one. Rejected alternative: default a tie to `passed=True`
    (fail-open) — rejected because a judge panel that can't agree has no
    analogous second, independent control the way `budget.Tracker`
    backstops the rate limiter's own fail-open default; there is no
    equivalent safety net here.

    Never called with an empty list — `judge()` already validates that
    `panel` is non-empty before ever reaching a panel of >1.
    """
    if not votes:
        raise ValueError("reduce_panel_votes() requires at least one vote")

    pass_count = sum(1 for v in votes if v.passed)
    fail_count = len(votes) - pass_count
    mitigations = BIAS_MITIGATIONS_APPLIED + _PANEL_BIAS_MITIGATIONS_ADDED

    if pass_count * 2 > len(votes):
        return PanelVerdict(
            passed=True,
            quorum_reached=True,
            votes=list(votes),
            bias_mitigations_applied=mitigations,
        )
    if fail_count * 2 > len(votes):
        return PanelVerdict(
            passed=False,
            quorum_reached=True,
            votes=list(votes),
            bias_mitigations_applied=mitigations,
        )
    return PanelVerdict(
        passed=False,
        quorum_reached=False,
        votes=list(votes),
        bias_mitigations_applied=mitigations,
    )


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


# Kelvran's own implementation of the "Combined Budget" technique's SHAPE
# (a merged CoT+rubric prompt, called twice with position swapped) named
# in docs/upgrade-research/evals-next-upgrade-2026-09-09.md's Finding 1 —
# an explicit three-point rubric checklist folded into one CoT-forcing
# prompt, not a verbatim reproduction of the cited paper's own exact
# wording (which this project doesn't have direct access to). Reference/
# candidate order is deliberately the one thing that swaps between the
# two templates below — everything else (the rubric itself, the QUOTE
# contract) is held identical, so a position effect is genuinely
# isolated to ordering, not confounded with a second, incidental prompt
# difference. See docs/rfcs/2026-09-09-evals-judge-debiasing-position-
# swap.md.
_DEBIASED_JUDGE_PROMPT_TEMPLATE_REFERENCE_FIRST = """\
You are an impartial grader comparing a candidate output against a reference answer.

Reference answer:
{reference}

Candidate output:
{output}

Work through this rubric, in order, before giving your verdict:
1. Factual accuracy: does the candidate state the same facts as the reference, \
with no contradictions or fabrications?
2. Completeness: does the candidate cover everything the reference covers, with \
no material omissions?
3. Phrasing equivalence: do any differences in wording, units, ordering, or \
format actually change the meaning?

Think step by step through all three points BEFORE giving your final verdict — \
do not state the verdict first. Then quote the exact, verbatim span (from \
either the reference answer or the candidate output above) that most directly \
grounds your verdict — do not paraphrase or summarize it.

Respond in exactly this format, with no other text:
REASONING: <your step-by-step reasoning through the rubric above>
QUOTE: <a verbatim quote from the reference answer or candidate output above>
VERDICT: <PASS or FAIL>
"""

_DEBIASED_JUDGE_PROMPT_TEMPLATE_CANDIDATE_FIRST = """\
You are an impartial grader comparing a candidate output against a reference answer.

Candidate output:
{output}

Reference answer:
{reference}

Work through this rubric, in order, before giving your verdict:
1. Factual accuracy: does the candidate state the same facts as the reference, \
with no contradictions or fabrications?
2. Completeness: does the candidate cover everything the reference covers, with \
no material omissions?
3. Phrasing equivalence: do any differences in wording, units, ordering, or \
format actually change the meaning?

Think step by step through all three points BEFORE giving your final verdict — \
do not state the verdict first. Then quote the exact, verbatim span (from \
either the reference answer or the candidate output above) that most directly \
grounds your verdict — do not paraphrase or summarize it.

Respond in exactly this format, with no other text:
REASONING: <your step-by-step reasoning through the rubric above>
QUOTE: <a verbatim quote from the reference answer or candidate output above>
VERDICT: <PASS or FAIL>
"""

_DEBIASED_JUDGE_PROMPT_TEMPLATE_WITH_AXIS_REFERENCE_FIRST = """\
You are an impartial grader comparing a candidate output against a reference answer.

Reference answer:
{reference}

Candidate output:
{output}

Grade specifically on this dimension: {axis}. Consider only this dimension when \
forming your verdict — a candidate output may be correct on other dimensions and \
still fail this one, or vice versa; do not let other dimensions influence this \
verdict.

Work through this rubric, in order, before giving your verdict, scoped to \
{axis} only:
1. Factual accuracy on this dimension: does the candidate contradict or \
fabricate anything relative to the reference, specifically on {axis}?
2. Completeness on this dimension: does the candidate cover everything the \
reference covers on {axis}, with no material omissions?
3. Phrasing equivalence on this dimension: do any wording/format differences \
actually change the {axis} judgment?

Think step by step through all three points BEFORE giving your final verdict — \
do not state the verdict first. Then quote the exact, verbatim span (from \
either the reference answer or the candidate output above) that most directly \
grounds your verdict — do not paraphrase or summarize it.

Respond in exactly this format, with no other text:
REASONING: <your step-by-step reasoning through the rubric above, scoped to \
{axis} only>
QUOTE: <a verbatim quote from the reference answer or candidate output above>
VERDICT: <PASS or FAIL>
"""

_DEBIASED_JUDGE_PROMPT_TEMPLATE_WITH_AXIS_CANDIDATE_FIRST = """\
You are an impartial grader comparing a candidate output against a reference answer.

Candidate output:
{output}

Reference answer:
{reference}

Grade specifically on this dimension: {axis}. Consider only this dimension when \
forming your verdict — a candidate output may be correct on other dimensions and \
still fail this one, or vice versa; do not let other dimensions influence this \
verdict.

Work through this rubric, in order, before giving your verdict, scoped to \
{axis} only:
1. Factual accuracy on this dimension: does the candidate contradict or \
fabricate anything relative to the reference, specifically on {axis}?
2. Completeness on this dimension: does the candidate cover everything the \
reference covers on {axis}, with no material omissions?
3. Phrasing equivalence on this dimension: do any wording/format differences \
actually change the {axis} judgment?

Think step by step through all three points BEFORE giving your final verdict — \
do not state the verdict first. Then quote the exact, verbatim span (from \
either the reference answer or the candidate output above) that most directly \
grounds your verdict — do not paraphrase or summarize it.

Respond in exactly this format, with no other text:
REASONING: <your step-by-step reasoning through the rubric above, scoped to \
{axis} only>
QUOTE: <a verbatim quote from the reference answer or candidate output above>
VERDICT: <PASS or FAIL>
"""


def build_debiased_judge_prompt(
    output: str,
    reference: str,
    position: Literal["reference_first", "candidate_first"],
    axis: str | None = None,
) -> str:
    """Build the position-swapped, rubric+CoT-merged debiasing prompt,
    per docs/rfcs/2026-09-09-evals-judge-debiasing-position-swap.md.

    `position` controls which of the reference answer / candidate
    output is presented first — the one deliberate axis this function
    introduces that `build_judge_prompt` has no concept of at all. A
    real caller (`evals.cli`'s `_debiased_judge_verdict`) calls this
    TWICE per judge, once per position, and combines the two verdicts;
    this function itself makes no network call and has no opinion on
    how its two outputs get combined — that policy lives in `evals.cli`,
    mirroring `judge()`'s own "pure prompt-building, zero I/O" precedent.

    `axis`, when given, scopes the verdict to a single named rubric
    dimension, composing with `position` independently (4 real template
    variants exist: 2 positions x {holistic, with-axis}).
    """
    if position not in ("reference_first", "candidate_first"):
        raise ValueError(
            f"position must be 'reference_first' or 'candidate_first', got {position!r}"
        )
    if axis is None:
        template = (
            _DEBIASED_JUDGE_PROMPT_TEMPLATE_REFERENCE_FIRST
            if position == "reference_first"
            else _DEBIASED_JUDGE_PROMPT_TEMPLATE_CANDIDATE_FIRST
        )
        return template.format(reference=reference, output=output)
    template = (
        _DEBIASED_JUDGE_PROMPT_TEMPLATE_WITH_AXIS_REFERENCE_FIRST
        if position == "reference_first"
        else _DEBIASED_JUDGE_PROMPT_TEMPLATE_WITH_AXIS_CANDIDATE_FIRST
    )
    return template.format(reference=reference, output=output, axis=axis)


@dataclass(frozen=True)
class ParsedJudgeResponse:
    """A single judge call's parsed response — a frozen value object
    (mirroring `evals.judge.providers.JudgeCallCost`'s own "small,
    internal-only, transient value" shape) rather than a bare tuple:
    `rationale`/`quote` are both plain strings, and a bare 2- or 3-tuple
    would be a real, silent transposition hazard at every call site.
    """

    passed: bool
    rationale: str
    quote: str


# A judge LLM very commonly wraps its "verbatim quote" in a matching pair
# of quote marks even when the prompt only asks for the span itself, with
# no formatting instruction either way — a real, observed model habit,
# not a hypothetical. Straight and curly variants both covered; the
# underlying span these wrap is still genuinely verbatim, only the added
# marks make a naive substring check miss it (a real false negative
# `quote_is_grounded`'s own design didn't anticipate).
_MATCHING_QUOTE_MARKS: dict[str, str] = {
    '"': '"',
    "'": "'",
    "“": "”",  # “ ”
    "‘": "’",  # ‘ ’
}


def _strip_one_matching_quote_mark_pair(text: str) -> str:
    """Strip a single leading/trailing matching quote-mark pair, if
    present — `'"Paris"'` -> `'Paris'`, but `'Paris'` (no marks) and
    mismatched marks (`'"Paris’'`) pass through unchanged.
    """
    if len(text) >= 2 and _MATCHING_QUOTE_MARKS.get(text[0]) == text[-1]:
        return text[1:-1]
    return text


def quote_is_grounded(quote: str, output: str, reference: str) -> bool:
    """Whether `quote` is a verbatim substring of `output` or
    `reference` — the measurement-only grounding check per
    docs/rfcs/2026-09-09-evals-quote-grounded-verdict.md. An empty quote
    (the judge omitted the QUOTE line, or emitted a blank one) is never
    grounded — a vacuous "True" for an empty string would defeat the
    whole point of the check.

    Falls back to checking the quote with one wrapping pair of quote
    marks stripped (see `_strip_one_matching_quote_mark_pair`) — ONLY
    when that strip actually changes the string AND leaves something
    non-empty, so a degenerate `'""'`/`"''"` quote (an empty pair of
    quote marks, itself never a real grounding) can never collapse to
    the vacuously-true `"" in output` check the leading empty-quote guard
    above exists to prevent.
    """
    if not quote:
        return False
    if quote in output or quote in reference:
        return True
    unwrapped = _strip_one_matching_quote_mark_pair(quote)
    if unwrapped and unwrapped != quote:
        return unwrapped in output or unwrapped in reference
    return False


def parse_judge_response(raw_response: str) -> ParsedJudgeResponse:
    verdict_match = _VERDICT_PATTERN.search(raw_response)
    if verdict_match is None:
        raise ValueError(
            f"judge response missing a VERDICT: PASS|FAIL line: {raw_response!r}"
        )
    passed = verdict_match.group(1).upper() == "PASS"

    reasoning_match = _REASONING_PATTERN.search(raw_response)
    rationale = reasoning_match.group(1).strip() if reasoning_match else ""

    quote_match = _QUOTE_PATTERN.search(raw_response)
    quote = quote_match.group(1).strip() if quote_match else ""

    return ParsedJudgeResponse(passed=passed, rationale=rationale, quote=quote)


async def judge(
    output: str,
    reference: str,
    call_model: CallModel | list[CallModel],
    axis: str | None = None,
) -> JudgeResult:
    """Score `output` against `reference` using an LLM judge, or a panel
    of them.

    `call_model` is either a single async dependency-injected callable, or
    a `list` of them (per docs/rfcs/2026-09-07-evals-judge-panel-interface
    .md — see this module's own docstring). Each callable takes the
    fully-built judge prompt and returns the judge model's raw text
    response. Production code wires a real provider SDK call; tests wire a
    scripted fake, so this function is fully unit testable with zero
    network calls and zero API keys.

    A `call_model` list of more than one entry is scored as a real panel,
    per docs/rfcs/2026-09-08-evals-judge-panel-reducer.md: every panelist
    is given the IDENTICAL prompt via `asyncio.gather` (independent
    refutation — none sees another's response or verdict), each parsed
    via `parse_judge_response`, then majority-reduced via
    `reduce_panel_votes` (see that function's own doc comment for the
    fail-closed tie-break policy). `judge()` itself has no way to know
    which real model id each opaque `call_model` wraps, so each panelist's
    `PanelVote.scorer_id` here is a positional placeholder
    (`"panelist_0"`, `"panelist_1"`, ...) — `reduce_panel_votes` only ever
    reads `.passed`, so this is harmless to the reduction itself; a caller
    that knows each panelist's real model id (e.g. `evals.cli`, which
    constructs the panel from named provider factories) is responsible
    for remapping these to real ids before persisting. Passing an empty
    list still raises `ValueError` — there is no judge to call.

    `axis`, when given, scopes this one call's verdict to a single named
    rubric dimension — see `build_judge_prompt`. `None` (the default)
    reproduces the exact original holistic-verdict behavior; the returned
    `JudgeResult` itself carries no `axis` field, since the caller already
    knows which axis it asked for and is responsible for recording it
    (e.g. as `Score.rubric_axis`) — not duplicated here.
    """
    panel = call_model if isinstance(call_model, list) else [call_model]
    if len(panel) == 0:
        raise ValueError("judge() requires at least one call_model, got an empty list")

    prompt = build_judge_prompt(output=output, reference=reference, axis=axis)

    if len(panel) == 1:
        raw_response = await panel[0](prompt)
        parsed = parse_judge_response(raw_response)
        return JudgeResult(
            passed=parsed.passed,
            rationale=parsed.rationale,
            bias_mitigations_applied=list(BIAS_MITIGATIONS_APPLIED),
            quote_grounded=quote_is_grounded(parsed.quote, output, reference),
        )

    raw_responses = await asyncio.gather(*(call(prompt) for call in panel))
    parsed_responses = [parse_judge_response(r) for r in raw_responses]
    votes = [
        PanelVote(
            scorer_id=f"panelist_{i}",
            passed=parsed.passed,
            rationale=parsed.rationale,
            trigger_quote=parsed.quote,
            quote_grounded=quote_is_grounded(parsed.quote, output, reference),
        )
        for i, parsed in enumerate(parsed_responses)
    ]
    verdict = reduce_panel_votes(votes)
    return JudgeResult(
        passed=verdict.passed,
        rationale="; ".join(f"{v.scorer_id}: {v.rationale}" for v in verdict.votes),
        bias_mitigations_applied=verdict.bias_mitigations_applied,
        panel_votes=verdict.votes,
        quorum_reached=verdict.quorum_reached,
    )
