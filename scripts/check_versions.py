#!/usr/bin/env python3
"""Fail when evals/pyproject.toml's version does not match the newest evals changelog file.

The gateway deployable needs no such check: its version is carried by the git
tag alone (module github.com/kelvran/gateway/gateway, tags gateway/vX.Y.Z).
The evals deployable is a Python package whose metadata version must be bumped
by hand, and it drifted (0.8.0 declared while evals/v0.10.1 was released) because
RELEASE.md's procedure had no bump step. Run from the repository root:

    python3 -I scripts/check_versions.py

Exit 0 when they match, 1 with a one-line explanation when they do not.
"""
from __future__ import annotations

import pathlib
import re
import sys
import tomllib

ROOT = pathlib.Path(__file__).resolve().parent.parent
PYPROJECT = ROOT / "evals" / "pyproject.toml"
CHANGELOG_DIR = ROOT / "evals" / "changelog"
SEMVER_FILE = re.compile(r"^(\d+)\.(\d+)\.(\d+)\.md$")


def declared_version() -> str:
    with PYPROJECT.open("rb") as fh:
        data = tomllib.load(fh)
    return str(data["project"]["version"])


def newest_changelog_version() -> str:
    versions: list[tuple[int, int, int]] = []
    for path in CHANGELOG_DIR.iterdir():
        match = SEMVER_FILE.match(path.name)
        if match:
            versions.append(tuple(int(part) for part in match.groups()))  # type: ignore[arg-type]
    if not versions:
        raise SystemExit(f"check_versions: no <semver>.md files under {CHANGELOG_DIR}")
    return ".".join(str(part) for part in max(versions))


def main() -> int:
    declared = declared_version()
    newest = newest_changelog_version()
    if declared == newest:
        print(f"check_versions: evals/pyproject.toml {declared} matches evals/changelog/{newest}.md")
        return 0
    print(
        f"check_versions: evals/pyproject.toml declares {declared} but the newest "
        f"changelog file is evals/changelog/{newest}.md; bump pyproject.toml (and uv.lock) "
        "before tagging evals/v" + newest
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
