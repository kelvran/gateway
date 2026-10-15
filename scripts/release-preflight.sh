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
#   5. WARNING only (also a ::warning:: annotation under GitHub Actions):
#      docs/VERSIONING.md's support-window table lists the release being cut
#      as the latest minor (RELEASE.md asks for it to be updated per release).
#   6. (gateway only) gateway/openapi/kelvran-gateway.openapi.yaml exists and
#      its info.version equals <version>; FAIL on either. The document is
#      public surface since gateway/v0.20.0 (docs/VERSIONING.md § 2).
#      info.version is outside that surface, but RELEASE.md gateway step 2
#      requires setting it and the file ships inside the tag's Go module zip,
#      so a stale value reaches anyone generating a client from it (a WARNING
#      until 2026-10-12).
#   7. (gateway only, added 2026-10-15) one Go toolchain: the `go` directive
#      of gateway/go.mod and gateway/compat/go.mod equals the
#      tag of every golang:<version>-alpine FROM line in
#      gateway/Dockerfile, neither go.mod carries a `toolchain` directive
#      (actions/setup-go would install it in preference to the go line; the
#      GOTOOLCHAIN=local image ignores it), no workflow (*.yml or *.yaml)
#      pins a literal go-version, and every actions/setup-go step reads
#      go-version-file: gateway/go.mod. FAIL on any. The suite's
#      TestOneGoToolchain (gateway/cmd/gateway) fails on every push for the
#      same drift; this repeats it at the tag, in a job that runs no
#      setup-go (pure shell).
#   8. (gateway only, added 2026-10-15) the OpenAPI breaking-gate waiver
#      gateway/openapi/oasdiff-err-ignore.txt is empty unless the dated
#      changelog carries a **BREAKING** entry: a waiver sanctions a MINOR
#      break (docs/VERSIONING.md § 3); its lines leave in the post-tag
#      follow-up commit (RELEASE.md gateway step 7). FAIL otherwise. The gate
#      itself (oasdiff against the previous tag, scripts/openapi-breaking.sh)
#      runs in release.yml's build job, which has the tags and the Go
#      toolchain this job does not.
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

# The marker is the bold **BREAKING** docs/VERSIONING.md § 3 prescribes, matched
# case-insensitively; the UPGRADE.md check wants a TABLE ROW for the release
# ("| `<deployable>/v<version>` |"), not a passing mention in prose.
if [ -s "$dated" ] && grep -Eiq '\*\*breaking\*\*' "$dated"; then
  if ! grep -Eq "^\| \`$deployable/v$version\` \|" "$root/UPGRADE.md"; then
    fail "$dated has a **BREAKING** entry but UPGRADE.md has no table row for $deployable/v$version"
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

# Support-window table: the "Latest minor" column of the row for this deployable.
if ! grep -Eq "^\| \`$deployable\` \| \`$deployable/v$version\`" "$root/docs/VERSIONING.md"; then
  msg="docs/VERSIONING.md support-window table does not list $deployable/v$version as the latest minor -- update it (RELEASE.md)"
  echo "preflight WARN: $msg" >&2
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::warning::$msg"; fi
fi

# OpenAPI document (gateway only): missing is a release defect, and so is a
# mismatching info.version -- outside the public surface, but a RELEASE.md
# step 2 item that ships inside the tag's Go module zip (a warning until
# 2026-10-12). The sed prints the first `  version:` line inside the `info:`
# block -- the range ends at the next top-level key, so a two-space
# `version:` under another key is never the one compared -- and quits: one
# process, no YAML dependency, no SIGPIPE under pipefail.
if [ "$deployable" = "gateway" ]; then
  doc="$root/gateway/openapi/kelvran-gateway.openapi.yaml"
  if [ ! -f "$doc" ]; then
    fail "gateway/openapi/kelvran-gateway.openapi.yaml is missing -- the OpenAPI document is public surface since gateway/v0.20.0 (docs/VERSIONING.md § 2)"
  else
    declared=$(sed -n '/^info:/,/^[^ ]/{/^  version: /{s///p;q;};}' "$doc")
    if [ "$declared" != "$version" ]; then
      fail "gateway/openapi/kelvran-gateway.openapi.yaml declares info.version ${declared:-<none>}, tag says $version -- set it (RELEASE.md gateway step 2)"
    fi
  fi
fi

