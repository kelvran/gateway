"""CLI integration tests for `evals cost-report`, per
docs/rfcs/2026-09-12-gateway-cost-attribution-aggregation.md — a separate
file from test_cli_integration.py (already at its own line-count cap),
mirroring test_ingest_integration.py's own precedent for the same reason.

Object storage itself is never touched: `evals.ingestion.object_store`'s
`list_object_keys`/`iter_object_lines` are monkeypatched on `cli_module`
directly, fed from a small synthetic fixture (inline, not a checked-in
file — three decodable events spanning two distinct agent_run_ids, plus
one deliberately malformed line) proving the command sums `cost_usd`
scoped to exactly the requested `agent_run_id`, not a blended total.
"""

from __future__ import annotations

from click.testing import CliRunner

import evals.cli as cli_module
from evals.cli import main

_FIXTURE_LINES = [
    '{"agentRunId": "run-abc", "costUsd": "0.05"}',
    '{"agentRunId": "run-abc", "costUsd": "0.10"}',
    '{"agentRunId": "run-xyz", "costUsd": "1.00"}',
    "not valid json",
]


def test_cost_report_sums_cost_usd_scoped_to_the_requested_agent_run_id(monkeypatch):
    monkeypatch.setattr(
        cli_module, "list_object_keys", lambda scheme, bucket, prefix: ["sample.jsonl"]
    )
    monkeypatch.setattr(
        cli_module,
        "iter_object_lines",
        lambda scheme, bucket, key: iter(_FIXTURE_LINES),
    )

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "cost-report",
            "--source",
            "s3://my-bucket/gatewayevents/v1/",
            "--agent-run-id",
            "run-abc",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "agent_run_id=run-abc: cost_usd=0.15" in result.output
    assert "2 matching event(s)" in result.output
    assert "1 failed to decode" in result.output


def test_cost_report_a_different_agent_run_id_gets_its_own_sum_not_the_blended_total(
    monkeypatch,
):
    monkeypatch.setattr(
        cli_module, "list_object_keys", lambda scheme, bucket, prefix: ["sample.jsonl"]
    )
    monkeypatch.setattr(
        cli_module,
        "iter_object_lines",
        lambda scheme, bucket, key: iter(_FIXTURE_LINES),
    )

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "cost-report",
            "--source",
            "s3://my-bucket/gatewayevents/v1/",
            "--agent-run-id",
            "run-xyz",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "agent_run_id=run-xyz: cost_usd=1.00" in result.output
    assert "1 matching event(s)" in result.output
