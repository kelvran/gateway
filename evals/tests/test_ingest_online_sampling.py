"""CLI integration tests for `evals ingest`'s `--sample-rate`/
`--sample-seed`/`--filter-rules` options — a separate file from
test_ingest_integration.py (already focused on the pre-existing decode/
--suite/--results behavior), mirroring that file's own precedent for
splitting by concern.

Object storage is monkeypatched exactly like test_ingest_integration.py
does — real synthetic JSON lines constructed inline here (not the
shared gatewayevents_object_storage_sample.jsonl fixture), since these
tests need specific `outcome` values (a failure outcome vs. a success
one) that fixture doesn't provide.
"""

from __future__ import annotations

import json
from pathlib import Path

from click.testing import CliRunner

import evals.cli as cli_module
from evals.cli import main


def _event_line(trace_id: str, outcome: str) -> str:
    return json.dumps({"traceId": trace_id, "outcome": outcome})


def _run_ingest(out_path: Path, monkeypatch, lines: list[str], extra_args: list[str]):
    monkeypatch.setattr(
        cli_module, "list_object_keys", lambda scheme, bucket, prefix: ["sample.jsonl"]
    )
    monkeypatch.setattr(
        cli_module, "iter_object_lines", lambda scheme, bucket, key: iter(lines)
    )

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "ingest",
            "--source",
            "s3://my-bucket/gatewayevents/v1/",
            "--out",
            str(out_path),
            *extra_args,
        ],
    )
    return result, out_path


def test_ingest_default_sample_rate_ingests_everything_unchanged(tmp_path, monkeypatch):
    """--sample-rate defaults to 1.0 -- omitting it must reproduce this
    command's original behavior byte-for-byte, the same regression proof
    every other additive flag in this codebase (e.g. --llm-judge-provider)
    already carries.
    """
    lines = [
        _event_line("trace-1", "OUTCOME_OK"),
        _event_line("trace-2", "OUTCOME_UPSTREAM_ERROR"),
    ]
    result, out_path = _run_ingest(tmp_path / "ingested.jsonl", monkeypatch, lines, [])

    assert result.exit_code == 0, result.output
    assert "2 decoded" in result.output
    assert "2 sampled in" in result.output
    decoded = [json.loads(line) for line in out_path.read_text().splitlines() if line]
    assert len(decoded) == 2


def test_ingest_sample_rate_zero_decodes_but_ingests_nothing(tmp_path, monkeypatch):
    lines = [_event_line("trace-1", "OUTCOME_OK") for _ in range(5)]
    result, out_path = _run_ingest(
        tmp_path / "ingested.jsonl", monkeypatch, lines, ["--sample-rate", "0.0"]
    )

    assert result.exit_code == 0, result.output
    assert "5 decoded" in result.output
    assert "0 sampled in" in result.output
    assert out_path.read_text() == ""


def test_ingest_sample_rate_with_seed_is_reproducible_across_two_runs(
    tmp_path, monkeypatch
):
    """The real property --sample-seed exists for: two independent
    `evals ingest` invocations against the SAME input, with the SAME
    --sample-seed, must select the identical subset of events -- a
    reproducible dry run, not a one-off random result.
    """
    lines = [_event_line(f"trace-{i}", "OUTCOME_OK") for i in range(200)]

    result_a, out_path_a = _run_ingest(
        tmp_path / "ingested-a.jsonl",
        monkeypatch,
        lines,
        ["--sample-rate", "0.3", "--sample-seed", "99"],
    )
    result_b, out_path_b = _run_ingest(
        tmp_path / "ingested-b.jsonl",
        monkeypatch,
        lines,
        ["--sample-rate", "0.3", "--sample-seed", "99"],
    )

    assert result_a.exit_code == 0, result_a.output
    assert result_b.exit_code == 0, result_b.output
    assert out_path_a.read_text() == out_path_b.read_text()
    # Not every line, and not zero -- a genuine, non-trivial subset.
    sampled_a = [line for line in out_path_a.read_text().splitlines() if line]
    assert 0 < len(sampled_a) < len(lines)


def test_ingest_filter_rules_excludes_a_non_matching_outcome(tmp_path, monkeypatch):
    lines = [_event_line("trace-1", "OUTCOME_OK")]
    result, out_path = _run_ingest(
        tmp_path / "ingested.jsonl", monkeypatch, lines, ["--filter-rules"]
    )

    assert result.exit_code == 0, result.output
    assert "1 decoded" in result.output
    assert "0 sampled in" in result.output
    assert out_path.read_text() == ""


def test_ingest_filter_rules_includes_a_matching_failure_outcome(tmp_path, monkeypatch):
    lines = [_event_line("trace-1", "OUTCOME_UPSTREAM_ERROR")]
    result, out_path = _run_ingest(
        tmp_path / "ingested.jsonl", monkeypatch, lines, ["--filter-rules"]
    )

    assert result.exit_code == 0, result.output
    assert "1 sampled in" in result.output
    decoded = [json.loads(line) for line in out_path.read_text().splitlines() if line]
    assert len(decoded) == 1
    assert decoded[0]["traceId"] == "trace-1"


def test_ingest_filter_rules_is_anded_with_sample_rate_not_a_substitute(
    tmp_path, monkeypatch
):
    """The real gap this guards: a rule-matching (failure-outcome) event
    must STILL be excluded by --sample-rate 0.0 -- --filter-rules narrows
    which events are ELIGIBLE, it never overrides a sample-rate rejection.
    """
    lines = [_event_line("trace-1", "OUTCOME_UPSTREAM_ERROR")]
    result, out_path = _run_ingest(
        tmp_path / "ingested.jsonl",
        monkeypatch,
        lines,
        ["--filter-rules", "--sample-rate", "0.0"],
    )

    assert result.exit_code == 0, result.output
    assert "0 sampled in" in result.output
    assert out_path.read_text() == ""


def test_ingest_without_filter_rules_ingests_a_non_matching_outcome_anyway(
    tmp_path, monkeypatch
):
    """The default (--filter-rules omitted) applies NO rule constraint at
    all -- an OUTCOME_OK event, which DEFAULT_RULE_FILTERS would reject,
    must still be ingested, matching this command's pre-existing
    behavior exactly.
    """
    lines = [_event_line("trace-1", "OUTCOME_OK")]
    result, out_path = _run_ingest(tmp_path / "ingested.jsonl", monkeypatch, lines, [])

    assert result.exit_code == 0, result.output
    assert "1 sampled in" in result.output
    decoded = [json.loads(line) for line in out_path.read_text().splitlines() if line]
    assert len(decoded) == 1
