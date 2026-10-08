#!/usr/bin/env bash
# Fail when a tracked file cites a repository path under docs/ that does not
# exist. The repository cites documentation paths in backticks and prose far
# more often than in Markdown links, which lychee never sees; this is the
# second half of the docs link check (the first is `lychee --offline`).
#
# Scope: `docs/<path>.<ext>` citations in *.md *.go *.py *.yaml *.yml, root
# anchored (a relative link such as ../operations/X.md is lychee's job). A
# citation preceded by a path character is part of a longer path or URL (for
# example github.com/ossf/scorecard/blob/main/docs/checks.md) and is skipped;
# so is a template placeholder containing YYYY or <…>.
# Exclusions, each deliberate:
#   docs/upgrade-research/  research notes cite paths they PROPOSED; many were
#                           never created, by design (the notes are a record).
#   docs/agents/LOGS.md     append-only history; a path that later moved stays
#   DECISIONS.md            as it was written (the repo never edits past entries).
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

fail=0
count=0
while IFS= read -r hit; do
  file=${hit%%:*}
  rest=${hit#*:}
  line=${rest%%:*}
  cited=${rest#*:}
  target=${cited%%#*}
  case "$target" in *YYYY*|*'<'*) continue ;; esac # template placeholder, not a path
  count=$((count + 1))
  if [ ! -e "$target" ]; then
    printf '%s:%s: cites missing path %s\n' "$file" "$line" "$cited"
    fail=1
  fi
done < <(git grep -n -o -P '(?<![A-Za-z0-9_/.-])docs/[A-Za-z0-9_][A-Za-z0-9_./-]*\.(md|txt|yaml|yml|json)' -- \
  '*.md' '*.go' '*.py' '*.yaml' '*.yml' \
  ':(exclude)docs/upgrade-research/**' ':(exclude)docs/agents/LOGS.md' ':(exclude)DECISIONS.md' || true)

if [ "$fail" -ne 0 ]; then
  echo "check-doc-paths: missing targets found ($count citations checked)" >&2
  exit 1
fi
echo "check-doc-paths: ok ($count citations resolve)"
