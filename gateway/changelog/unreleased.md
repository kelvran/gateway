# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `gateway` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.2.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) — load-bearing for the Go module path/tag (`v0.1.0`, `v0.2.0`, ...).

## Added

- `api/gatewayevents/v1` enrichment, per `docs/rfcs/2026-09-03-gatewayevents-decision-enrichment.md`: `GatewayDecisionEvent` gains 5 new fields (7–11) closing the exact gap the original contract RFC named and deferred — `rate_limit_fail_open` (bool: the rate limiter's backend errored and the request was let through anyway), `fallback_happened`/`fallback_from_deployment`/`fallback_reason` (whether a request fell back to a second deployment, from which one, and why), and `budget_spent_usd` (the key's real cumulative spend at the moment the budget check ran, decimal-as-string, populated even on a `BUDGET_EXCEEDED` rejection). Purely additive proto change (`buf breaking` confirmed clean under the repo's `FILE` category) — no `UPGRADE.md` entry. `checkRateLimit` widens its return to `(ok, failedOpen bool)`; a new unexported `fallbackInfo` struct threads fallback detail through both the buffered inline fallback block and `streamDeploymentWithFallback`'s return; a new `budget.Tracker.SpentUSD` getter is called at the `budget.Allow` call site (never inside `finalize`) so the captured spend reflects decision-time, not finalize-time.

## Changed

## Deprecated

## Removed

## Fixed

## Security
