from __future__ import annotations

import asyncio
from decimal import Decimal

from evals.judge.providers import (
    DEFAULT_JUDGE_MODEL,
    OPENAI_DEFAULT_JUDGE_MODEL,
    _compute_anthropic_cost_usd,
    _compute_openai_cost_usd,
    make_anthropic_call_model,
    make_openai_call_model,
)


class _FakeUsage:
    def __init__(self, input_tokens: int = 10, output_tokens: int = 5) -> None:
        self.input_tokens = input_tokens
        self.output_tokens = output_tokens


class _FakeTextBlock:
    def __init__(self, text: str) -> None:
        self.text = text


class _FakeMessage:
    def __init__(self, text: str, usage: _FakeUsage | None = None) -> None:
        self.content = [_FakeTextBlock(text)]
        self.usage = usage or _FakeUsage()


class _FakeMessagesResource:
    def __init__(self, response_text: str, usage: _FakeUsage | None = None) -> None:
        self.response_text = response_text
        self.usage = usage
        self.calls: list[dict] = []

    async def create(self, **kwargs):
        self.calls.append(kwargs)
        return _FakeMessage(self.response_text, usage=self.usage)


class _FakeAsyncAnthropic:
    def __init__(self, response_text: str, usage: _FakeUsage | None = None) -> None:
        self.messages = _FakeMessagesResource(response_text, usage=usage)


def test_call_model_sends_model_and_prompt_to_messages_create():
    fake_client = _FakeAsyncAnthropic("REASONING: fine.\nVERDICT: PASS\n")
    call_model = make_anthropic_call_model(
        model="claude-haiku-4-5-20251001", client=fake_client
    )

    result = asyncio.run(call_model("here is the judge prompt"))

    assert result == "REASONING: fine.\nVERDICT: PASS\n"
    assert len(fake_client.messages.calls) == 1
    call = fake_client.messages.calls[0]
    assert call["model"] == "claude-haiku-4-5-20251001"
    assert call["messages"] == [{"role": "user", "content": "here is the judge prompt"}]


def test_call_model_extracts_text_from_first_content_block():
    fake_client = _FakeAsyncAnthropic("some response text")
    call_model = make_anthropic_call_model(client=fake_client)

    result = asyncio.run(call_model("prompt"))

    assert result == "some response text"


def test_make_anthropic_call_model_uses_default_model_when_unspecified():
    fake_client = _FakeAsyncAnthropic("x")
    call_model = make_anthropic_call_model(client=fake_client)

    asyncio.run(call_model("prompt"))

    assert fake_client.messages.calls[0]["model"] == "claude-haiku-4-5-20251001"


def test_importing_providers_module_never_requires_an_api_key(monkeypatch):
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)

    import importlib

    import evals.judge.providers as providers_module

    importlib.reload(providers_module)


def test_call_model_records_last_call_cost_from_real_usage():
    fake_client = _FakeAsyncAnthropic(
        "REASONING: fine.\nVERDICT: PASS\n",
        usage=_FakeUsage(input_tokens=1000, output_tokens=500),
    )
    call_model = make_anthropic_call_model(client=fake_client)

    assert call_model.last_call_cost is None

    asyncio.run(call_model("prompt"))

    assert call_model.last_call_cost is not None
    assert call_model.last_call_cost.input_tokens == 1000
    assert call_model.last_call_cost.output_tokens == 500
    # 1000 input tokens @ $1/MTok + 500 output tokens @ $5/MTok.
    expected = (
        Decimal(1000) * Decimal("1.00") + Decimal(500) * Decimal("5.00")
    ) / Decimal(1_000_000)
    assert call_model.last_call_cost.cost_usd == expected


def test_call_model_rebinds_last_call_cost_across_calls_not_mutates():
    fake_client = _FakeAsyncAnthropic(
        "x", usage=_FakeUsage(input_tokens=10, output_tokens=5)
    )
    call_model = make_anthropic_call_model(client=fake_client)

    asyncio.run(call_model("prompt"))
    first_cost = call_model.last_call_cost

    fake_client.messages.usage = _FakeUsage(input_tokens=20, output_tokens=10)
    asyncio.run(call_model("prompt"))
    second_cost = call_model.last_call_cost

    assert first_cost.input_tokens == 10
    assert second_cost.input_tokens == 20
    assert first_cost is not second_cost


def test_compute_cost_usd_returns_none_for_an_unpriced_model():
    assert _compute_anthropic_cost_usd("some-unpinned-model", _FakeUsage()) is None


def test_compute_cost_usd_computes_real_price_for_default_judge_model():
    usage = _FakeUsage(input_tokens=2_000_000, output_tokens=1_000_000)
    cost = _compute_anthropic_cost_usd(DEFAULT_JUDGE_MODEL, usage)

    # 2M input tokens @ $1/MTok + 1M output tokens @ $5/MTok = $2 + $5.
    assert cost == Decimal("7.00")


class _FakeOpenAIUsage:
    def __init__(self, prompt_tokens: int = 10, completion_tokens: int = 5) -> None:
        self.prompt_tokens = prompt_tokens
        self.completion_tokens = completion_tokens


