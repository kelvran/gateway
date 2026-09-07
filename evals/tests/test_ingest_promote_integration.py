"""End-to-end CLI integration tests proving `evals ingest`'s output really
feeds `evals promote` — the exact plumbing
docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md's own
"Unresolved Questions" section left open ("What happens downstream of
`evals ingest`'s output file — feeding `evals promote` unchanged, or a
distinct review path for live-sampled data"), now resolved per
DECISIONS.md: no separate review path.

A separate file from both test_ingest_integration.py and
test_promote_integration.py (each already at their own line-count cap),
per this project's own "many small files" convention — this file's own
job is specifically the *seam* between the two commands, not either
command in isolation.

Reuses `tests/fixtures/gatewayevents_object_storage_sample.jsonl` exactly
as `test_ingest_integration.py` does: object storage itself is never
touched, `evals.ingestion.object_store`'s `list_object_keys`/
`iter_object_lines` are monkeypatched on `cli_module` directly.
"""

from __future__ import annotations

import json
from pathlib import Path

from click.testing import CliRunner

import evals.cli as cli_module
from evals.cli import main

_FIXTURE_PATH = Path("tests/fixtures/gatewayevents_object_storage_sample.jsonl")

_OK_TRACE_ID = "4bf92f3577b34da6a3ce929d0e0e4736"
_OK_SPAN_ID = "00f067aa0ba902b7"
_OK_RUN_ID = f"gatewayevents-{_OK_TRACE_ID}-{_OK_SPAN_ID}"

_AUTH_FAILED_TRACE_ID = "5cf92f3577b34da6a3ce929d0e0e4737"
_AUTH_FAILED_SPAN_ID = "11f067aa0ba902b8"
_AUTH_FAILED_RUN_ID = f"gatewayevents-{_AUTH_FAILED_TRACE_ID}-{_AUTH_FAILED_SPAN_ID}"


def _fixture_lines() -> list[str]:
    return [line for line in _FIXTURE_PATH.read_text().splitlines() if line.strip()]


def _run_ingest(tmp_path: Path, monkeypatch, extra_args: list[str]) -> tuple[str, Path]:
    lines = _fixture_lines()
    monkeypatch.setattr(
        cli_module, "list_object_keys", lambda scheme, bucket, prefix: ["sample.jsonl"]
    )
    monkeypatch.setattr(
        cli_module, "iter_object_lines", lambda scheme, bucket, key: iter(lines)
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
            *extra_args,
        ],
    )
    return result.output, out_path


def test_ingest_with_suite_and_results_produces_promotable_cases_and_runs(
    tmp_path, monkeypatch
):
    suite_path = tmp_path / "suite.json"
    results_path = tmp_path / "runs.jsonl"

    output, out_path = _run_ingest(
        tmp_path,
        monkeypatch,
        ["--suite", str(suite_path), "--results", str(results_path)],
    )

    assert "2 decoded" in output
    assert "1 failed to decode" in output
    assert "promotable: 2 EvalCase(s)" in output
    assert "2 Run(s)" in output
    # --out is still written exactly as before -- additive, not replaced.
    assert len(out_path.read_text().splitlines()) == 2

    cases = json.loads(suite_path.read_text())
    assert len(cases) == 2
    assert {c["id"] for c in cases} == {_OK_RUN_ID, _AUTH_FAILED_RUN_ID}
    for case in cases:
        assert case["tier"] == "drift_sample"
        assert case["revision"] == 1
        assert case["task_spec"]["source"] == "gatewayevents_v1"

    ok_case = next(c for c in cases if c["id"] == _OK_RUN_ID)
    assert ok_case["task_spec"]["outcome"] == "OUTCOME_OK"
    assert ok_case["task_spec"]["virtual_key_id"] == "team-fixture"
    assert ok_case["task_spec"]["requested_model"] == "gpt-4o"
    assert ok_case["reference"] is None

    auth_failed_case = next(c for c in cases if c["id"] == _AUTH_FAILED_RUN_ID)
    assert auth_failed_case["task_spec"]["outcome"] == "OUTCOME_AUTH_FAILED"
    assert auth_failed_case["task_spec"]["virtual_key_id"] == "_unauthenticated"

    runs = [json.loads(line) for line in results_path.read_text().splitlines() if line]
    assert len(runs) == 2
    ok_run = next(r for r in runs if r["id"] == _OK_RUN_ID)
    assert ok_run["status"] == "completed"
    assert ok_run["eval_case_id"] == _OK_RUN_ID
    assert ok_run["eval_case_revision"] == 1
    assert ok_run["stdout"] == ""
    assert ok_run["error"] is None

    auth_failed_run = next(r for r in runs if r["id"] == _AUTH_FAILED_RUN_ID)
    assert auth_failed_run["status"] == "error"
    assert auth_failed_run["error"] == "gateway outcome: OUTCOME_AUTH_FAILED"


