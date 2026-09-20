# RFC: Admin API risk-tiered `Operator` role

- **Status**: Accepted, implemented 2026-09-20 (same day this RFC was written). `internal/admin.Credentials` gained the `Operator` field; `requireAdminOrOperatorBearerToken` gates the three routes named below; `controlplane.AdminConfig.OperatorTokenEnv` (`admin.operator_token_env`) wires it through `cmd/gateway/main.go`'s existing startup-guard convention. `THREAT_MODEL.md`'s Gateway Elevation-of-Privilege row has its own matching "Implemented, same day" correction.
- **Date**: 2026-09-20
- **Author(s)**: Session agent (Claude Code), per `docs/upgrade-research/multitenancy-rbac-tier1-2026-09-20.md` Finding 5, and `THREAT_MODEL.md`'s Gateway Elevation-of-Privilege row's own 2026-09-20 correction, which names this exact RFC as its own recommended follow-up.

## Summary

Add a fourth, narrower admin credential tier — `Operator` — sitting between the existing read-only `Viewer`/`CostViewer` tiers and full `Admin`. `Operator` authenticates the subset of write routes that are reversible and scoped to a single named resource (rotate one virtual key, reweight one deployment, erase one cache entry). Every other write route — anything irreversible, cross-tenant, or supply-chain-adjacent (virtual-key create/delete, prompt upsert/delete/promote/rollback, backup, pprof) — stays `Admin`-only. `Admin` continues to satisfy every `Operator`-gated route too (a strict superset), so no existing single-admin-token deployment needs any config change to keep working exactly as today.

## Motivation

`gateway/internal/admin/admin.go`'s `Credentials{Admin, Viewer, CostViewer}` today has exactly one write-capable tier: `Admin`. Every write route on the mux — regardless of how reversible or how narrow its blast radius — is gated by the identical `requireBearerToken(creds.Admin, ...)` call. That list has grown substantially since the tier model itself was last reviewed (`docs/rfcs/2026-09-09-gateway-admin-viewer-role.md`, which only ever reasoned about `POST`/`DELETE /admin/virtual_keys/{name}`):

| Route | Added | Blast radius if abused |
|---|---|---|
| `POST /admin/virtual_keys/{name}` | v1 | Creates an unrestricted (no budget cap, no model allow-list) virtual key — `THREAT_MODEL.md`'s own long-standing named risk |
| `DELETE /admin/virtual_keys/{name}` | v1 | Removes a tenant's access entirely — irreversible without re-provisioning |
| `POST /admin/virtual_keys/{name}/rotate` | 2026-09-14 | Rotates one key's credential, with an optional grace period — reversible, single-resource |
| `POST /admin/deployments/{name}/weight` | 2026-09-11 | Can silently redirect all traffic for a model to a chosen deployment — reversible (re-set the weight), single-resource |
| `POST /admin/prompts/{id}` / `DELETE /admin/prompts/{id}` | 2026-09-13 | Prompts are deliberately GLOBAL, not tenant-scoped (`docs/rfcs/2026-09-13-gateway-prompt-management.md`) — affects every virtual key's resolved prompts at once |
| `PUT`/`DELETE /admin/prompts/{id}/labels/{label}` | 2026-09-18 | Production-prompt promote/rollback — a supply-chain-into-prompts vector, per the research finding's own framing |
| `POST /admin/backup` | 2026-09-17 | Exports every virtual key's hash plus budget/spend state across the **entire** deployment to disk — not scoped to any one key |
| `POST /admin/cache/erase` | 2026-09-05 | A DoS/anti-forensic primitive — can also cover tracks after another abuse, per `THREAT_MODEL.md`'s Cache Repudiation row |
| `GET /admin/debug/pprof/*` | 2026-09-14, opt-in | Unscoped scrape of process memory, including plaintext upstream API keys/session tokens held as Go strings — a data-plane secret-disclosure primitive, categorically worse than a control-plane one |

`THREAT_MODEL.md`'s Gateway Elevation-of-Privilege row, corrected the same day as this RFC (see its own "**Corrected 2026-09-20**" paragraph), names this exact list and states plainly: "a future admin-RBAC risk-tiering pass... should scope against this full list, not just virtual-key minting." This RFC is that pass.