class _FakeOpenAIMessage:
    def __init__(self, content: str | None) -> None:
        self.content = content


class _FakeOpenAIChoice:
    def __init__(self, content: str | None) -> None:
        self.message = _FakeOpenAIMessage(content)


class _FakeChatCompletion:
    def __init__(self, content: str | None, usage: _FakeOpenAIUsage | None) -> None:
        self.choices = [_FakeOpenAIChoice(content)]
        self.usage = usage


class _FakeCompletionsResource:
    def __init__(
        self, response_text: str | None, usage: _FakeOpenAIUsage | None = None
    ) -> None:
        self.response_text = response_text
        self.usage = usage
        self.calls: list[dict] = []

    async def create(self, **kwargs):
        self.calls.append(kwargs)
        return _FakeChatCompletion(self.response_text, usage=self.usage)


class _FakeChatResource:
    def __init__(
        self, response_text: str | None, usage: _FakeOpenAIUsage | None = None
    ) -> None:
        self.completions = _FakeCompletionsResource(response_text, usage=usage)


class _FakeAsyncOpenAI:
    def __init__(
        self, response_text: str | None, usage: _FakeOpenAIUsage | None = None
    ) -> None:
        self.chat = _FakeChatResource(response_text, usage=usage)


def test_openai_call_model_sends_model_and_prompt_to_chat_completions_create():
    fake_client = _FakeAsyncOpenAI("REASONING: fine.\nVERDICT: PASS\n")
    call_model = make_openai_call_model(model="gpt-4o-mini", client=fake_client)

    result = asyncio.run(call_model("here is the judge prompt"))

    assert result == "REASONING: fine.\nVERDICT: PASS\n"
    assert len(fake_client.chat.completions.calls) == 1
    call = fake_client.chat.completions.calls[0]
    assert call["model"] == "gpt-4o-mini"
    assert call["messages"] == [{"role": "user", "content": "here is the judge prompt"}]


def test_make_openai_call_model_uses_default_model_when_unspecified():
    fake_client = _FakeAsyncOpenAI("x")
    call_model = make_openai_call_model(client=fake_client)

    asyncio.run(call_model("prompt"))

    assert fake_client.chat.completions.calls[0]["model"] == OPENAI_DEFAULT_JUDGE_MODEL


def test_openai_call_model_raises_on_none_content():
    # A real refusal or content-filtered response leaves .content = None —
    # judge() cannot score an empty verdict, so this must fail loudly, not
    # silently return an empty string.
    fake_client = _FakeAsyncOpenAI(None)
    call_model = make_openai_call_model(client=fake_client)

    try:
        asyncio.run(call_model("prompt"))
        raised = False
    except ValueError:
        raised = True
    assert raised, "expected a ValueError for None message content"


def test_openai_call_model_records_last_call_cost_from_real_usage():
    fake_client = _FakeAsyncOpenAI(
        "REASONING: fine.\nVERDICT: PASS\n",
        usage=_FakeOpenAIUsage(prompt_tokens=1_000_000, completion_tokens=1_000_000),
    )
    call_model = make_openai_call_model(client=fake_client)

    assert call_model.last_call_cost is None

    asyncio.run(call_model("prompt"))

    assert call_model.last_call_cost is not None
    assert call_model.last_call_cost.input_tokens == 1_000_000
    assert call_model.last_call_cost.output_tokens == 1_000_000
    # 1M input tokens @ $0.15/MTok + 1M output tokens @ $0.60/MTok.
    expected = Decimal("0.15") + Decimal("0.60")
    assert call_model.last_call_cost.cost_usd == expected


def test_openai_call_model_handles_missing_usage_as_none_cost():
    # response.usage is a genuinely Optional field on the real OpenAI SDK
    # type -- must not crash, and must honestly report "not measured".
    fake_client = _FakeAsyncOpenAI("x", usage=None)
    call_model = make_openai_call_model(client=fake_client)

    asyncio.run(call_model("prompt"))

    assert call_model.last_call_cost is not None
    assert call_model.last_call_cost.cost_usd is None
    assert call_model.last_call_cost.input_tokens == 0
    assert call_model.last_call_cost.output_tokens == 0


def test_importing_providers_module_never_requires_an_openai_api_key(monkeypatch):
    monkeypatch.delenv("OPENAI_API_KEY", raising=False)

    import importlib

    import evals.judge.providers as providers_module

    importlib.reload(providers_module)


def test_compute_openai_cost_usd_returns_none_for_an_unpriced_model():
    assert _compute_openai_cost_usd("some-unpinned-model", _FakeOpenAIUsage()) is None


def test_compute_openai_cost_usd_computes_real_price_for_default_judge_model():
    usage = _FakeOpenAIUsage(prompt_tokens=2_000_000, completion_tokens=1_000_000)
    cost = _compute_openai_cost_usd(OPENAI_DEFAULT_JUDGE_MODEL, usage)

    # 2M input tokens @ $0.15/MTok + 1M output tokens @ $0.60/MTok.
    assert cost == Decimal("0.30") + Decimal("0.60")
