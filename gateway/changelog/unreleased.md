# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `gateway` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.2.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) — load-bearing for the Go module path/tag (`v0.1.0`, `v0.2.0`, ...).

## Added

## Changed

## Deprecated

## Removed

## Fixed
- The `publish-image` job no longer tries to attach the image SBOM to the GitHub Release (`release.yml` creates that Release first, and the job deliberately holds `contents: read`); the CycloneDX SBOM and SLSA provenance are attested to the registry only. The job can also be dispatched for an existing `gateway/v*` tag (`workflow_dispatch`, input `release_tag`) to rebuild, push, sign and attest that tag's image from the current workflow definition. The `gateway/v0.18.0` image was re-published this way (index `sha256:acf52ab117fb…`, signed, SBOM and provenance attested; the tag run's first index, `sha256:a68f3eaf…`, stays in GHCR untagged).

## Security
