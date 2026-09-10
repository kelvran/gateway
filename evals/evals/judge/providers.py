"""Real Anthropic-, OpenAI-, and Bedrock-backed `call_model`
implementations for `judge()`.

This is the ONLY file in `evals/` that imports `anthropic` or `openai` —
per docs/rfcs/2026-09-04-evals-llm-judge-provider-wiring.md, that
property has never changed (`boto3`, added below, is already a real
`evals/` dependency for object-storage ingestion, per docs/rfcs/2026-09-
07-evals-trace-ingestion-object-storage.md — not a new SDK surface for
this file to uniquely own the way `anthropic`/`openai` are). `judge()`/
`llm_judge.py`'s scoring logic itself was originally never modified
either, but that narrower claim no longer holds as of docs/rfcs/2026-09-
07-evals-judge-panel-interface.md, which widened `judge()`'s `call_model`
type to also accept a `list` — still zero SDK imports and zero network
calls added to that module, so this file remains the sole SDK-importing
file and `judge()` remains fully testable without a live provider API
key, unchanged.
The OpenAI provider (added 2026-09-05, per that same RFC's own named
follow-on: "a same-shaped follow-on function") reuses the exact same
design the Anthropic provider established — same `call_model` contract,
same lazy-key, same `last_call_cost` side channel — deliberately, not
because it happened to be convenient, since `judge()`'s DI seam was
already real and this is exactly the kind of swap it exists for.

The Bedrock provider (added 2026-09-08, per docs/rfcs/2026-09-08-evals-
judge-panel-reducer.md's revised panel composition) is the real
`--llm-judge-panel` judge pair going forward: two Claude models (Sonnet 5
+ Haiku 4.5) invoked via AWS Bedrock's Converse API instead of Anthropic
+ OpenAI's own direct APIs — an explicit, accepted tradeoff (both judges
share the same vendor/architecture/RLHF lineage, so the panel's bias-
reduction premise is weaker than a genuinely disjoint pair; see that
RFC's own honest accounting of this) made deliberately for AWS-only
operational simplicity. `boto3`'s `bedrock-runtime` client is
synchronous — `_BedrockCallModel.__call__` wraps the blocking
`client.converse()` call in `asyncio.to_thread`, per this project's own
"no blocking calls inside async functions" convention (`AGENTS.md`), the
one place in this module a call isn't already natively async (unlike
`AsyncAnthropic`/`AsyncOpenAI`).

Importing this module never requires a key: `AsyncAnthropic()`,
`AsyncOpenAI()`, and `boto3.client("bedrock-runtime")` all read their
respective credentials (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and
boto3's own standard AWS credential chain — `AWS_ACCESS_KEY_ID`/
`AWS_SECRET_ACCESS_KEY`/`AWS_REGION` env vars, a shared credentials file,
or an IAM role, never a Kelvran-specific env var name) lazily, inside
their own `make_*_call_model()` factories, not at import time. Only
calling a factory (or the callable it returns) does.
"""

from __future__ import annotations

import asyncio
import os
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from decimal import Decimal
from typing import Any

import boto3
from anthropic import AsyncAnthropic
from anthropic.types import Usage
from botocore.config import Config as BotoConfig
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

    Two call sites now depend on this invariant, not one:
    `evals.cli`'s ordinary judge-invocation path, and (added 2026-09-09,
    per docs/rfcs/2026-09-09-evals-judge-debiasing-position-swap.md)
    `_debiased_judge_verdict`'s position-swapped debiasing helper, which
    deliberately makes its two calls to the SAME `call_model` instance
    sequentially, reading `last_call_cost` after each, specifically to
    stay safe under this exact constraint rather than racing on it via
    `asyncio.gather`. The real trigger for finally fixing this properly
    (returning cost alongside the response instead of via this side
    channel) is the day two calls to the same instance genuinely need to
    run concurrently — which doesn't exist even with debiasing on, since
    debiasing's own design keeps its two calls sequential by
    construction.
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


