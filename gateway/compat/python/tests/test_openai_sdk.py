"""The official `openai` package against the gateway's OpenAI-compatible routes,
served by an anthropic deployment through the translate path."""

import openai
import pytest

EXPECTED_TEXT = "It's 72°F and sunny in Boston."
EXPECTED_STREAM_TEXT = "The sky is blue today."

Turn = list[dict[str, str]]


def _client(url: str, credential: str) -> openai.OpenAI:
    return openai.OpenAI(base_url=f"{url}/v1", api_key=credential, max_retries=0)


def test_buffered_chat_completion(
    gateway_url: str, gateway_credential: str, model: str, question: Turn
) -> None:
    completion = _client(gateway_url, gateway_credential).chat.completions.create(
        model=model, messages=question
    )
    assert completion.choices[0].message.content == EXPECTED_TEXT
    assert completion.choices[0].finish_reason == "stop"
    assert completion.usage is not None
    assert completion.usage.prompt_tokens == 58


def test_stream_accumulates(
    gateway_url: str, gateway_credential: str, model: str, question: Turn
) -> None:
    chunks = _client(gateway_url, gateway_credential).chat.completions.create(
        model=model, messages=question, stream=True
    )
    text = ""
    finish = None
    for chunk in chunks:
        if not chunk.choices:
            continue  # a usage-only chunk carries no choices
        text += chunk.choices[0].delta.content or ""
        finish = chunk.choices[0].finish_reason or finish
    assert text == EXPECTED_STREAM_TEXT
    assert finish == "stop"


def test_models_list_names_the_model(
    gateway_url: str, gateway_credential: str, model: str
) -> None:
    page = _client(gateway_url, gateway_credential).models.list()
    ids = [m.id for m in page.data]
    assert model in ids


def test_error_envelope_carries_a_code(
    gateway_url: str, gateway_credential: str, question: Turn
) -> None:
    with pytest.raises(openai.BadRequestError) as info:
        _client(gateway_url, gateway_credential).chat.completions.create(
            model="no-such-model", messages=question
        )
    assert info.value.status_code == 400
    assert info.value.code == "model_not_found"
    assert info.value.type == "invalid_request_error"


def test_wrong_credential_is_401(
    gateway_url: str, gateway_credential: str, model: str, question: Turn
) -> None:
    with pytest.raises(openai.AuthenticationError) as info:
        _client(gateway_url, "not-" + gateway_credential).chat.completions.create(
            model=model, messages=question
        )
    assert info.value.status_code == 401
