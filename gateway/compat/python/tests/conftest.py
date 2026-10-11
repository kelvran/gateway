"""Fixtures for the compatibility matrix's Python leg.

The leg runs only against a live gateway, which the Go harness starts
(harness_test.go's TestMain) and python_leg_test.go names through the
environment.
Without that it FAILS -- never skips -- so the merge gate cannot go green on a
run that exercised nothing.
"""

import os

import pytest

URL_ENV = "KELVRAN_COMPAT_URL"
CREDENTIAL_ENV = "KELVRAN_COMPAT_CREDENTIAL"
MODEL_ENV = "KELVRAN_COMPAT_MODEL"


def _required(name: str) -> str:
    value = os.environ.get(name, "")
    if not value:
        pytest.fail(
            f"{name} is not set: the compat leg runs only against a live gateway "
            "and fails rather than skips, so the merge gate bites",
            pytrace=False,
        )
    return value


@pytest.fixture(scope="session")
def gateway_url() -> str:
    return _required(URL_ENV).rstrip("/")


@pytest.fixture(scope="session")
def gateway_credential() -> str:
    return _required(CREDENTIAL_ENV)


@pytest.fixture(scope="session")
def model() -> str:
    """The canonical model served by the anthropic deployment (passthrough)."""
    return os.environ.get(MODEL_ENV, "claude-sys")


@pytest.fixture
def question(request: pytest.FixtureRequest) -> list[dict[str, str]]:
    """One user turn, distinct per test: the gateway's response cache is on, as
    it is for every operator, and an identical body would be answered from the
    cache (re-encoded) instead of relayed from the mock upstream."""
    text = f"What's the weather in Boston? [{request.node.name}]"
    return [{"role": "user", "content": text}]
