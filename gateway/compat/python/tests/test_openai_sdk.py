"""The official `openai` package against the gateway's OpenAI-compatible routes,
served by an anthropic deployment through the translate path."""

import json

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


WEATHER_TOOL = {
    "type": "function",
    "function": {
        "name": "get_weather",
        "description": "Get the current weather for a city",
        "parameters": {
            "type": "object",
            "properties": {"city": {"type": "string"}},
            "required": ["city"],
        },
    },
}
# the tool_use id the streamed tool golden (anthropic_stream_thinking_tool.txt) introduces
STREAMED_TOOL_USE_ID = "toolu_01WeatherThink000000"


def test_stream_helper_reassembles_a_tool_call(
    gateway_url: str, gateway_credential: str, model: str, question: Turn
) -> None:
    """The SDK's stream helper accumulates `delta.tool_calls[].function.*` and
    never the flat members; since gateway/v0.20.0 the gateway writes OpenAI's
    nesting beside the deprecated flat `name`/`arguments_json`, so the final
    completion carries the whole call."""
    with _client(gateway_url, gateway_credential).chat.completions.stream(
        model=model, messages=question, tools=[WEATHER_TOOL]
    ) as stream:
        for _ in stream:
            pass
        final = stream.get_final_completion()
    choice = final.choices[0]
    assert choice.finish_reason == "tool_calls"
    assert choice.message.tool_calls is not None
    assert len(choice.message.tool_calls) == 1
    call = choice.message.tool_calls[0]
    assert call.id == STREAMED_TOOL_USE_ID
    assert call.type == "function"
    assert call.function.name == "get_weather"
    assert json.loads(call.function.arguments) == {"city": "Boston"}


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
