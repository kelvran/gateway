"""CLI integration tests for `evals ingest`, per docs/rfcs/2026-09-07-
evals-trace-ingestion-object-storage.md — a separate file from
test_cli_integration.py (already at its own line-count cap) rather than
growing that file further, mirroring test_promote_integration.py's own
precedent for the same reason.

Object storage itself is never touched: `evals.ingestion.object_store`'s
`list_object_keys`/`iter_object_lines` are monkeypatched on `cli_module`
directly (the same pattern test_cli_integration.py already uses for
`scheduler_module.run_in_sandbox`/`make_anthropic_call_model`), fed from
`tests/fixtures/gatewayevents_object_storage_sample.jsonl` — a synthetic
fixture in exactly the shape the real Vector shipper
(docs/operations/vector-gatewayevents-s3.yaml) writes to S3: one decoded
GatewayDecisionEvent JSON object per line, per that RFC's §2 layout,
plus one deliberately malformed line to prove a single bad line never
aborts the whole ingest run.
"""

from __future__ import annotations

import json
from pathlib import Path

from click.testing import CliRunner

import evals.cli as cli_module
from evals.cli import main

_FIXTURE_PATH = Path("tests/fixtures/gatewayevents_object_storage_sample.jsonl")


def _fixture_lines() -> list[str]:
    return [line for line in _FIXTURE_PATH.read_text().splitlines() if line.strip()]


def test_ingest_decodes_a_synthetic_object_storage_fixture_end_to_end(
    tmp_path, monkeypatch
):
    lines = _fixture_lines()
    assert len(lines) == 3  # 2 decodable events + 1 malformed line

    monkeypatch.setattr(
        cli_module, "list_object_keys", lambda scheme, bucket, prefix: ["sample.jsonl"]
    )
    monkeypatch.setattr(
        cli_module,
        "iter_object_lines",
        lambda scheme, bucket, key: iter(lines),
    )

    out_path = tmp_path / "ingested.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "ingest",
            "--source",
            "s3://my-bucket/gatewayevents/v1/",
            "--out",
            str(out_path),
        ],
    )

    assert result.exit_code == 0, result.output
    assert "ingested 1 object(s)" in result.output
    assert "2 decoded" in result.output
    assert "1 failed to decode" in result.output

    decoded = [json.loads(line) for line in out_path.read_text().splitlines() if line]
    assert len(decoded) == 2
    assert decoded[0]["traceId"] == "4bf92f3577b34da6a3ce929d0e0e4736"
    assert decoded[0]["virtualKeyId"] == "team-fixture"
    assert decoded[0]["outcome"] == "OUTCOME_OK"
    assert decoded[1]["traceId"] == "5cf92f3577b34da6a3ce929d0e0e4737"
    assert decoded[1]["virtualKeyId"] == "_unauthenticated"
    assert decoded[1]["outcome"] == "OUTCOME_AUTH_FAILED"


def test_ingest_decodes_a_synthetic_object_storage_fixture_end_to_end_gcs(
    tmp_path, monkeypatch
):
    """Same fixture/assertions as the s3:// test above, driven through a
    gs:// source instead -- proves `ingest_cmd` genuinely threads the
    scheme parsed out of `--source` through to `list_object_keys`/
    `iter_object_lines`, not just the s3:// path.
    """
    lines = _fixture_lines()
    assert len(lines) == 3  # 2 decodable events + 1 malformed line

    seen_schemes: list[str] = []

    def _fake_list_object_keys(scheme: str, bucket: str, prefix: str) -> list[str]:
        seen_schemes.append(scheme)
        return ["sample.jsonl"]

    monkeypatch.setattr(cli_module, "list_object_keys", _fake_list_object_keys)
    monkeypatch.setattr(
        cli_module,
        "iter_object_lines",
        lambda scheme, bucket, key: iter(lines),
    )

    out_path = tmp_path / "ingested.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "ingest",
            "--source",
            "gs://my-bucket/gatewayevents/v1/",
            "--out",
            str(out_path),
        ],
    )

    assert result.exit_code == 0, result.output
    assert seen_schemes == ["gs"]
    assert "ingested 1 object(s)" in result.output
    assert "2 decoded" in result.output
    assert "1 failed to decode" in result.output

    decoded = [json.loads(line) for line in out_path.read_text().splitlines() if line]
    assert len(decoded) == 2
    assert decoded[0]["traceId"] == "4bf92f3577b34da6a3ce929d0e0e4736"
    assert decoded[1]["traceId"] == "5cf92f3577b34da6a3ce929d0e0e4737"


def test_ingest_lists_multiple_objects_and_aggregates_counts_across_all_of_them(
    tmp_path, monkeypatch
):
    lines = _fixture_lines()
    monkeypatch.setattr(
        cli_module,
        "list_object_keys",
        lambda scheme, bucket, prefix: ["obj-1.jsonl", "obj-2.jsonl"],
    )
    monkeypatch.setattr(
        cli_module,
        "iter_object_lines",
        lambda scheme, bucket, key: iter(lines),
    )

    out_path = tmp_path / "ingested.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "ingest",
            "--source",
            "s3://my-bucket/gatewayevents/v1/",
            "--out",
            str(out_path),
        ],
    )

    assert result.exit_code == 0, result.output
    assert "ingested 2 object(s)" in result.output
    # Each of the 2 objects contributes the same 2 decodable + 1 malformed
    # lines -- counts must aggregate across every listed object, not just
    # the first one.
    assert "4 decoded" in result.output
    assert "2 failed to decode" in result.output

    decoded = [json.loads(line) for line in out_path.read_text().splitlines() if line]
    assert len(decoded) == 4


def test_ingest_with_no_objects_found_fails_with_nonzero_exit(tmp_path, monkeypatch):
    monkeypatch.setattr(
        cli_module, "list_object_keys", lambda scheme, bucket, prefix: []
    )

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "ingest",
            "--source",
            "s3://my-bucket/empty-prefix/",
            "--out",
            str(tmp_path / "ingested.jsonl"),
        ],
    )

    assert result.exit_code != 0
    assert "no objects found" in result.output


def test_ingest_rejects_an_unsupported_source_scheme(tmp_path):
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "ingest",
            "--source",
            "ftp://my-bucket/gatewayevents/v1/",
            "--out",
            str(tmp_path / "ingested.jsonl"),
        ],
    )

    assert result.exit_code != 0
    assert "unsupported object-storage scheme" in result.output


def test_ingest_never_reimplements_decode_it_calls_the_real_decode_function(
    tmp_path, monkeypatch
):
    """Sanity-check-by-breaking companion: proves `ingest_cmd` genuinely
    calls `evals.ingestion.decode.decode_gateway_decision_event` rather
    than some inline reimplementation, by making the real decode function
    itself raise and confirming every line is then counted as a decode
    failure.
    """

    def _always_fails(raw: str):
        raise ValueError("simulated decode failure")

    monkeypatch.setattr(cli_module, "decode_gateway_decision_event", _always_fails)
    monkeypatch.setattr(
        cli_module, "list_object_keys", lambda scheme, bucket, prefix: ["sample.jsonl"]
    )
    monkeypatch.setattr(
        cli_module,
        "iter_object_lines",
        lambda scheme, bucket, key: iter(_fixture_lines()),
    )

    out_path = tmp_path / "ingested.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "ingest",
            "--source",
            "s3://my-bucket/gatewayevents/v1/",
            "--out",
            str(out_path),
        ],
    )

    assert result.exit_code == 0, result.output
    assert "0 decoded" in result.output
    assert "3 failed to decode" in result.output
    assert out_path.read_text() == ""
