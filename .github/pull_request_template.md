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
- [ ] If this changes user-facing behavior: `gateway/changelog/unreleased.md` or `evals/changelog/unreleased.md` has an entry