def test_ingest_without_suite_or_results_writes_only_out_exactly_as_before(
    tmp_path, monkeypatch
):
    """Sanity check that the new flags are genuinely additive: omitting
    both reproduces the pre-existing decode-only behavior, with no suite
    or results file ever created.
    """
    output, out_path = _run_ingest(tmp_path, monkeypatch, [])

    assert "2 decoded" in output
    assert "promotable:" not in output
    assert len(out_path.read_text().splitlines()) == 2
    assert not (tmp_path / "suite.json").exists()
    assert not (tmp_path / "runs.jsonl").exists()


def test_ingest_requires_suite_and_results_together(tmp_path, monkeypatch):
    output, _ = _run_ingest(
        tmp_path, monkeypatch, ["--suite", str(tmp_path / "suite.json")]
    )
    assert "--suite and --results must be given together" in output


def test_ingested_run_is_promotable_to_drift_sample_tier(tmp_path, monkeypatch):
    """The real end-to-end proof: ingest against the synthetic
    object-storage fixture, then promote the resulting Run — confirming
    `evals promote` needs zero special-casing for a live-sampled-data
    source, per the project owner's "no separate review path" decision.
    """
    suite_path = tmp_path / "suite.json"
    results_path = tmp_path / "runs.jsonl"
    _run_ingest(
        tmp_path,
        monkeypatch,
        ["--suite", str(suite_path), "--results", str(results_path)],
    )

    promoted_path = tmp_path / "promoted.json"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "promote",
            "--suite",
            str(suite_path),
            "--results",
            str(results_path),
            "--run-id",
            _OK_RUN_ID,
            "--tier",
            "drift_sample",
            "--output",
            str(promoted_path),
        ],
    )

    assert result.exit_code == 0, result.output
    promoted = json.loads(promoted_path.read_text())
    assert len(promoted) == 1
    new_case = promoted[0]
    assert new_case["id"] == f"{_OK_RUN_ID}-promoted-{_OK_RUN_ID}"
    assert new_case["revision"] == 1
    assert new_case["tier"] == "drift_sample"
    assert new_case["task_spec"]["source"] == "gatewayevents_v1"
    assert new_case["task_spec"]["outcome"] == "OUTCOME_OK"
    assert new_case["reference"] is None
    assert f"promoted-from:{_OK_RUN_ID}@1" in new_case["tags"]
    assert f"promoted-from-run:{_OK_RUN_ID}" in new_case["tags"]


def test_ingested_run_at_regression_tier_still_enforces_failing_score_precondition(
    tmp_path, monkeypatch
):
    """Proves `--tier regression`'s existing precondition (a real, on-file
    failing Score) is enforced identically for an ingested Run as for
    any other source — "no separate review path" means no special-case
    exemption either way. An ingested Run has no Score at all (a
    GatewayDecisionEvent carries no judge verdict to honestly produce
    one from), so a --scores file that's on-disk but empty of matches
    for this run-id must fail the exact same "nothing to
    regression-test" way test_promote_integration.py's own
    test_promote_regression_tier_requires_a_failing_score already proves
    for a rollout-sourced Run.
    """
    suite_path = tmp_path / "suite.json"
    results_path = tmp_path / "runs.jsonl"
    _run_ingest(
        tmp_path,
        monkeypatch,
        ["--suite", str(suite_path), "--results", str(results_path)],
    )

    scores_path = tmp_path / "scores.jsonl"
    scores_path.write_text("")  # on-disk, real, but no Score for this run

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "promote",
            "--suite",
            str(suite_path),
            "--results",
            str(results_path),
            "--scores",
            str(scores_path),
            "--run-id",
            _OK_RUN_ID,
            "--tier",
            "regression",
            "--output",
            str(tmp_path / "promoted.json"),
        ],
    )

    assert result.exit_code != 0
    assert "nothing to regression-test" in result.output
    assert not (tmp_path / "promoted.json").exists()
