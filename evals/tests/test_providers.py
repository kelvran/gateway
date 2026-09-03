from __future__ import annotations

import asyncio

from evals.judge.providers import make_anthropic_call_model


class _FakeTextBlock:
    def __init__(self, text: str) -> None:
        self.text = text


class _FakeMessage:
    def __init__(self, text: str) -> None:
        self.content = [_FakeTextBlock(text)]


class _FakeMessagesResource:
    def __init__(self, response_text: str) -> None:
        self.response_text = response_text
        self.calls: list[dict] = []

    async def create(self, **kwargs):
        self.calls.append(kwargs)
        return _FakeMessage(self.response_text)


class _FakeAsyncAnthropic:
    def __init__(self, response_text: str) -> None:
        self.messages = _FakeMessagesResource(response_text)


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