The real cost of the current flat model: an operator who wants to grant a routine, low-risk, frequently-needed capability — say, an on-call engineer rotating a leaked virtual-key credential, or a deploy pipeline nudging a canary deployment's weight — has no way to do so without handing out the SAME credential that can also exfiltrate a full cross-tenant backup or silently drop a production prompt's schema enforcement. Every credential distributed for a routine task inherits the full blast radius of the most dangerous route on the mux.

Two independent vendors, both verified live against their own current docs this same research round, confirm a narrow, named-route credential split — not a full policy engine, not an org/tenant hierarchy — is the real industry granularity ceiling for exactly this class of problem: Kong Gateway's RBAC grants permissions via an `(endpoint, workspace, actions)` 3-tuple; Cloudflare's API tokens are scoped to one resource plus one action level. Kelvran has already built one instance of this pattern itself — `CostViewer` (2026-09-15), which authenticates exactly one route and nothing else. This RFC is the next application of an already-proven pattern in this codebase, not a novel design.

## Detailed Design

### Tier assignment

**`Operator`** (new) — reversible, single-named-resource writes:
- `POST /admin/virtual_keys/{name}/rotate`
- `POST /admin/deployments/{name}/weight`
- `POST /admin/cache/erase`

**`Admin`** (existing, semantics narrow to "irreversible, cross-tenant, or supply-chain/secret-disclosure-adjacent"):
- `POST`/`DELETE /admin/virtual_keys/{name}` (create can mint an unrestricted budget; delete is not undoable without re-provisioning)
- `POST`/`DELETE /admin/prompts/{id}`, `PUT`/`DELETE /admin/prompts/{id}/labels/{label}` (global blast radius across every tenant, plus the promote/rollback supply-chain angle)
- `POST /admin/backup` (full cross-tenant export)
- `GET /admin/debug/pprof/*` (secret-disclosure primitive; kept at the highest tier deliberately, not demoted to `Operator` despite "read-only," since it also is not "single-resource-scoped" — it exposes the whole process)

