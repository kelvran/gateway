"""Deterministic scorers: exact-match and regex-match.

No external dependency, no network call — these are the cheapest tier of
the Scorer Service and always run before any LLM-judge call.
"""

from __future__ import annotations

import re


def exact_match(output: str, reference: str) -> bool:
    """Return True iff `output` is exactly equal to `reference`."""
    return output == reference


def regex_match(output: str, pattern: str) -> bool:
    """Return True iff `pattern` matches somewhere in `output`.

    Uses `re.search`, not `re.match` — the pattern does not need to anchor
    at the start of `output` unless the caller's pattern says so explicitly.
    """
    return re.search(pattern, output) is not None
