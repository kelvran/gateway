"""Real Anthropic- and OpenAI-backed `call_model` implementations for
`judge()`.

This is the ONLY file in `evals/` that imports `anthropic` or `openai` —
per docs/rfcs/2026-09-04-evals-llm-judge-provider-wiring.md, `judge()`/
`llm_judge.py` are never modified, preserving that module's own
"zero network calls, testable without a live provider API key" property.
The OpenAI provider (added 2026-09-05, per that same RFC's own named
follow-on: "a same-shaped follow-on function") reuses the exact same
design the Anthropic provider established — same `call_model` contract,
same lazy-key, same `last_call_cost` side channel — deliberately, not
because it happened to be convenient, since `judge()`'s DI seam was
already real and this is exactly the kind of swap it exists for.

Importing this module never requires a key: both `AsyncAnthropic()` and
`AsyncOpenAI()` read their respective env vars (`ANTHROPIC_API_KEY`,
`OPENAI_API_KEY`) lazily, inside their own `make_*_call_model()`
factories, not at import time. Only calling a factory (or the callable it
returns) does.
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from decimal import Decimal

from anthropic import AsyncAnthropic
from anthropic.types import Usage
from openai import AsyncOpenAI
from openai.types import CompletionUsage as OpenAIUsage

# A pinned, dated snapshot id — never a bare alias — so the judge model
# never silently drifts underneath a suite of results without a
# deliberate, reviewable bump. Mirrors gateway/internal/guardrail's own
# guardrailPolicyVersion "bumped by hand" convention. Public (not
# underscore-prefixed) so cli.py can cite the real model id actually
# called as a Score.scorer_id, per docs/rfcs/2026-09-04-evals-score-
# model.md's harness-transparency requirement.
DEFAULT_JUDGE_MODEL = "claude-haiku-4-5-20251001"

# Anthropic Claude Haiku 4.5 (claude-haiku-4-5-20251001) BASE pricing, USD
# per million tokens — non-cached, non-batch (this module's call site sets
# no cache_control and has no batch path, so base pricing is always what
# applies). Live-fetched and cross-checked 2026-09-04 against
# https://platform.claude.com/docs/en/about-claude/pricing's Model pricing
# table (Claude Haiku 4.5 row: $1/MTok input, $5/MTok output) and
# https://claude.com/pricing. Anthropic's pricing pages carry no
# "last updated" date of their own and can change without this constant
# knowing about it. Bumped by hand only, on a deliberate, reviewable
# commit — mirrors guardrailPolicyVersion's "bumped by hand" convention
# (gateway/internal/guardrail). RE-VERIFY AGAINST https://claude.com/pricing
# BEFORE TRUSTING THIS IN PRODUCTION, especially after 2026-10-15 (Haiku
# 4.5's earliest possible Anthropic-committed retirement date).
#
# Decimal, built from strings (never a float literal) -- per
# docs/rfcs/2026-09-04-evals-score-model.md's own documented lesson,
# constructing a Decimal from a float intermediate (Decimal(1.00), not
# Decimal("1.00")) reintroduces the exact binary imprecision Decimal
# exists to prevent.
_JUDGE_MODEL_PRICE_PER_MTOK_USD: dict[str, tuple[Decimal, Decimal]] = {
    DEFAULT_JUDGE_MODEL: (Decimal("1.00"), Decimal("5.00")),  # (input, output)
}


def _compute_anthropic_cost_usd(model: str, usage: Usage) -> Decimal | None:
    """Compute a judge call's real cost from its response `usage`.

    Returns `None` for an unpriced model — honestly "not measured," never
    a fabricated `0` — mirroring the "`None` = not applicable" convention
    `Run.cost_usd`/`Score` already use elsewhere in this codebase. Division
    by 1,000,000 (a power of ten) is always exact for `Decimal`, never
    rounds.
    """
    prices = _JUDGE_MODEL_PRICE_PER_MTOK_USD.get(model)
    if prices is None:
        return None
    input_price, output_price = prices
    return (
        Decimal(usage.input_tokens) * input_price
        + Decimal(usage.output_tokens) * output_price
    ) / Decimal(1_000_000)


# OpenAI gpt-4o-mini (the real, current default for the OpenAI provider
# below) standard, non-batch, non-cached pricing, USD per million tokens.
# Live-fetched 2026-09-05 against
# https://developers.openai.com/api/docs/pricing (platform.openai.com/
# docs/pricing 301-redirects there): $0.15/MTok input, $0.60/MTok output.
# Same "bumped by hand only, re-verify before trusting in production"
# posture as _JUDGE_MODEL_PRICE_PER_MTOK_USD above — OpenAI's pricing page
# carries no machine-readable "last updated" date either.
OPENAI_DEFAULT_JUDGE_MODEL = "gpt-4o-mini"

_OPENAI_MODEL_PRICE_PER_MTOK_USD: dict[str, tuple[Decimal, Decimal]] = {
    OPENAI_DEFAULT_JUDGE_MODEL: (Decimal("0.15"), Decimal("0.60")),  # (input, output)
}


def _compute_openai_cost_usd(model: str, usage: OpenAIUsage) -> Decimal | None:
    """OpenAI analogue of `_compute_anthropic_cost_usd` — same `None`-for-
    unpriced convention, same exact-division property. Field names differ
    from Anthropic's `Usage` (`prompt_tokens`/`completion_tokens`, not
    `input_tokens`/`output_tokens`), confirmed directly against the
    installed `openai` SDK's real `CompletionUsage` model, not assumed
    from Anthropic's shape.
    """
    prices = _OPENAI_MODEL_PRICE_PER_MTOK_USD.get(model)
    if prices is None:
        return None
    input_price, output_price = prices
    return (
        Decimal(usage.prompt_tokens) * input_price
        + Decimal(usage.completion_tokens) * output_price
    ) / Decimal(1_000_000)


@dataclass(frozen=True)
class JudgeCallCost:
    """The real token usage and computed cost of one judge call."""

    input_tokens: int
    output_tokens: int
    cost_usd: Decimal | None


class _AnthropicCallModel:
    """A `call_model` implementation backed by a real Anthropic API call.

    Exposes `last_call_cost`, rebound (never mutated in place) to a fresh
    `JudgeCallCost` after every call — a deliberate, narrow exception to
    the org-wide "never mutate, always create new objects" rule, forced by
    `judge()`'s contract: `call_model` is plain
    `Callable[[str], Awaitable[str]]` (text in, text out), and `judge()`
    itself discards the raw response after parsing it, so this side
    channel on an object the caller already holds a reference to is the
    only way cost data survives without widening `judge()`'s signature or
    touching `llm_judge.py` at all — see
    docs/rfcs/2026-09-04-evals-llm-judge-provider-wiring.md and
    docs/rfcs/2026-09-04-evals-score-model.md's own "judge()/llm_judge.py
    untouched" invariant.

    Safe only because callers use this sequentially (one `judge()` call
    awaited, `last_call_cost` read, before the next call) — v1's CLI never
    runs judge calls concurrently. A future concurrent caller would need
    per-call storage instead of this one shared attribute.
    """

    def __init__(self, model: str, client: AsyncAnthropic) -> None:
        self._model = model
        self._client = client
        self.last_call_cost: JudgeCallCost | None = None

    async def __call__(self, prompt: str) -> str:
        response = await self._client.messages.create(
            model=self._model,
            max_tokens=1024,
            messages=[{"role": "user", "content": prompt}],
        )
        self.last_call_cost = JudgeCallCost(
            input_tokens=response.usage.input_tokens,
            output_tokens=response.usage.output_tokens,
            cost_usd=_compute_anthropic_cost_usd(self._model, response.usage),
        )
        return response.content[0].text


def make_anthropic_call_model(
    model: str = DEFAULT_JUDGE_MODEL,
    client: AsyncAnthropic | None = None,
) -> Callable[[str], Awaitable[str]]:
    """Build a `call_model` callable backed by a real Anthropic API call.

    The returned object satisfies `Callable[[str], Awaitable[str]]` (it
    implements `__call__`) and additionally exposes `last_call_cost` —
    see `_AnthropicCallModel`.
    """
    anthropic_client = client or AsyncAnthropic()
    return _AnthropicCallModel(model=model, client=anthropic_client)


class _OpenAICallModel:
    """A `call_model` implementation backed by a real OpenAI API call —
    the OpenAI analogue of `_AnthropicCallModel`, same design, same
    sequential-callers-only `last_call_cost` caveat (see that class's own
    docstring for the full reasoning, not repeated here).
    """

    def __init__(self, model: str, client: AsyncOpenAI) -> None:
        self._model = model
        self._client = client
        self.last_call_cost: JudgeCallCost | None = None

    async def __call__(self, prompt: str) -> str:
        response = await self._client.chat.completions.create(
            model=self._model,
            messages=[{"role": "user", "content": prompt}],
        )
        usage = response.usage
        self.last_call_cost = JudgeCallCost(
            input_tokens=usage.prompt_tokens if usage else 0,
            output_tokens=usage.completion_tokens if usage else 0,
            cost_usd=_compute_openai_cost_usd(self._model, usage) if usage else None,
        )
        content = response.choices[0].message.content
        if content is None:
            raise ValueError(
                f"OpenAI judge call for model {self._model!r} returned no text "
                "content (a real refusal or a content-filtered response) — judge() "
                "cannot score an empty verdict"
            )
        return content


def make_openai_call_model(
    model: str = OPENAI_DEFAULT_JUDGE_MODEL,
    client: AsyncOpenAI | None = None,
) -> Callable[[str], Awaitable[str]]:
    """Build a `call_model` callable backed by a real OpenAI API call —
    the same-shaped follow-on function
    docs/rfcs/2026-09-04-evals-llm-judge-provider-wiring.md named as
    ready-made once a second provider was actually needed.

    The returned object satisfies `Callable[[str], Awaitable[str]]` (it
    implements `__call__`) and additionally exposes `last_call_cost` —
    see `_OpenAICallModel`.
    """
    openai_client = client or AsyncOpenAI()
    return _OpenAICallModel(model=model, client=openai_client)
