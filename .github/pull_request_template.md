## Summary

<!-- What changed and why -- link an issue/RFC if one exists. -->

## Which deployable(s)?

- [ ] `gateway/` (Go)
- [ ] `evals/` (Python)
- [ ] `api/` (cross-language contract -- requires `buf breaking` to pass)
- [ ] Docs / CI only

## Test plan

<!-- What did you run? `make verify`, a specific test, a manual check? -->

## Checklist

- [ ] `make verify` passes locally (build + vet + lint + test for both deployables)
- [ ] If this touches `api/`: `buf breaking` passes, and `UPGRADE.md` has an entry if the change is intentionally breaking
- [ ] If this touches `gateway/openapi/`: `scripts/openapi-breaking.sh` passes (oasdiff against the previous `gateway/v*` tag); a sanctioned break has its line in `gateway/openapi/oasdiff-err-ignore.txt` beside the `**BREAKING**` entry
- [ ] If this changes user-facing behavior: `gateway/changelog/unreleased.md` or `evals/changelog/unreleased.md` has an entry
- [ ] If this breaks or deprecates a public surface (`docs/VERSIONING.md` § 2): the changelog entry is marked `**BREAKING**` / `## Deprecated`, and `UPGRADE.md` / `DEPRECATED.md` has the row
