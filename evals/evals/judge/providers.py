"""Real Anthropic-backed `call_model` implementation for `judge()`.

This is the ONLY file in `evals/` that imports `anthropic` — per
docs/rfcs/2026-09-04-evals-llm-judge-provider-wiring.md, `judge()`/
`llm_judge.py` are never modified, preserving that module's own
"zero network calls, testable without a live provider API key" property.

Importing this module never requires a key: `AsyncAnthropic()` reads
`ANTHROPIC_API_KEY` from the environment lazily, inside
`make_anthropic_call_model()`, not at import time. Only calling the
factory (or the closure it returns) does.
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable

from anthropic import AsyncAnthropic

# A pinned, dated snapshot id — never a bare alias — so the judge model
# never silently drifts underneath a suite of results without a
# deliberate, reviewable bump. Mirrors gateway/internal/guardrail's own
# guardrailPolicyVersion "bumped by hand" convention.
_DEFAULT_JUDGE_MODEL = "claude-haiku-4-5-20251001"


def make_anthropic_call_model(
    model: str = _DEFAULT_JUDGE_MODEL,
    client: AsyncAnthropic | None = None,
) -> Callable[[str], Awaitable[str]]:
    """Build a `call_model` closure backed by a real Anthropic API call."""
    anthropic_client = client or AsyncAnthropic()

    async def call_model(prompt: str) -> str:
        response = await anthropic_client.messages.create(
            model=model,
            max_tokens=1024,
            messages=[{"role": "user", "content": prompt}],
        )
        return response.content[0].text

    return call_model