# Real, current Bedrock model IDs for Claude Sonnet 5 and Claude Haiku 4.5,
# verified 2026-09-08 directly against AWS's own live model-card pages
# (docs.aws.amazon.com/bedrock/latest/userguide/model-card-anthropic-
# claude-{sonnet-5,haiku-4-5}.html's own "Programmatic Access" tables) —
# never guessed from a plausible-looking naming pattern. Note the real,
# asymmetric naming confirmed directly from AWS's own docs: Sonnet 5's id
# carries no date suffix; Haiku 4.5's does.
#
# Deliberately the `global.`-prefixed cross-region inference profile ID,
# not the bare `bedrock-runtime` model ID — a real, live-verified
# correction (2026-09-08) to this constant's own prior assumption that
# the bare form was the correct single-region default: a real Converse
# call against the bare id fails outright with `ValidationException:
# Invocation of model ID ... with on-demand throughput isn't supported.
# Retry your request with the ID or ARN of an inference profile that
# contains this model.` Neither Sonnet 5 nor Haiku 4.5 supports on-demand
# invocation by bare id at all on this account — confirmed by a real
# live call, not inferred from documentation prose. `global.` (over a
# `us.`/`eu.`/`au.`/`jp.` geo-scoped profile) is used so this never needs
# to track whatever region `AWS_REGION` happens to be set to — AWS's own
# docs confirm global cross-Region inference is supported for on-demand
# model inference; both ids were re-verified live against this exact
# credential before landing.
BEDROCK_SONNET_5_MODEL_ID = "global.anthropic.claude-sonnet-5"
BEDROCK_HAIKU_4_5_MODEL_ID = "global.anthropic.claude-haiku-4-5-20251001-v1:0"

# botocore's own default read_timeout (60s) is too short for a generous
# max_tokens budget against an extended-thinking model on a long prompt —
# a REAL, live-discovered gotcha (2026-09-11, while running
# evals.audit_corpus's own full-corpus live sanity pass, per
# docs/rfcs/2026-09-11-evals-audit-corpus.md): a real Converse call to
# Sonnet 5 with max_tokens=8192 against a long audit prompt exceeded 60s
# and raised botocore.exceptions.ReadTimeoutError mid-run, confirmed by
# reading the raw traceback directly ("read timeout=60"). This is
# distinct from (and a second real gap alongside) the missing-maxTokens
# gotcha documented on make_bedrock_call_model below -- that gap causes
# an empty response; this one causes a real network-level timeout even
# once maxTokens is set generously enough to get a real response.
# Applied to every Bedrock client this factory builds, not just
# audit_corpus_cmd's own generous-max_tokens caller: any future caller
# raising max_tokens is exposed to the identical timeout risk.
_BEDROCK_READ_TIMEOUT_SECONDS = 300

# Bedrock bills these two (both third-party models) through AWS
# Marketplace, per each model's own live "Pricing" section, which defers
# to a separate, JS-rendered pricing page rather than publishing an
# inline per-token number on the model card itself — a live fetch of that
# page did not surface a real, current per-token rate for either model,
# confirmed independently TWICE now (this pass re-checked it, 2026-09-09,
# specifically re-verifying the original judge-panel build's own earlier
# same finding): aws.amazon.com/bedrock/pricing's rendered Anthropic
# on-demand table shows only legacy "Claude 3.5 Sonnet"/"Claude 3.5 Sonnet
# v2" rows — no row for Sonnet 5 or Haiku 4.5 exists on that page at all,
# consistent with (though not confirmed as caused by) the same page's own
# note that some newer, non-GA Anthropic model access is still gated.
#
# Anthropic's OWN direct-API pricing for these exact model names IS
# published and was verified live (claude.com/pricing, 2026-09-09): Sonnet
# 5 is $2/$10 per MTok (input/output), Haiku 4.5 is $1/$5 per MTok. This
# is deliberately NOT used as a substitute Bedrock rate — AWS Marketplace
# billing for a third-party foundation model is not guaranteed to match
# that vendor's own direct-API price, and no independent confirmation of
# parity for these two specific models exists. Reusing it here would be
# exactly the kind of fabricated-estimate substitution this convention
# exists to prevent, not a reasonable approximation.
#
# So: NEITHER model gets a price-table entry here, still. This means
# `_compute_bedrock_cost_usd` honestly returns `None` for both today,
# mirroring `_compute_anthropic_cost_usd`/`_compute_openai_cost_usd`'s own
# established "no price-table entry -> genuinely unmeasured, never a
# fabricated estimate" convention exactly — not a gap unique to this
# provider. Add real entries here once verified directly against a live
# AWS bill or the rendered Bedrock pricing page ACTUALLY showing a rate
# for these model ids specifically — the same "bumped by hand only,
# re-verify before trusting in production" posture the other two price
# tables already carry.
_BEDROCK_MODEL_PRICE_PER_MTOK_USD: dict[str, tuple[Decimal, Decimal]] = {}


