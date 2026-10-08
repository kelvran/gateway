# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `evals` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.2.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) by default. Revisit CalVer (`YYYY.MM.PATCH`) once `evals` ships continuously without hard breaking changes — see `RELEASE.md`.

## Added
- `docs/VERSIONING.md` states what an `evals/vX.Y.Z` version promises: the CLI commands and their options and the persisted `Run`/`Score` schemas are the public surface (no module is importable-public yet), PATCH never breaks, MINOR breaks only with a `**BREAKING**` changelog entry and an `UPGRADE.md` row (enforced by `scripts/release-preflight.sh` at the tag), and the latest minor gets all fixes while the previous minor gets security fixes for 90 days.

## Changed

## Deprecated

## Removed

## Fixed
- `pyproject.toml` declared `version = "0.8.0"` for the `v0.10.1` code (and had since `v0.9.0`): the release procedure had no version-bump step. Bumped to `0.10.1`, `uv.lock` refreshed, and `scripts/check_versions.py` now fails CI when the declared version lags the newest `evals/changelog/<version>.md`.

## Security
