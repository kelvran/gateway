# RFC: Admin API viewer-role credential tier + audit logging

## Status

Accepted, implemented 2026-09-09.

## Context

`docs/upgrade-research/gateway-next-upgrade-2026-09-09.md`'s Finding 3, cross-referencing LiteLLM's and Kong's real hardening ladders for admin/control-plane surfaces against Kelvran's own code, found Kelvran's single flat bearer secret (`internal/admin/admin.go`'s `requireBearerToken` wraps the entire mux with one `subtle.ConstantTimeCompare` check) sits below even the free-tier rung comparable OSS gateways ship: LiteLLM's free/open-source RBAC includes a read-only `proxy_admin_viewer` role alongside full-access `proxy_admin`. Full OIDC/SAML Admin-UI SSO and org/team-scoped roles are both Enterprise-license-gated industry-wide (a business decision, not proof of technical necessity — see Caveats in the research doc) and are explicitly out of scope here; only the free-tier viewer/admin split is a fit to build now. Separately, direct inspection of `admin.go` confirmed neither `upsertVirtualKeyHandler` nor `deleteVirtualKeyHandler` writes any audit-log entry today — there is no logger dependency anywhere in the package at all.

## Design

`admin.Handler`'s signature widens from a single `token string` to a new `Credentials{Admin, Viewer string}` struct plus a `*slog.Logger` parameter. `Credentials.Admin` is required (same non-empty-before-construction contract the single token always had); `Credentials.Viewer` is optional — empty means no viewer tier is configured.

Route-level authorization is now per-route rather than one blanket mux wrap: `GET /admin/config` is wrapped by a new `requireEitherBearerToken(creds, next)` (accepts `Admin` OR, when non-empty, `Viewer`); `POST`/`DELETE /admin/virtual_keys/{name}` keep using the existing `requireBearerToken` with `creds.Admin` specifically — a viewer credential is structurally rejected by both write routes, never a runtime role check inside a shared handler. `bearerToken(r)` factors out the shared "extract and validate the `Authorization: Bearer <token>` header shape" logic both auth functions now use, so that check stays in exactly one place.

`controlplane.AdminConfig` gains `ViewerTokenEnv string`, parsed from a new optional `admin.viewer_token_env` YAML key, mirroring `TokenEnv`'s exact convention. `cmd/gateway/main.go` resolves a second `viewerToken` with the identical "set but resolves empty → refuse to start" guard `adminToken` already has, only when `ViewerTokenEnv != ""` — omitting it reproduces today's exact single-credential behavior with zero code-path change.

`upsertVirtualKeyHandler`/`deleteVirtualKeyHandler` both gain a `logger *slog.Logger` parameter and log `admin_virtual_key_upserted`/`admin_virtual_key_deleted` on success, with the virtual key's `name` and a static `authorized_by` field — never the presented credential or the key's hash.

## Alternatives considered

**A single `Handler` with an internal role-check inside each handler function** (checking which credential was presented and branching on write permission at the handler level) — rejected: per-route middleware wrapping keeps the authorization decision structurally separate from business logic, and makes it impossible for a future write route to be added without an explicit, visible `requireBearerToken(creds.Admin, ...)` wrap — a role check buried inside a handler body is easy to accidentally omit on a new handler.

**Full OIDC/SSO or org/team-scoped roles** — rejected for this pass per the research: both are Enterprise-tier features industry-wide, gated on either a deliberate product decision to monetize a premium tier or a genuine multi-human-operator need, neither of which exists today (the admin surface is explicitly single-secret, loopback-only, off-by-default, with zero recorded demand for multiple human operators).

## Verification

`internal/admin/admin_test.go`: all 12 pre-existing test call sites updated to the new `Handler(cfg, pipeline, Credentials{...}, logger)` signature (verified zero behavior change — every pre-existing test still passes unmodified in intent). New tests: `TestViewerTokenCanReadConfigButNotMutateVirtualKeys` (viewer reads config, 401s on both write routes), `TestAdminTokenStillWorksForEverythingWhenAViewerTierIsConfigured` (no regression when both tiers are configured), `TestOmittingTheViewerTierBehavesExactlyAsBefore`, and `TestUpsertVirtualKeyLogsAnAuditEntryWithoutLeakingTheSecret`/`TestDeleteVirtualKeyLogsAnAuditEntryWithoutLeakingTheSecret` (capture real `slog` output via a buffer-backed logger, assert the audit line and key name are present and the presented credential/key hash never appear in it). `cmd/gateway/admin_viewer_startup_test.go` (new file) proves the startup guards for real via `run()` itself, loading a real YAML config from a temp file — `TestRunRefusesToStartWithAdminTokenEnvSetButEmpty` (previously entirely untested for the pre-existing admin-token guard too), `TestRunRefusesToStartWithViewerTokenEnvSetButEmpty`, `TestRunAcceptsOmittingTheViewerTierEntirely`. Full `go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./... && go-arch-lint check && gofmt -l . && go mod tidy` clean except the same pre-existing, already-documented rootless-Docker environmental failures.
