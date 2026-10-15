#!/usr/bin/env bash
# OpenAPI breaking gate (round-5 item 9, 2026-10-15). Run from anywhere inside
# the repository:
#
#   scripts/openapi-breaking.sh              # base = the newest gateway/v* release tag that does not contain HEAD
#   scripts/openapi-breaking.sh 242d5a2d     # base given (a commit, a tag, any ref; local runs)
#
# The document gateway/openapi/kelvran-gateway.openapi.yaml is public surface
# since gateway/v0.20.0 (docs/VERSIONING.md § 2). The contract tests in
# gateway/cmd/gateway pin it to the code; this compares it with the one shipped
# at the previous gateway/v* tag, so a shipped path, operation, status, header,
# security requirement, operationId, request parameter, request member or
# response enum value cannot vanish or change shape between releases without a
# sanctioned break.
#
#   - oasdiff v1.33.0 (github.com/oasdiff/oasdiff), installed in-runner with
#     `go install` so its exit code is seen as is (`go run` folds every child
#     exit into 1): 0 clean, 1 breaking change at or above --fail-on, 100+ a
#     tool or load error. The GitHub action is not used: by default it uploads
#     the comparison to oasdiff.com.
#   - --fail-on ERR with gateway/openapi/oasdiff-severity.txt raising the § 2
#     surfaces oasdiff rates INFO or WARN by default (one `<check-id> err` per
#     line; the file takes no comments or blank lines, exit 106 otherwise).
#   - gateway/openapi/oasdiff-err-ignore.txt waives a sanctioned MINOR break:
#     one line per finding, `METHOD /path <description>` (substring match,
#     case-insensitive). It must be empty unless a **BREAKING** entry is in
#     flight (TestOpenAPIBreakingGateWaiverIsEmptyUnlessABreakIsInFlight on
#     every push; release-preflight.sh check 8 at the tag); the lines leave in
#     the post-tag follow-up commit (RELEASE.md gateway step 7), once the new
#     tag is the base.
#   - The base is read straight from the git object store (`<ref>:<path>`), so
#     the clone must carry tags (actions/checkout fetch-depth 0). Only release
#     tags (gateway/vX.Y.Z) are base candidates: a pre-release (-rc.N) may
#     change or disappear without a row (docs/VERSIONING.md § 7) and would sort
#     above its GA. On the tag's own push the newest release tag contains HEAD,
#     so the previous release is the base; on a rebuild of an older tag the
#     same rule picks that tag's own predecessor. A base older than
#     gateway/v0.20.0 has no document: a notice and exit 0, legitimate exactly
#     until that tag exists; a base at or after it without the document fails.
#   - oasdiff reads a `.oasdiff.*` file in the working directory or
#     $OASDIFF_CONFIG for every flag the command line leaves unset; both are
#     refused here so nothing reconfigures the gate behind these flags.
#   - oasdiff has no rule for the x-kelvran-* members (its extension-aware
#     rules read only x-extensible-enum, x-stability-level, x-sunset and
#     x-since-date); they stay pinned to the code by the contract tests
#     (docs/reference/openapi.md).
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
doc=gateway/openapi/kelvran-gateway.openapi.yaml
severity=gateway/openapi/oasdiff-severity.txt
waiver=gateway/openapi/oasdiff-err-ignore.txt
first_tag_with_document=gateway/v0.20.0
oasdiff_module=github.com/oasdiff/oasdiff@v1.33.0
release_tag='^gateway/v[0-9]+\.[0-9]+\.[0-9]+$'

note() {
  echo "openapi-breaking: $*" >&2
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::notice::openapi-breaking: $*"; fi
}
fail() {
  echo "openapi-breaking FAIL: $*" >&2
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::error::openapi-breaking: $*"; fi
  exit 1
}

[ -f "$doc" ] || fail "$doc is missing"
[ -f "$severity" ] || fail "$severity is missing"
[ -f "$waiver" ] || fail "$waiver is missing (an empty file when no break is in flight)"

# Nothing may reconfigure oasdiff behind this script's flags.
unset OASDIFF_CONFIG
shopt -s nullglob
for stray in .oasdiff.* oasdiff.*; do
  fail "$stray would reconfigure oasdiff behind this script's flags; remove it"
done
shopt -u nullglob

base=${1:-}
if [ -z "$base" ]; then
  # Newest release tag that does not contain HEAD; versionsort.suffix=- keeps
  # a pre-release below its GA and the grep drops pre-releases altogether.
  base=$(git -c versionsort.suffix=- tag --list 'gateway/v*' --sort=-v:refname --no-contains HEAD | grep -E "$release_tag" | head -1 || true)
  [ -n "$base" ] || fail "no gateway/vX.Y.Z release tag in this clone (actions/checkout needs fetch-depth: 0 to bring the tags)"
fi
git rev-parse --verify -q "$base^{commit}" >/dev/null || fail "base $base does not resolve to a commit in this clone"

if ! git cat-file -e "$base:$doc" 2>/dev/null; then
  # Only a release tag older than the first one carrying the document may
  # legitimately lack it; any other base without the document is a failure.
  if [[ "$base" =~ $release_tag ]]; then
    oldest=$(printf '%s\n%s\n' "$base" "$first_tag_with_document" | sort -V | head -1)
    if [ "$oldest" != "$first_tag_with_document" ]; then
      note "base $base predates the OpenAPI document (first shipped in $first_tag_with_document); nothing to compare yet"
      exit 0
    fi
  fi
  fail "$base carries no $doc"
fi

bin_dir=$(mktemp -d)
trap 'rm -rf "$bin_dir"' EXIT
GOBIN="$bin_dir" go install "$oasdiff_module"

args=(breaking --fail-on ERR --allow-external-refs=false --severity-levels "$severity")
waived=0
if [ -s "$waiver" ]; then
  waived=$(grep -c . "$waiver" || true)
  args+=(--err-ignore "$waiver")
  note "$waived waiver line(s) in $waiver are in effect"
fi
if [ -n "${GITHUB_ACTIONS:-}" ]; then
  args+=(--format githubactions)
else
  args+=(--color never)
fi

set +e
"$bin_dir/oasdiff" "${args[@]}" "$base:$doc" "$doc"
rc=$?
set -e
case "$rc" in
  0) echo "openapi-breaking OK: $doc against $base (${waived} waiver line(s))" ;;
  1) fail "breaking change(s) in $doc against $base -- see the report above; a sanctioned MINOR break needs its line in $waiver and a **BREAKING** changelog entry (docs/VERSIONING.md § 3)" ;;
  *) fail "oasdiff exited $rc (a tool or load error, not a verdict)" ;;
esac