def _compute_bedrock_cost_usd(
    model: str, input_tokens: int, output_tokens: int
) -> Decimal | None:
    """Bedrock analogue of `_compute_anthropic_cost_usd`/
    `_compute_openai_cost_usd` — same `None`-for-unpriced convention.
    Takes raw token counts (not a provider-specific `Usage` object) since
    Bedrock Converse's own `usage` shape (`inputTokens`/`outputTokens`, a
    plain dict via `boto3`) doesn't need a typed wrapper the way the
    `anthropic`/`openai` SDKs' response objects do.
    """
    prices = _BEDROCK_MODEL_PRICE_PER_MTOK_USD.get(model)
    if prices is None:
        return None
    input_price, output_price = prices
    return (
        Decimal(input_tokens) * input_price + Decimal(output_tokens) * output_price
    ) / Decimal(1_000_000)


class _BedrockCallModel:
    """A `call_model` implementation backed by a real AWS Bedrock Converse
    API call — the Bedrock analogue of `_AnthropicCallModel`/
    `_OpenAICallModel`, same `last_call_cost` side-channel design and its
    same sequential-callers-only caveat (see `_AnthropicCallModel`'s own
    docstring for the full reasoning, not repeated here).

    Unlike the Anthropic/OpenAI SDKs, `boto3`'s Bedrock client is
    synchronous — `__call__` wraps the blocking `client.converse()` call
    in `asyncio.to_thread`, per this project's own "no blocking calls
    inside async functions" convention (`AGENTS.md`), so this class still
    satisfies `Callable[[str], Awaitable[str]]` without blocking the event
    loop.

    Converse's real request/response wire shape (`messages[].content[]
    {text}`, `usage.inputTokens`/`usage.outputTokens`,
    `output.message.content[].text`) was cross-verified directly against
    this same repo's own `gateway/internal/adapter/bedrock/bedrock.go` —
    Kelvran's gateway already talks to this exact API in Go — rather than
    assumed from general knowledge of the Converse API.

    `client` is typed `Any`, not a real `boto3` client type: `boto3`
    generates its client classes dynamically at runtime with no
    importable static type (confirmed — this repo's existing `boto3`
    caller, `evals/ingestion/object_store.py`, doesn't type its own
    client variable either), so `Any` here is honest, not a skipped type.
    """

    def __init__(self, model: str, client: Any, max_tokens: int | None = None) -> None:
        self._model = model
        self._client = client
        self._max_tokens = max_tokens
        self.last_call_cost: JudgeCallCost | None = None

    def _invoke(self, prompt: str) -> dict:
        kwargs: dict[str, Any] = {
            "modelId": self._model,
            "messages": [{"role": "user", "content": [{"text": prompt}]}],
        }
        if self._max_tokens is not None:
            kwargs["inferenceConfig"] = {"maxTokens": self._max_tokens}
        return self._client.converse(**kwargs)

    async def __call__(self, prompt: str) -> str:
        response = await asyncio.to_thread(self._invoke, prompt)
        usage = response["usage"]
        input_tokens = usage["inputTokens"]
        output_tokens = usage["outputTokens"]
        self.last_call_cost = JudgeCallCost(
            input_tokens=input_tokens,
            output_tokens=output_tokens,
            cost_usd=_compute_bedrock_cost_usd(
                self._model, input_tokens, output_tokens
            ),
        )
        content_blocks = response["output"]["message"]["content"]
        text_blocks = [b["text"] for b in content_blocks if "text" in b]
        if not text_blocks:
            raise ValueError(
                f"Bedrock Converse call for model {self._model!r} returned no text "
                "content block (a real refusal or a tool-only response) — judge() "
                "cannot score an empty verdict"
            )
        return "".join(text_blocks)


