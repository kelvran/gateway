"""Report-only citation-staleness checker for `task_spec["verified_against"]`
prose across `regression_corpus_*.json` fixtures. A `verified_against`
string names one or more real gateway/evals source files a case's own
`output`/`reference` claim was checked against at authoring time -- this
module flags when a cited file has since been touched MORE RECENTLY than
the corpus case itself, a signal the citation may now be stale (the
verification it once proved may no longer hold), never a hard failure.

Deliberately report-only, unlike `evals.field_swap_lint`: a bulk-edit
commit (the exact kind this session's own triage swarms have produced
repeatedly) bumps every case's OWN blame timestamp too, and a cited file
can be touched for a reason wholly unrelated to the specific claim a case
cites it for (a comment fix, an unrelated function added to the same
file) -- both are real, structural false-positive risks a human reviewer
can dismiss at a glance but a hard CI gate cannot. This mirrors
`evals.audit_corpus`'s own established discipline for exactly this kind of
judgment-requiring signal.

## The `verified_against` citation grammar

Real citations in this corpus (all 20 real instances live in
`regression_corpus_cost_abuse.json` as of 2026-09-11) use a natural-
language, NOT machine-designed grammar: `;`-separated groups, each
typically opening with a `path/to/file.ext:START-END` citation, followed
by zero or more `, :START-END` or ` and :START-END` continuations that
deliberately omit the filename -- it carries over from the group's own
most recent full-path citation. A real example (verbatim, from
`costabuse-no-custom-pricing-cache-token-doublecount-path`):

    gateway/internal/costaccounting/costaccounting.go:18-28 (Usage struct,
    now with cache fields), :38-43 (ModelPrice struct, now with cache-rate
    fields), :69-80 (Calculator.Calculate, ...), :89-96 (resolveCacheRate);
    gateway/internal/gateway/dataplane/dataplane.go:1790-1796 (...) and
    :1902-1904 (...); gateway/internal/gateway/dataplane/streamrunaway.go:182-184 (...)

A naive per-citation regex without carry-over state would silently parse
`:38-43`/`:69-80`/`:89-96` as citations with NO file at all (or discard
them), and `:1902-1904` as belonging to nothing -- this module's
`parse_citations` tracks the most recently seen FULL path instead. A
citation whose file component is present but has no `/` (a bare
`dataplane.go` restated after its full path was already given earlier in
the same string, also observed in this corpus) is resolved by matching its
basename against the most recent full path sharing that basename.
Citations with no line number at all (a bare file mention, or pure prose
like "repo-wide grep for velocity/anomaly/baseline across
gateway/internal/ (zero matches)") carry no location to blame and are
silently skipped -- not every real citation is a line-attributable one,
and this module never fabricates a location for one that isn't.

Deliberately a flat, third-layer module (per `.importlinter`'s `layers`
contract), sibling to `evals.audit_corpus`/`evals.auto_flag`/
`evals.field_swap_lint` -- imports only `evals.models`.
"""

from __future__ import annotations

import re
import subprocess
from dataclasses import dataclass
from pathlib import Path

from evals.models import EvalCase

_CITATION_RE = re.compile(
    r"(?P<file>[\w./-]*\.[A-Za-z]{1,6})?:(?P<start>\d+)(?:-(?P<end>\d+))?"
)


@dataclass(frozen=True)
class Citation:
    """One resolved `file:start-end` reference parsed out of a
    `verified_against` string. `file` is always a repo-root-relative path
    with at least one `/` -- a carried-over or basename-only citation is
    resolved to the full path it was resolved against before this type is
    ever constructed; there is no "unresolved file" representation here.
    """

    file: str
    start_line: int
    end_line: int


