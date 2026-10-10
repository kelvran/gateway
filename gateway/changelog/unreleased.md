# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `gateway` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.2.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) — load-bearing for the Go module path/tag (`v0.1.0`, `v0.2.0`, ...).

## Added
- The canonical chat response carries the provider's native stop reason beside the canonical `finish_reason`: `stop_reason` and, for Anthropic, the matched `stop_sequence`, on `/v1/chat/completions` responses and on the streaming chunk that sets `finish_reason`, for `anthropic` and `bedrock` deployments (omitted elsewhere; `finish_reason` is unchanged). They round-trip through the cache and the streaming replay. The first canonical-schema gap fill for the Anthropic Messages ingress (item 11 slice S3; the fields RFC-1 §1's `EncodeResponse` reads): an Anthropic-shaped response can then be rendered without guessing the stop reason back from `stop`.

## Changed
- Two error statuses that were the `502` `upstream_error` default now say what happened (plan gate G16, decided 2026-10-10 together with RFC-1; a MINOR change under `docs/VERSIONING.md` §3: statuses are public surface, and nothing a documented client does stops working — the SDKs still raise an exception, the only `Retry-After` promise in §2 is on 429, and `type`/`code` gain values without losing any): `response_format` on a model pool with no deployment able to enforce it is `400` `invalid_request_error` / `response_format_unsupported` with `param` `response_format`; an `Idempotency-Key` reused within its window with a different body is `422` `invalid_request_error` / `idempotency_key_reused` with `param` `Idempotency-Key`. Neither carries `Retry-After` or bumps the key's retry backoff any more, and the decision event records `OUTCOME_INVALID_REQUEST` instead of `OUTCOME_UPSTREAM_ERROR`. Both are decided before any upstream call, as before.

## Deprecated

## Removed

## Fixed
- The `publish-image` job no longer tries to attach the image SBOM to the GitHub Release (`release.yml` creates that Release first, and the job deliberately holds `contents: read`); the CycloneDX SBOM and SLSA provenance are attested to the registry only. The job can also be dispatched for an existing `gateway/v*` tag (`workflow_dispatch`, input `release_tag`) to rebuild, push, sign and attest that tag's image from the current workflow definition. The `gateway/v0.18.0` image was re-published this way (index `sha256:acf52ab117fb…`, signed, SBOM and provenance attested; the tag run's first index, `sha256:a68f3eaf…`, stays in GHCR untagged).

## Security
