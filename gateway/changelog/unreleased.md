# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `gateway` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.2.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) — load-bearing for the Go module path/tag (`v0.1.0`, `v0.2.0`, ...).

## Added

## Changed

## Deprecated

## Removed

## Fixed

- **A local admin mutation can no longer be silently dropped as "stale" — the root cause of the CI-only `TestConcurrentDeleteVirtualKeyRequestsForSameNameNeverBothSucceed` failures first disclosed on 2026-09-22.** `UpsertVirtualKey`, `DeleteVirtualKey`, `RotateVirtualKey` and `UpdateDeploymentWeight` read the wall clock *before* taking the mutation lock, so two concurrent local calls whose clock reads and lock acquisitions inverted made the loser look older than the version already recorded, and the last-writer-wins guard — which exists for late-arriving *remote* events and is correct for them — returned success without applying or publishing anything: two `204`s and two audit entries for one deletion, or a later local upsert / weight update discarded while its caller was told it succeeded. A local mutation now derives its version under the lock and strictly past the newest version this replica has already applied for the same target (`internal/gateway/dataplane/mutation_version.go`), so it can never be stale against the replica's own view, whatever the clock does; the remote-apply paths (`Apply*FromEvent`) still honour the originating replica's own version. Because two lagging replicas can now legitimately publish the *same* token for the same target, the last-writer-wins guard additionally breaks equal-token ties deterministically by the originating instance ID (the same token from the same origin remains the idempotent re-apply that canary promotion relies on), so every replica converges on one value instead of two replicas swapping values. Consequence for operators: the event envelope's `published_at_unix_nano` remains a last-writer-wins ordering token but may now be one nanosecond past a previously applied version rather than the exact wall-clock time — treat it as an ordering token, not as a timestamp to display or window on. Covered by ten deterministic tests: five that inject a clock reading backwards between two sequential local calls (delete twice, later upsert, rotate-then-delete, upsert-then-rotate, later weight update), and five cross-replica tests that simulate several replicas in one process (a local delete and a local weight update bumping past a far-future remote version with the published token checked, equal-token convergence with bystanders receiving in both orders, and the tie-break rules).

## Security