def parse_citations(verified_against: str) -> list[Citation]:
    """Parse every line-attributable citation out of a `verified_against`
    string, resolving carried-over and basename-only file references
    against the most recently seen full path. Citations with no line
    number, or a bare filename that can't be resolved against any
    already-seen full path (e.g. the first citation in a malformed
    string), are silently omitted -- never guessed.
    """
    citations: list[Citation] = []
    last_full_path: str | None = None

    for match in _CITATION_RE.finditer(verified_against):
        file_group = match.group("file")
        start = int(match.group("start"))
        end = int(match.group("end")) if match.group("end") else start

        resolved: str | None
        if file_group and "/" in file_group:
            resolved = file_group
            last_full_path = file_group
        elif file_group:
            # A bare filename (no directory) -- resolve against the most
            # recently seen full path sharing this exact basename.
            if last_full_path is not None and Path(last_full_path).name == file_group:
                resolved = last_full_path
            else:
                resolved = None
        else:
            # No file component at all -- a carried-over ":START-END".
            resolved = last_full_path

        if resolved is not None:
            citations.append(Citation(file=resolved, start_line=start, end_line=end))

    return citations


@dataclass(frozen=True)
class StalenessFinding:
    """One citation whose cited file was touched more recently than the
    corpus case citing it -- a signal the citation may be stale, not a
    confirmed defect. `fixture_touched`/`cited_file_touched` are
    `--date=short` (YYYY-MM-DD) strings, day-granularity by design so the
    two independently-sourced dates (a `git blame` line date, a `git log`
    whole-file date) are directly comparable without a timezone/precision
    mismatch.
    """

    eval_case_id: str
    citation: Citation
    fixture_touched: str
    cited_file_touched: str


def _git_date(repo_root: Path, args: list[str]) -> str | None:
    """Run a `git` subcommand expected to print a `--date=short` date as
    the FIRST line of stdout (a plain `git log --format=%cd` prints
    nothing else; `git log -L<range>:<file>` prints the date followed by
    a blank line and a diff hunk -- only the first line is ever the
    date). Returns `None` on any failure (an unresolvable citation, a
    file git has no history for, git itself unavailable) -- never raises.
    This tool is report-only; a git-plumbing failure must never crash a
    corpus-wide scan over one bad citation.
    """
    try:
        result = subprocess.run(
            ["git", *args],
            cwd=repo_root,
            capture_output=True,
            text=True,
            check=True,
        )
    except (subprocess.CalledProcessError, FileNotFoundError):
        return None
    first_line = result.stdout.splitlines()[0].strip() if result.stdout else ""
    return first_line or None


def _fixture_case_touched(
    repo_root: Path, fixture_path: Path, case_id: str
) -> str | None:
    """The `--date=short` date the line containing `"id": "<case_id>"`
    was last touched in `fixture_path`, via `git log -L` (a per-line
    history query, functionally equivalent to `git blame` for "when was
    this exact line last changed" but composes more directly with
    `--format`) -- this is the corpus case's own "last verified" proxy,
    since `EvalCase` itself has no timestamp field at all.
    """
    try:
        lines = fixture_path.read_text().splitlines()
    except OSError:
        return None

    needle = f'"id": "{case_id}"'
    line_no = next((i + 1 for i, line in enumerate(lines) if needle in line), None)
    if line_no is None:
        return None

    blame_output = _git_date(
        repo_root,
        [
            "log",
            "-1",
            "--format=%cd",
            "--date=short",
            f"-L{line_no},{line_no}:{fixture_path}",
        ],
    )
    return blame_output


def check_case_staleness(
    case: EvalCase,
    fixture_path: Path,
    repo_root: Path,
) -> list[StalenessFinding]:
    """Return one `StalenessFinding` per citation in `case`'s
    `verified_against` whose cited file was touched more recently than
    the case's own line in `fixture_path`. Returns `[]` (never raises) if
    `case` has no `verified_against`, its own line can't be found, or any
    individual citation's dates can't be resolved.
    """
    verified_against = case.task_spec.get("verified_against")
    if not verified_against:
        return []

    fixture_touched = _fixture_case_touched(repo_root, fixture_path, case.id)
    if fixture_touched is None:
        return []

    findings: list[StalenessFinding] = []
    for citation in parse_citations(verified_against):
        cited_path = repo_root / citation.file
        if not cited_path.is_file():
            continue
        cited_touched = _git_date(
            repo_root,
            ["log", "-1", "--format=%cd", "--date=short", "--", citation.file],
        )
        if cited_touched is None:
            continue
        if cited_touched > fixture_touched:
            findings.append(
                StalenessFinding(
                    eval_case_id=case.id,
                    citation=citation,
                    fixture_touched=fixture_touched,
                    cited_file_touched=cited_touched,
                )
            )
    return findings
