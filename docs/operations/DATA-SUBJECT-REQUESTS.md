# Data Subject Requests — Operator Procedure

Added 2026-09-18, per `docs/upgrade-research/data-retention-right-to-erasure-2026-09-15.md`'s
Finding 3 ("mirrors OpenAI's/LLM Gateway's own accepted 'automatic-window-plus-manual-channel'
shape") and `docs/upgrade-research/ai-compliance-regulatory-readiness-2026-09-14.md`'s own
already-named-but-never-written pointer to this file.

This is a **manual, interim procedure**, not automated tooling — see `SECURITY.md`'s "Data
Retention & Right to Erasure" section for the disclosed retention windows and erasure mechanisms
each store actually has today. Kelvran itself is almost never the GDPR/CCPA-obligated party (that's
typically the organization operating a deployment); this document tells that operator's own staff
what Kelvran's software can and cannot do when a real data-subject request arrives.

## What Kelvran's own tooling can do today

- **Erase a specific cached response** (L1 exact-match + L2 normalized-match only — see the
  limitation below): `POST /admin/cache/erase` with the request's own defining fields
  (`virtual_key_id`, `model`, `messages`, and any of `temperature`/`max_tokens`/`response_format`/
  `prompt_id`/`prompt_version`/`prompt_label` the original request set). Requires the Admin
  credential. Returns `{"l1_found": bool, "l2_found": bool, "l3_skipped": true}` — `l3_skipped` is
  always `true`, named explicitly so this response can't be misread as confirming full erasure.
  **Corrected 2026-10-08**: two statements above have drifted. (1) "Requires the Admin credential" is
  too narrow: `POST /admin/cache/erase` is registered behind `requireAdminOrOperatorBearerToken`
  (`gateway/internal/admin/admin.go`), so when `admin.operator_token_env` is configured the Operator
  tier's token is accepted too — cache erase is one of exactly three Operator-tier write routes, with
  virtual-key rotate and deployment reweight (`OperatorTokenEnv` doc comment,
  `gateway/internal/gateway/controlplane/config.go`; shipped in `gateway/v0.14.0`). With no operator
  tier configured the route requires the admin token exactly as before. The audit line records which
  tier erased (`authorized_by`). See `docs/how-to/admin-api-rbac.md`. (2) The field list is
  incomplete. The request body embeds `adapter.ChatRequest` (`eraseCacheEntryRequest`, `admin.go`), and
  the L1/L2 keys are rebuilt from every key-bearing field (`Pipeline.EraseCacheEntry`,
  `gateway/internal/gateway/dataplane/dataplane.go`), so the erase call must re-supply whatever the
  original request set from: `model`, `messages`, `temperature`, `max_tokens`, `response_format`,
  `prompt_id`/`prompt_version`/`prompt_label`, `thinking_binding_mode`, and — on `main` since
  2026-10-08, not in `gateway/v0.17.0` — `tools` and `tool_choice` (`toolsFingerprint`, folded into
  `cache.Key`/`cache.NormalizedKey`; `gateway/changelog/unreleased.md`). A request that carried tools
  but is erased without them targets a different key and returns `l1_found: false, l2_found: false`.
  Separately, `end_user_id` (present since before `v0.17.0`) must be set to the original end-user
  value when the virtual key has `cache_scope_to_end_user` enabled; empty targets the tenant-scoped
  entry. See `docs/how-to/caching.md`.
- **Delete a virtual key entirely, INCLUDING its full budget-spend history**:
  `DELETE /admin/virtual_keys/{name}`. `budget.Tracker.Delete` removes both the live in-memory
  spend record and, when `budget.persist_path` is configured, the persisted bbolt record too
  (`boltstore.Store.Delete`) — a real, complete per-tenant erasure of this specific store, not just
  a going-forward key revocation. It does not retroactively erase that key's own past cache
  entries or audit-log lines (see limitations below) — those are separate stores with their own,
  narrower mechanisms.
  **Corrected 2026-10-08**: the store list above is incomplete. Since `gateway/v0.14.0` (2026-09-21) a
  third budget store exists: when `budget.redis_addr` is set, `budget.Tracker.Delete` does not touch
  bbolt at all — `redis_addr` wins over `persist_path` and a warning is logged (`BudgetConfig`,
  `gateway/internal/gateway/controlplane/config.go`; `newBudgetTracker`, `gateway/cmd/gateway/main.go`)
  — and instead calls `redisbudget.Backend.Delete`, which `DEL`s that key's spend, alert-bucket and
  warn-alert Redis keys in one call (`gateway/internal/budget/redisbudget/redisbudget.go`). The
  erasure is still complete for this store, and in this mode it is fleet-wide, because every replica
  reads the same Redis keys. See `docs/how-to/virtual-keys-and-budgets.md`.
- **Disable the admin audit log** going forward: set `admin.enable_audit_log: false` in
  `config.yaml`. This stops NEW entries from being written; it does not retroactively remove
  anything already logged (Kelvran writes to `slog`'s configured output, typically process
  stdout/stderr captured by whatever log-aggregation the operator runs — Kelvran itself never
  persists the audit log to a file or database it controls).
  **Corrected 2026-10-08**: "Kelvran itself never persists the audit log to a file" has been false
  since `gateway/v0.15.0` (2026-09-23). When `admin.audit_log_path` is set
  (`AdminConfig.AuditLogPath`, `gateway/internal/gateway/controlplane/config.go`), `cmd/gateway`
  opens that path with `auditstore.Open` and every admin audit event is ALSO appended as one JSONL
  record to that file (`gateway/internal/admin/auditstore`), queryable via `GET /admin/audit`
  (Admin or Viewer token; filters `msg`, `field`/`value`, `since`/`until`). The `slog` line still
  flows as before. Both paths are governed by the same `admin.enable_audit_log` switch, so setting
  it to `false` stops NEW entries in both; it still removes nothing already written. The JSONL file
  is a store under the operator's control that this document previously said did not exist: Kelvran
  never truncates or rotates it (`Open` uses `O_APPEND`; its doc comment says the file is never
  truncated) and `auditstore` exposes only `Open`/`Append`/`Query`/`Close` — no per-entry delete,
  so the "no per-entry deletion path" limitation below still stands. If `audit_log_path` is unset
  (the default), the original sentence remains accurate. See `docs/how-to/admin-api-rbac.md`
  ("Read the audit trail").

## Real limitations — read before promising a data subject anything

- **Cache L3 (lexical near-duplicate) has no erasure mechanism at all.** `POST /admin/cache/erase`
  deliberately does not cover it (no `Delete` method exists on the `LexicalCache` interface).
  A byte-identical or near-identical follow-up request for erased content can still be served from
  L3 until its own TTL (default 300s) expires. If a request is genuinely urgent, the interim
  workaround is restarting the gateway process (L3 is in-memory-only, never persisted) — a real but
  blunt instrument that also clears every OTHER tenant's L1/L2/L3 cache, not a targeted operation.
- **There is no way to "find every cached entry belonging to data subject X."** Kelvran's cache is
  keyed by a content hash of the request itself (model/messages/temperature/etc.), not a per-tenant
  index — you must already know the specific request(s) in question (e.g. from the subject's own
  account of what they sent, or from `GatewayDecisionEvent` records if event export is configured)
  to erase them. A write-time PII-provenance index that would enable this is real, named future
  work, not built.
- **Budget-spend history has no AUTOMATIC retention window** — absent a manual
  `DELETE /admin/virtual_keys/{name}` call, it persists indefinitely (when `budget.persist_path`
  is configured). Per-key deletion itself IS real and complete (see above); a configurable,
  automatic rolling-deletion window (independent of deleting the key entirely) is the part that
  remains real, disclosed future work, not yet built.
- **The admin audit log has no per-entry deletion path.** Only whole-log disablement
  (`admin.enable_audit_log: false`) exists — see `SECURITY.md`'s own disclosure of the Article
  17(3) legal-obligation/legal-claims basis an operator may (not automatically does) invoke for
  this store specifically, and why that determination is the operator's own to make.
- **None of this is retroactive across a fleet.** Every mechanism above acts on one gateway
  instance's own in-memory/local-disk state. A multi-instance deployment with Redis-backed rate
  limiting or config propagation still has per-instance caches — an erasure call to one instance
  does not reach any other instance's own L1/L2/L3 state.
  **Corrected 2026-10-08**: "every mechanism" is too broad since `gateway/v0.14.0` (2026-09-21). It
  still holds for `POST /admin/cache/erase` — `configpropagation` has no cache-erase event type, only
  `deployment_weight`, `virtual_key_upsert` and `virtual_key_delete`
  (`gateway/internal/configpropagation/configpropagation.go`). It no longer holds for
  `DELETE /admin/virtual_keys/{name}`: when `config_propagation.redis_addr` is set,
  `Pipeline.DeleteVirtualKey` publishes a `virtual_key_delete` event and every other replica's
  subscriber (`cmd/gateway`) applies it through `ApplyVirtualKeyDeleteFromEvent`, which runs the same
  `applyVirtualKeyDeleteLocked` body — including `budget.Tracker.Delete`
  (`gateway/internal/gateway/dataplane/dataplane.go`) — so one call removes the key and its budget
  record on every replica; with `budget.redis_addr` the budget record is a single shared store anyway.
  Delivery is pub/sub with no replay, so if the publish failed (`configpropagation_publish_failed`
  log line; the matching `kelvran.configpropagation.publish_failed` counter is on `main` since
  2026-10-08, not in `gateway/v0.17.0`) the operator still re-applies per instance exactly as before.

- **Identifiers on exported spans have no erasure path in Kelvran.** Since 2026-10-09 every request span can carry Claude Code's session, agent, parent-agent and prompt ids (and the client tool) beside the `agent_run_id` that was already there; they are exported through `telemetry.exporter` and retained by the tracing backend for as long as it keeps spans. Kelvran cannot delete a span it has exported. The levers are preventive: `attribution.capture_ids: false` (gateway-wide) or `attribution_capture_ids: false` on a key stops the Claude Code identifiers from being recorded (a request that fails authentication never carries them); `agent_run_id` has no switch. Erasure of an exported span is the backend operator's procedure. See `SECURITY.md`'s retention table.

## Recommended interim workflow for a real request

1. Confirm the requestor's identity and scope per your own organization's standard data-subject-
   request intake process (out of scope for this document).
2. If the request names specific prompts/responses: call `POST /admin/cache/erase` for each known
   request shape, on every gateway instance in the fleet. Record the `l1_found`/`l2_found`
   response for your own compliance record — remember `l3_skipped` is always `true`.
3. If the request is "delete my account"/tenant-scoped: call `DELETE /admin/virtual_keys/{name}`
   on every gateway instance in the fleet — this fully erases that key's own budget-spend history
   (live and persisted), not just the key's ability to authenticate going forward.
   **Corrected 2026-10-08**: "on every gateway instance in the fleet" is unconditional here but has
   not been since `gateway/v0.14.0` (2026-09-21). With `config_propagation.redis_addr` set, one call
   reaches every replica (see the fleet limitation above); a repeat call against a replica that
   already applied the propagated delete returns `404` (`ErrVirtualKeyNotFound`,
   `gateway/internal/admin/admin.go`), which here means "already erased", not failure. "Live and
   persisted" covers bbolt or Redis, whichever `budget:` store is configured. Without config
   propagation, or after a `configpropagation_publish_failed` line, call it on every gateway instance
   exactly as written.
4. Document, in your own organization's own records (not Kelvran's), that L3 cache entries for this
   subject's own recent requests may still exist per the limitations above, and note when they will
   naturally expire (L3's own TTL, default 300s) — this is the one real remaining gap step 2/3
   above don't close.
