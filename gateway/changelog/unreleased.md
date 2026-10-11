# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `gateway` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.2.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) — load-bearing for the Go module path/tag (`v0.1.0`, `v0.2.0`, ...).

## Added

## Changed

## Deprecated

## Removed

## Fixed
- `400` `system_parts_unsupported`, `400` `lossy_ingress_rejected` and `400` `invalid_prompt_reference` (every cause: `prompt_id` with `messages`, `prompt_label` with `prompt_version`, an unknown prompt, a stored prompt whose content fails validation) no longer carry `Retry-After`, no longer bump the key's retry backoff, and their decision event records `OUTCOME_INVALID_REQUEST` instead of `OUTCOME_UPSTREAM_ERROR`. All are decided before any byte reaches an upstream — the rule that plan gate G16 set for `response_format_unsupported` and `idempotency_key_reused` in `gateway/v0.19.0` — but the three codes were still classified as upstream errors (the system-parts and lossy codes were added after G16; the prompt-reference code existed before it), so a client that honoured the header waited for nothing and the key's next real rejection started from a longer backoff. Found by item 14's design review against the route inventory and the review of this fix.

## Security