**`Viewer`**/`CostViewer`: unchanged. `Operator` is a peer to `Viewer`, not a superset or subset of it — an `Operator` credential is write-capable on its 3 routes but has no read access to `GET /admin/config`/`GET /admin/prompts`/`GET /admin/virtual_keys` beyond what those write routes themselves return in their response bodies (e.g., the rotated key's own new state).

### Why this exact two-way split, not N tiers

The research grounding this RFC deliberately recommends a two-tier split (`Operator` vs. narrowed `Admin`), not a finer-grained per-route policy engine — matching what both cited vendor precedents actually ship (a flat allowlist tuple, not a graph of scopes) and what `CostViewer`'s own precedent already established for this codebase. A richer capability/claims-based token model is explicitly **not** proposed here; see Alternatives Considered.

### Implementation shape

Mirrors `docs/rfcs/2026-09-09-gateway-admin-viewer-role.md`'s own established pattern exactly:

- `Credentials` gains `Operator string` (optional, empty means unconfigured — same convention as `Viewer`/`CostViewer`).
- `controlplane.AdminConfig` gains `OperatorTokenEnv string`, parsed from `admin.operator_token_env`, mirroring `ViewerTokenEnv`'s exact parsing/startup-guard convention (`cmd/gateway/main.go` refuses to start if the env var is set but resolves empty).
- A new middleware, `requireEitherAdminOrOperatorBearerToken(creds, next)`, mirroring `requireEitherBearerToken`'s exact shape (constant-time compare against `creds.Admin` OR, when configured, `creds.Operator`) — **not** a third bespoke function reusing `requireAnyBearerToken`'s tier-list approach, since that helper's contract (used by `CostViewer`, which authenticates alongside `Admin`/`Viewer` on one specific read route) is subtly different: it needs to distinguish which of N tiers authenticated for audit-logging purposes, whereas this is the same two-tier "narrow-or-full" shape the existing `Viewer` middleware already handles. The three `Operator`-tier routes swap their `requireBearerToken(creds.Admin, ...)` wrap for `requireEitherAdminOrOperatorBearerToken(creds, ...)`.
- Every route that stays `Admin`-only keeps `requireBearerToken(creds.Admin, ...)` byte-for-byte unchanged — an `Operator`-only credential is structurally rejected by all of them, never a runtime role check inside a shared handler (matching the 2026-09-09 RFC's own "per-route middleware wrapping, not an internal branch" decision, and its own explicit Alternatives-Considered rejection of the internal-branch approach).
- Audit logging: every affected route already emits a structured audit-log entry via the existing `auditLog` helper; that helper's `authorized_by`/credential-tier field gains an `"operator"` value alongside its existing `"admin"`/`"viewer"`/`"cost_viewer"` values, so a security review can distinguish which tier actually performed a given rotate/reweight/erase after the fact.

### Backward compatibility / migration

Zero required change for any existing deployment. `OperatorTokenEnv` is optional and defaults to unconfigured; when unconfigured, `creds.Operator == ""`, and `requireEitherAdminOrOperatorBearerToken` degrades to accepting `Admin` only — byte-for-byte the current behavior. An operator who wants the narrower tier opts in by setting `admin.operator_token_env` and provisioning a second secret, exactly like adopting `Viewer` or `CostViewer` today. No route moves OFF `Admin`'s authority — `Admin` remains a strict superset of every tier, so a single-admin-token deployment loses no capability by this RFC shipping.

## Drawbacks

- A fourth credential tier is one more secret an operator running the full tier ladder (`Admin`/`Viewer`/`CostViewer`/`Operator`) must provision, rotate, and store — real operational overhead, though opt-in and additive.
- The reversibility-based line is a judgment call, not a formal property Kelvran's code can verify automatically (e.g., `POST /admin/deployments/{name}/weight` is "reversible" only in the sense that a human can re-set the weight — it does not undo whatever cost/latency impact live traffic experienced in between). This RFC's own line-drawing (see Detailed Design) is disclosed as a judgment call, matching Open Question 2 the grounding research itself already flagged as unresearched due to no real incident history yet.
- `Operator`, once shipped, is one more axis `THREAT_MODEL.md`'s Elevation-of-Privilege row and every future admin-route addition must remember to classify — the same "which tier does a NEW route belong to" question the existing `Admin`/`Viewer` split already requires discipline about, now with one more option to get wrong.

## Alternatives Considered

**A full capability/claims-based token** (e.g., a signed JWT enumerating an arbitrary subset of allowed routes per credential) — rejected for this pass: neither cited vendor precedent (Kong, Cloudflare) nor Kelvran's own `CostViewer` shipped anything beyond a flat, named tier; building a general claims engine for a 9-route admin surface with a single operator today is speculative scope with no demand signal, the same "not a full policy engine" judgment the grounding research itself makes explicitly.

**Splitting by data-sensitivity instead of reversibility** (secrets/backup-adjacent vs. everything else) — considered, not chosen: the grounding research names this as a live, genuinely open alternative axis (Open Question 2) and explicitly says Kelvran doesn't yet have enough real incident/support history to know which axis matters more in practice. Reversibility was chosen here as the more concrete, checkable property of the two, not because the alternative was refuted.

**Doing nothing (leave the flat `Admin` tier as-is)** — rejected: `THREAT_MODEL.md`'s own already-corrected framing already treats the current state as understated risk, not an accepted one; the fix is cheap (an additive, opt-in credential tier, no route removed from `Admin`) relative to the blast-radius gap it closes.

**Per-virtual-key or per-tenant-scoped admin credentials** (a `tenant_id`-scoped token that can only mutate ONE virtual key) — out of scope here: this is the genuinely deferred multi-tenancy/RBAC question (`docs/upgrade-research/gateway-enterprise-multitenancy-2026-09-11.md` Finding 4, reconfirmed still NOT YET by this same day's research pass) — a different axis (which tenant) than this RFC's axis (which operation), and gated on a real customer trigger that has not fired. This RFC's `Operator` tier is global across all tenants, exactly like `Admin`/`Viewer`/`CostViewer` already are.

## Unresolved Questions

- Should `POST /admin/deployments/{name}/weight` require the `Operator` credential to also present the deployment `name` as part of some future finer-grained per-resource scoping, or is "any named deployment" an acceptable grant at this tier? This RFC proposes the latter (matching `CostViewer`'s own precedent of a route-level, not resource-level, grant) but does not close the door on resource-level scoping if a real future need for it appears.
- ~~Once implemented, `THREAT_MODEL.md`'s Elevation-of-Privilege row needs a follow-up update crediting the new tier split~~ — **done**, same day: see that row's own "Implemented, same day" correction.
- Should `Operator` be able to read the routes it can write to (e.g., `GET /admin/deployments` if such a route existed) without a separate `Viewer` credential also being presented? No such GET route currently exists for deployments, so this is moot today, but worth resolving before/if one is added.
