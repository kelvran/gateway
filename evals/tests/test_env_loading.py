"""Tests for `evals.cli`'s local-dev `.env` auto-loading (`_load_env_file`).

Covers the property that actually matters here: a real, already-exported
process env var (a CI secret, an explicit `export`) always wins over
whatever `evals/.env` sets — `load_dotenv`'s `override=False` default is
relied on deliberately, not just accepted. Also covers the "file doesn't
exist" case, since the file is a local-dev convenience, never a
requirement (CI and production never have one).
"""

from __future__ import annotations

import os

import evals.cli as cli_module

_TEST_VAR = "EVALS_TEST_ENV_LOADING_VAR"


def test_load_env_file_populates_an_unset_variable_from_the_file(tmp_path, monkeypatch):
    monkeypatch.delenv(_TEST_VAR, raising=False)
    env_path = tmp_path / ".env"
    env_path.write_text(f"{_TEST_VAR}=from-file\n")

    cli_module._load_env_file(env_path)

    assert os.environ[_TEST_VAR] == "from-file"


def test_load_env_file_never_overrides_an_already_set_variable(tmp_path, monkeypatch):
    monkeypatch.setenv(_TEST_VAR, "from-real-env")
    env_path = tmp_path / ".env"
    env_path.write_text(f"{_TEST_VAR}=from-file\n")

    cli_module._load_env_file(env_path)

    assert os.environ[_TEST_VAR] == "from-real-env"


def test_load_env_file_is_a_noop_when_the_file_does_not_exist(tmp_path, monkeypatch):
    monkeypatch.delenv(_TEST_VAR, raising=False)
    missing_path = tmp_path / "does-not-exist.env"

    cli_module._load_env_file(missing_path)

    assert _TEST_VAR not in os.environ


def test_main_group_callback_loads_the_env_file(monkeypatch):
    calls = []
    monkeypatch.setattr(cli_module, "_load_env_file", lambda: calls.append(True))

    cli_module.main.callback()

    assert calls == [True]
