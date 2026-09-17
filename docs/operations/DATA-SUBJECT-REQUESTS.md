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
- **Delete a virtual key entirely, INCLUDING its full budget-spend history**:
  `DELETE /admin/virtual_keys/{name}`. `budget.Tracker.Delete` removes both the live in-memory
  spend record and, when `budget.persist_path` is configured, the persisted bbolt record too
  (`boltstore.Store.Delete`) — a real, complete per-tenant erasure of this specific store, not just
  a going-forward key revocation. It does not retroactively erase that key's own past cache
  entries or audit-log lines (see limitations below) — those are separate stores with their own,
  narrower mechanisms.
- **Disable the admin audit log** going forward: set `admin.enable_audit_log: false` in
  `config.yaml`. This stops NEW entries from being written; it does not retroactively remove
  anything already logged (Kelvran writes to `slog`'s configured output, typically process
  stdout/stderr captured by whatever log-aggregation the operator runs — Kelvran itself never
  persists the audit log to a file or database it controls).

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

## Recommended interim workflow for a real request

1. Confirm the requestor's identity and scope per your own organization's standard data-subject-
   request intake process (out of scope for this document).
2. If the request names specific prompts/responses: call `POST /admin/cache/erase` for each known
   request shape, on every gateway instance in the fleet. Record the `l1_found`/`l2_found`
   response for your own compliance record — remember `l3_skipped` is always `true`.
3. If the request is "delete my account"/tenant-scoped: call `DELETE /admin/virtual_keys/{name}`
   on every gateway instance in the fleet — this fully erases that key's own budget-spend history
   (live and persisted), not just the key's ability to authenticate going forward.
4. Document, in your own organization's own records (not Kelvran's), that L3 cache entries for this
   subject's own recent requests may still exist per the limitations above, and note when they will
   naturally expire (L3's own TTL, default 300s) — this is the one real remaining gap step 2/3
   above don't close.