def make_bedrock_call_model(
    model: str,
    client: Any | None = None,
    region_name: str | None = None,
    max_tokens: int | None = None,
) -> Callable[[str], Awaitable[str]]:
    """Build a `call_model` callable backed by a real AWS Bedrock Converse
    API call, per docs/rfcs/2026-09-08-evals-judge-panel-reducer.md's
    revised panel composition.

    `model` is required (unlike `make_anthropic_call_model`/
    `make_openai_call_model`'s own defaulted `model` parameter) —
    deliberately, since this factory is called twice with two DIFFERENT
    real model ids (`BEDROCK_SONNET_5_MODEL_ID`,
    `BEDROCK_HAIKU_4_5_MODEL_ID`) to build the panel's two disjoint-size
    (not disjoint-vendor — see the RFC's own honest accounting of this
    tradeoff) judges; a single implicit default would silently produce
    two identical judges if a caller forgot to pass `model` twice.

    Credentials resolve via `boto3`'s own standard AWS credential chain
    (`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` env vars, a shared
    credentials file, or an IAM role) — never a Kelvran-specific env var
    name, so this factory composes with however AWS credentials are
    already configured for any other caller in this environment (e.g.
    the object-storage ingestion path's own `boto3` usage).

    Region resolution is NOT left to boto3's own default session,
    deliberately: a real, live-verified finding (2026-09-08) is that this
    project's own pinned botocore version only honors `AWS_DEFAULT_REGION`
    for a bare `boto3.client(region_name=None)` call, NOT `AWS_REGION`
    alone, despite AWS's own newer-SDK convention treating `AWS_REGION`
    as the primary name — a real gap between documented and observed
    behavior, caught only because a clean CI environment (no
    `~/.aws/config` fallback) surfaced it after this passed locally
    (silently masked by a `region` already set in this machine's own
    `~/.aws/config`, unrelated to `evals`). `region_name`, when given,
    wins outright; otherwise `AWS_REGION` is checked first (the name
    `evals/.env.example` documents and the more common cross-SDK
    convention), falling back to `AWS_DEFAULT_REGION` — resolved here
    explicitly and passed to `boto3.client(...)` as a real value, never
    left as `None` for botocore's own env-var scan to (unreliably) find.

    `max_tokens`, when given, sets `inferenceConfig.maxTokens` on the
    real Converse call. Omitting it (the default, used by every
    `--llm-judge`/`--llm-judge-panel` caller, unchanged) leaves Bedrock's
    own service-side default in effect — a REAL, live-discovered gotcha
    (2026-09-11, while sanity-checking `evals.audit_corpus`, per
    docs/rfcs/2026-09-11-evals-audit-corpus.md): against a long,
    JSON-heavy audit prompt (a full `EvalCase.task_spec`, unlike a short
    judge comparison prompt), Claude Sonnet 5 spent its ENTIRE default
    token budget on an internal `reasoningContent` block before ever
    emitting visible text, surfacing as `_BedrockCallModel.__call__`'s
    own "no text content block" `ValueError` — `stopReason: "max_tokens"`
    with an empty `reasoningText.text`, confirmed by inspecting the raw
    Converse response directly. `audit_corpus_cmd` passes an explicit,
    generous `max_tokens` to avoid this; the underlying gap (no
    `maxTokens` control at all) is pre-existing and shared by every
    `--llm-judge`/`--llm-judge-panel` call site too, none of which have
    hit it yet only because their prompts are short — named here as a
    real, disclosed latent risk, not silently fixed for those callers in
    this pass.

    Every client built by this factory (not just callers who pass
    `max_tokens`) uses `_BEDROCK_READ_TIMEOUT_SECONDS` (300s) instead of
    botocore's own 60s default — see that constant's own doc comment for
    the second, distinct real gotcha this closes (a genuine network-level
    `ReadTimeoutError`, not the empty-response gotcha above).
    """
    resolved_region = (
        region_name
        or os.environ.get("AWS_REGION")
        or os.environ.get("AWS_DEFAULT_REGION")
    )
    bedrock_client = client or boto3.client(
        "bedrock-runtime",
        region_name=resolved_region,
        config=BotoConfig(read_timeout=_BEDROCK_READ_TIMEOUT_SECONDS),
    )
    return _BedrockCallModel(model=model, client=bedrock_client, max_tokens=max_tokens)
