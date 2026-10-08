#!/usr/bin/env bash
# Release preflight for one deployable. Run from the repository root:
#
#   scripts/release-preflight.sh gateway 0.18.0
#   scripts/release-preflight.sh evals 0.11.0
#
# Exit 0 when every check passes, 1 with one line per failure otherwise.
# .github/workflows/release.yml and release-evals.yml run this at the pushed
# tag before building anything, so a release that forgot a RELEASE.md step
# fails in seconds instead of publishing half-finished assets. Checks:
#
#   1. <deployable>/changelog/<version>.md exists and is not empty.
#   2. <deployable>/changelog/unreleased.md holds no entries (its content
#      must have been moved into the dated file).
#   3. Every **BREAKING** entry in the dated changelog has a row in
#      UPGRADE.md naming <deployable>/v<version>.
#   4. (evals only) pyproject.toml declares exactly <version>, and
#      scripts/check_versions.py agrees.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <gateway|evals> <version-without-v>" >&2
  exit 2
fi
deployable=$1
version=$2
case "$deployable" in
  gateway|evals) ;;
  *) echo "preflight: unknown deployable $deployable (want gateway or evals)" >&2; exit 2 ;;
esac
if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "preflight: version $version is not SemVer without the v prefix" >&2
  exit 2
fi

root=$(cd "$(dirname "$0")/.." && pwd)
failures=0
fail() {
  echo "preflight FAIL: $*" >&2
  failures=$((failures + 1))
}

dated="$root/$deployable/changelog/$version.md"
if [ ! -s "$dated" ]; then
  fail "$deployable/changelog/$version.md is missing or empty -- move unreleased.md into it first (RELEASE.md step 1)"
fi

unreleased="$root/$deployable/changelog/unreleased.md"
if [ -f "$unreleased" ] && grep -Eq '^\s*[-*] ' "$unreleased"; then
  fail "$deployable/changelog/unreleased.md still holds entries -- it must be reset to empty category headers (RELEASE.md step 2)"
fi

if [ -s "$dated" ] && grep -q '\*\*BREAKING\*\*' "$dated"; then
  if ! grep -q "$deployable/v$version" "$root/UPGRADE.md"; then
    fail "$dated has a **BREAKING** entry but UPGRADE.md has no row for $deployable/v$version"
  fi
fi

if [ "$deployable" = "evals" ]; then
  declared=$(python3 -I - "$root/evals/pyproject.toml" <<'PY'
import sys, tomllib
with open(sys.argv[1], "rb") as fh:
    print(tomllib.load(fh)["project"]["version"])
PY
)
  if [ "$declared" != "$version" ]; then
    fail "evals/pyproject.toml declares version $declared, tag says $version (RELEASE.md evals step 1)"
  fi
  if ! python3 -I "$root/scripts/check_versions.py"; then
    fail "scripts/check_versions.py disagrees with the newest evals changelog file"
  fi
fi

if [ "$failures" -gt 0 ]; then
  echo "preflight: $failures check(s) failed for $deployable v$version" >&2
  exit 1
fi
echo "preflight OK: $deployable v$version"
