"""Unit tests for `evals.corpus_staleness`'s citation parser (including the
carried-over-filename shorthand a naive per-citation regex would silently
mis-parse) and a real, hermetic-git-repo integration test proving the
staleness comparison itself works end-to-end -- never against the live
Kelvran repo's own evolving history, which would make dates non-
reproducible across runs.
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

from click.testing import CliRunner

from evals.cli import main
from evals.corpus_staleness import Citation, check_case_staleness, parse_citations
from evals.models import EvalCase

_FIXTURE_CASE_JSON = """[
  {
    "id": "case-1",
    "revision": 1,
    "task_spec": {
      "output": "a",
      "verified_against": "gateway/internal/foo.go:1"
    },
    "reference": "a",
    "tier": "regression"
  }
]
"""


def test_parse_citations_single_file_single_line():
    got = parse_citations("gateway/internal/foo.go:42")
    assert got == [Citation(file="gateway/internal/foo.go", start_line=42, end_line=42)]


def test_parse_citations_single_file_line_range():
    got = parse_citations("gateway/internal/foo.go:42-58")
    assert got == [Citation(file="gateway/internal/foo.go", start_line=42, end_line=58)]


def test_parse_citations_carried_over_filename_shorthand():
    # The real shorthand this module exists to handle correctly: after the
    # first full-path citation, subsequent ":START-END" citations in the
    # same sentence omit the filename entirely and reuse it.
    got = parse_citations(
        "gateway/internal/costaccounting/costaccounting.go:18-28 (Usage struct), "
        ":38-43 (ModelPrice struct), :69-80 (Calculator.Calculate)"
    )
    assert got == [
        Citation(
            file="gateway/internal/costaccounting/costaccounting.go",
            start_line=18,
            end_line=28,
        ),
        Citation(
            file="gateway/internal/costaccounting/costaccounting.go",
            start_line=38,
            end_line=43,
        ),
        Citation(
            file="gateway/internal/costaccounting/costaccounting.go",
            start_line=69,
            end_line=80,
        ),
    ]


def test_parse_citations_carry_over_resets_on_new_full_path():
    got = parse_citations("a/foo.go:1-2 (x), :3-4 (y); b/bar.go:5-6 (z), :7-8 (w)")
    assert got == [
        Citation(file="a/foo.go", start_line=1, end_line=2),
        Citation(file="a/foo.go", start_line=3, end_line=4),
        Citation(file="b/bar.go", start_line=5, end_line=6),
        Citation(file="b/bar.go", start_line=7, end_line=8),
    ]


def test_parse_citations_bare_basename_resolves_against_prior_full_path():
    # A restated bare filename (no directory), distinct from the
    # colon-only carry-over shorthand -- also real, observed in this
    # corpus (costabuse-fallback-response-model-billing-mismatch).
    got = parse_citations(
        "gateway/internal/gateway/dataplane/dataplane.go:1300-1306 (x); "
        "dataplane.go:1007-1013 (y)."
    )
    assert got == [
        Citation(
            file="gateway/internal/gateway/dataplane/dataplane.go",
            start_line=1300,
            end_line=1306,
        ),
        Citation(
            file="gateway/internal/gateway/dataplane/dataplane.go",
            start_line=1007,
            end_line=1013,
        ),
    ]


def test_parse_citations_unresolvable_bare_filename_is_skipped():
    # No prior full path at all -- "foo.go:1-2" has no directory and
    # nothing to resolve it against, so it must be silently dropped, not
    # guessed.
    got = parse_citations("foo.go:1-2 (x)")
    assert got == []


def test_parse_citations_bare_filename_mismatched_basename_is_skipped():
    # A bare filename whose basename does NOT match the most recently
    # seen full path must never be resolved against it anyway -- proves
    # the basename check is load-bearing, not just a no-op fallback to
    # "whatever the last full path was". A weaker implementation that
    # blindly carries over last_full_path for ANY bare filename would
    # wrongly resolve "other.go:9" to "a/foo.go" here.
    got = parse_citations("a/foo.go:1-2 (x); other.go:9 (y)")
    assert got == [Citation(file="a/foo.go", start_line=1, end_line=2)]


def test_parse_citations_no_line_number_is_skipped():
    got = parse_citations(
        "repo-wide grep for velocity/anomaly/baseline across gateway/internal/ "
        "(zero matches)"
    )
    assert got == []


def test_parse_citations_empty_string():
    assert parse_citations("") == []


def _run_git(repo: Path, *args: str, when: str) -> None:
    env = {
        **os.environ,
        "GIT_AUTHOR_DATE": when,
        "GIT_COMMITTER_DATE": when,
    }
    subprocess.run(["git", *args], cwd=repo, check=True, env=env, capture_output=True)


def _init_repo(repo: Path) -> None:
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
    subprocess.run(
        ["git", "config", "user.email", "test@example.com"], cwd=repo, check=True
    )
    subprocess.run(["git", "config", "user.name", "Test"], cwd=repo, check=True)


def test_check_case_staleness_flags_a_cited_file_touched_after_the_fixture(tmp_path):
    repo = tmp_path
    _init_repo(repo)

    cited = repo / "gateway" / "internal" / "foo.go"
    cited.parent.mkdir(parents=True)
    cited.write_text("package foo\n")
    _run_git(repo, "add", "gateway/internal/foo.go", when="2026-01-01T00:00:00")
    _run_git(repo, "commit", "-q", "-m", "add foo.go", when="2026-01-01T00:00:00")

    fixture_dir = repo / "evals" / "tests" / "fixtures"
    fixture_dir.mkdir(parents=True)
    fixture_path = fixture_dir / "regression_corpus_x.json"
    fixture_path.write_text(_FIXTURE_CASE_JSON)
    _run_git(repo, "add", "-A", when="2026-01-02T00:00:00")
    _run_git(repo, "commit", "-q", "-m", "add fixture", when="2026-01-02T00:00:00")

    # foo.go touched again AFTER the fixture case was last written --
    # this is the staleness signal.
    cited.write_text("package foo\n\nfunc Bar() {}\n")
    _run_git(repo, "add", "gateway/internal/foo.go", when="2026-01-03T00:00:00")
    _run_git(repo, "commit", "-q", "-m", "extend foo.go", when="2026-01-03T00:00:00")

    case = EvalCase.model_validate(
        {
            "id": "case-1",
            "revision": 1,
            "task_spec": {
                "output": "a",
                "verified_against": "gateway/internal/foo.go:1",
            },
            "reference": "a",
            "tier": "regression",
        }
    )
    findings = check_case_staleness(case, fixture_path, repo)
    assert len(findings) == 1
    assert findings[0].eval_case_id == "case-1"
    assert findings[0].citation.file == "gateway/internal/foo.go"
    assert findings[0].fixture_touched == "2026-01-02"
    assert findings[0].cited_file_touched == "2026-01-03"


def test_check_case_staleness_no_finding_when_fixture_is_newer(tmp_path):
    repo = tmp_path
    _init_repo(repo)

    cited = repo / "gateway" / "internal" / "foo.go"
    cited.parent.mkdir(parents=True)
    cited.write_text("package foo\n")
    _run_git(repo, "add", "gateway/internal/foo.go", when="2026-01-01T00:00:00")
    _run_git(repo, "commit", "-q", "-m", "add foo.go", when="2026-01-01T00:00:00")

    fixture_dir = repo / "evals" / "tests" / "fixtures"
    fixture_dir.mkdir(parents=True)
    fixture_path = fixture_dir / "regression_corpus_x.json"
    fixture_path.write_text(_FIXTURE_CASE_JSON)
    # Fixture written AFTER foo.go's only touch -- the citation is fresh.
    _run_git(repo, "add", "-A", when="2026-01-05T00:00:00")
    _run_git(repo, "commit", "-q", "-m", "add fixture", when="2026-01-05T00:00:00")

    case = EvalCase.model_validate(
        {
            "id": "case-1",
            "revision": 1,
            "task_spec": {
                "output": "a",
                "verified_against": "gateway/internal/foo.go:1",
            },
            "reference": "a",
            "tier": "regression",
        }
    )
    assert check_case_staleness(case, fixture_path, repo) == []


def test_check_case_staleness_no_verified_against_returns_empty(tmp_path):
    case = EvalCase.model_validate(
        {
            "id": "case-1",
            "revision": 1,
            "task_spec": {"output": "a"},
            "tier": "regression",
        }
    )
    assert check_case_staleness(case, tmp_path / "nonexistent.json", tmp_path) == []


def test_check_corpus_staleness_cli_reports_and_never_fails_on_findings(tmp_path):
    repo = tmp_path
    _init_repo(repo)

    cited = repo / "gateway" / "internal" / "foo.go"
    cited.parent.mkdir(parents=True)
    cited.write_text("package foo\n")
    _run_git(repo, "add", "gateway/internal/foo.go", when="2026-01-01T00:00:00")
    _run_git(repo, "commit", "-q", "-m", "add foo.go", when="2026-01-01T00:00:00")

    suite_path = repo / "regression_corpus_x.json"
    suite_path.write_text(_FIXTURE_CASE_JSON)
    _run_git(repo, "add", "-A", when="2026-01-02T00:00:00")
    _run_git(repo, "commit", "-q", "-m", "add fixture", when="2026-01-02T00:00:00")

    cited.write_text("package foo\n\nfunc Bar() {}\n")
    _run_git(repo, "add", "gateway/internal/foo.go", when="2026-01-03T00:00:00")
    _run_git(repo, "commit", "-q", "-m", "extend foo.go", when="2026-01-03T00:00:00")

    out_path = repo / "findings.json"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "check-corpus-staleness",
            "--suite",
            str(suite_path),
            "--repo-root",
            str(repo),
            "--out",
            str(out_path),
        ],
    )
    assert result.exit_code == 0, result.output
    assert "case-1" in result.output
    assert out_path.exists()
