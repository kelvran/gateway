"""The official `anthropic` package against the gateway's /v1/messages routes.

Every expected value is a byte of the recorded golden the Go harness's mock
replays (gateway/compat/testdata/): an anthropic deployment relays Anthropic's
own response, so the SDK must see Anthropic's own id, model and usage.
"""

import anthropic
import pytest

EXPECTED_TEXT = "It's 72°F and sunny in Boston."
EXPECTED_STREAM_TEXT = "The sky is blue today."

Turn = list[dict[str, str]]


def _client(url: str, credential: str) -> anthropic.Anthropic:
    return anthropic.Anthropic(base_url=url, api_key=credential, max_retries=0)


def test_buffered_message_is_anthropics_own_bytes(
    gateway_url: str, gateway_credential: str, model: str, question: Turn
) -> None:
    msg = _client(gateway_url, gateway_credential).messages.create(
        model=model, max_tokens=64, messages=question
    )
    assert msg.id == "msg_01XFDUDYJgAACzvnptvVoYEL"
    assert msg.model == "claude-opus-4-20250514"
    assert msg.content[0].type == "text"
    assert msg.content[0].text == EXPECTED_TEXT
    assert msg.stop_reason == "end_turn"
    assert (msg.usage.input_tokens, msg.usage.output_tokens) == (58, 12)


def test_stream_accumulates_the_relayed_frames(
    gateway_url: str, gateway_credential: str, model: str, question: Turn
) -> None:
    with _client(gateway_url, gateway_credential).messages.stream(
        model=model, max_tokens=64, messages=question
    ) as stream:
        final = stream.get_final_message()
    text = "".join(block.text for block in final.content if block.type == "text")
    assert text == EXPECTED_STREAM_TEXT
    assert final.stop_reason == "end_turn"
    assert final.usage.output_tokens == 9


def test_count_tokens_is_the_deployments_own_count(
    gateway_url: str, gateway_credential: str, model: str, question: Turn
) -> None:
    count = _client(gateway_url, gateway_credential).messages.count_tokens(
        model=model, messages=question
    )
    assert count.input_tokens == 2095


def test_models_list_names_the_model(
    gateway_url: str, gateway_credential: str, model: str
) -> None:
    page = _client(gateway_url, gateway_credential).models.list()
    ids = [m.id for m in page.data]
    assert model in ids
    assert all(m.type == "model" for m in page.data)


def test_error_envelope_carries_a_code(
    gateway_url: str, gateway_credential: str, question: Turn
) -> None:
    with pytest.raises(anthropic.BadRequestError) as info:
        _client(gateway_url, gateway_credential).messages.create(
            model="no-such-model", max_tokens=64, messages=question
        )
    assert info.value.status_code == 400
    body = info.value.body
    assert isinstance(body, dict)
    assert body["type"] == "error"
    assert body["error"]["type"] == "invalid_request_error"
    assert body["error"]["code"] == "model_not_found"


def test_bearer_credential_form_is_accepted(
    gateway_url: str, gateway_credential: str, model: str, question: Turn
) -> None:
    client = anthropic.Anthropic(
        base_url=gateway_url, auth_token=gateway_credential, max_retries=0
    )
    msg = client.messages.create(model=model, max_tokens=64, messages=question)
    assert msg.content[0].text == EXPECTED_TEXT


def test_wrong_credential_is_401(
    gateway_url: str, gateway_credential: str, model: str, question: Turn
) -> None:
    with pytest.raises(anthropic.AuthenticationError) as info:
        _client(gateway_url, "not-" + gateway_credential).messages.create(
            model=model, max_tokens=64, messages=question
        )
    assert info.value.status_code == 401