# One Go toolchain (gateway only): the official golang image and actions/setup-go
# both run with GOTOOLCHAIN=local, so a go.mod ahead of the image breaks
# `docker build` and an image ahead of go.mod ships a standard library the
# suite never ran on. Dependabot moves the image alone, so the declarations are
# compared as whole versions, every golang:<version>-alpine FROM line counts
# (the variant this Dockerfile uses; another variant would be skipped, not
# compared), a `toolchain`
# directive is refused (setup-go installs it in preference to the go line, the
# image ignores it), and every setup-go step must read gateway/go.mod. Pure
# shell: the release preflight job runs no setup-go. The go.mod sed prints its
# first match and quits (one process per file, no SIGPIPE under pipefail).
if [ "$deployable" = "gateway" ]; then
  directive='/^go [0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*[[:space:]]*$/{s/^go //;s/[[:space:]]*$//;p;q;}'
  gomod=$(sed -n "$directive" "$root/gateway/go.mod")
  compat=$(sed -n "$directive" "$root/gateway/compat/go.mod")
  images=$(sed -nE 's/^FROM .*golang:([0-9]+\.[0-9]+\.[0-9]+)-alpine.*$/\1/p' "$root/gateway/Dockerfile")
  if [ -z "$gomod" ] || [ -z "$compat" ] || [ -z "$images" ]; then
    fail "one Go toolchain: could not read a go X.Y.Z directive (gateway/go.mod: ${gomod:-<none>}, gateway/compat/go.mod: ${compat:-<none>}) or a builder tag (gateway/Dockerfile golang:<version>-alpine: ${images:-<none>})"
  else
    for image in $images; do
      if [ "$gomod" != "$compat" ] || [ "$gomod" != "$image" ]; then
        fail "one Go toolchain: gateway/go.mod says go $gomod, gateway/compat/go.mod says go $compat, gateway/Dockerfile builds with golang:$image-alpine -- bump them together"
        break
      fi
    done
  fi
  if grep -Eq '^toolchain([[:space:]]|$)' "$root/gateway/go.mod" "$root/gateway/compat/go.mod"; then
    fail "one Go toolchain: a toolchain directive in go.mod -- actions/setup-go installs it in preference to the go line while the GOTOOLCHAIN=local image builds with the go line; delete it and move the go line instead"
  fi
  # Workflows: *.yml and *.yaml both run. grep's exit 1 (no match) is a result,
  # exit 2 is a scan failure, so the status is read instead of masked.
  set +e
  workflows=$(find "$root/.github/workflows" -maxdepth 1 -type f \( -name '*.yml' -o -name '*.yaml' \) | sort)
  # shellcheck disable=SC2086 # newline-separated repository paths without spaces
  literal=$(grep -lE '^[[:space:]]*go-version:' $workflows /dev/null 2>&1)
  literal_rc=$?
  # shellcheck disable=SC2086
  steps=$(cat $workflows /dev/null | grep -cE '^[[:space:]]*-?[[:space:]]*uses: ["'"'"']?actions/setup-go@')
  # shellcheck disable=SC2086
  reads=$(cat $workflows /dev/null | grep -cE '^[[:space:]]*go-version-file: gateway/go\.mod[[:space:]]*$')
  set -e
  if [ -z "$workflows" ]; then
    fail "one Go toolchain: no workflow files under .github/workflows -- the workflows moved?"
  elif [ "$literal_rc" -eq 0 ]; then
    fail "one Go toolchain: $(printf '%s\n' "$literal" | sed "s#^$root/##" | tr '\n' ' ')pin a literal go-version -- every actions/setup-go step reads go-version-file: gateway/go.mod"
  elif [ "$literal_rc" -ne 1 ]; then
    fail "one Go toolchain: could not scan .github/workflows for go-version (grep exit $literal_rc: $literal)"
  fi
  if [ "${steps:-0}" -eq 0 ]; then
    fail "one Go toolchain: no actions/setup-go step under .github/workflows -- the workflows moved?"
  elif [ "$steps" != "$reads" ]; then
    fail "one Go toolchain: $steps actions/setup-go step(s) but $reads go-version-file: gateway/go.mod line(s) across .github/workflows -- every step reads gateway/go.mod"
  fi
fi

# OpenAPI breaking-gate waiver (gateway only): lines in the ignore file waive
# findings of scripts/openapi-breaking.sh; they are legitimate only beside a
# **BREAKING** entry in the release being cut; the lines leave in the post-tag
# follow-up commit.
if [ "$deployable" = "gateway" ]; then
  waiver="$root/gateway/openapi/oasdiff-err-ignore.txt"
  if [ ! -f "$waiver" ]; then
    fail "gateway/openapi/oasdiff-err-ignore.txt is missing -- the OpenAPI breaking gate needs the (empty) waiver file"
  elif grep -q . "$waiver" && ! { [ -s "$dated" ] && grep -Eiq '\*\*breaking\*\*' "$dated"; }; then
    fail "gateway/openapi/oasdiff-err-ignore.txt holds waiver line(s) but $deployable/changelog/$version.md has no **BREAKING** entry -- a waiver is only for a sanctioned break; its lines leave in the post-tag follow-up commit (RELEASE.md gateway step 7)"
  fi
fi

if [ "$failures" -gt 0 ]; then
  echo "preflight: $failures check(s) failed for $deployable v$version" >&2
  exit 1
fi
echo "preflight OK: $deployable v$version"
