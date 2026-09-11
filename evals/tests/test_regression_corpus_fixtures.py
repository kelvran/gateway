"""Corpus-wide structural checks across every `regression_corpus_*.json`
fixture file (per the judge-panel-reducer implementation plan's Phase 4).

Deliberately structural only — every individual case's own real-world
correctness (attack scenarios, judge verdicts, etc.) is each corpus
file's own concern, verified when it was authored. What no single file's
own content can catch is a cross-file collision: an `EvalCase.id` reused
across two different corpus files would silently break anything that
joins on id (`evals promote`, cache-key lookups keyed by case id), and
nothing before this test ever checked for that across the whole set.
"""

from __future__ import annotations

import json
from pathlib import Path

from evals.field_swap_lint import find_reference_swaps
from evals.models import EvalCase

_FIXTURES_DIR = Path(__file__).parent / "fixtures"


def _regression_corpus_files() -> list[Path]:
    return sorted(_FIXTURES_DIR.glob("regression_corpus_*.json"))


def test_at_least_the_known_regression_corpus_files_are_present():
    # A minimal, load-bearing sanity check: if this ever drops to zero,
    # every other test in this file passes vacuously and would hide it.
    assert len(_regression_corpus_files()) >= 6


def test_every_case_in_every_regression_corpus_file_is_a_valid_eval_case():
    for path in _regression_corpus_files():
        raw_cases = json.loads(path.read_text())
        for raw_case in raw_cases:
            EvalCase.model_validate(raw_case)


def test_no_eval_case_id_is_reused_across_regression_corpus_files():
    id_to_files: dict[str, list[str]] = {}
    for path in _regression_corpus_files():
        raw_cases = json.loads(path.read_text())
        for raw_case in raw_cases:
            id_to_files.setdefault(raw_case["id"], []).append(path.name)

    duplicates = {
        case_id: files for case_id, files in id_to_files.items() if len(files) > 1
    }
    assert duplicates == {}


def test_no_eval_case_id_is_reused_within_a_single_regression_corpus_file():
    for path in _regression_corpus_files():
        raw_cases = json.loads(path.read_text())
        ids = [raw_case["id"] for raw_case in raw_cases]
        assert len(ids) == len(set(ids)), f"{path.name} has a duplicate id"


def test_no_regression_corpus_file_has_a_field_swap_between_two_cases():
    # A cross-case collision no single case's own content can catch --
    # the exact bug class (guardrail-13/18, DECISIONS.md 2026-09-15) where
    # two cases' `reference` values were transposed with each other,
    # silently making both regression-guard cases fail regardless of real
    # system behavior. Checked per-file, not across the whole corpus at
    # once: a legitimate "GAP" convention case's reference is specific to
    # its own file's cited codepath, so a cross-file swap has never been
    # observed and isn't this test's concern.
    for path in _regression_corpus_files():
        raw_cases = json.loads(path.read_text())
        cases = [EvalCase.model_validate(raw_case) for raw_case in raw_cases]
        swaps = find_reference_swaps(cases)
        assert swaps == [], f"{path.name} has a field swap: {swaps}"
