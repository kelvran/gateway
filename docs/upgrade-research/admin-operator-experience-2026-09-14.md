# Admin/Operator Experience — Upgrade Research (2026-09-14)

Recovered by hand from the deep-research workflow's own returned JSON — its file-write step failed
silently, a known recurring class per `AGENTS.md`'s Gotchas section. Reconstructed from the
workflow's `summary`/`findings` fields, not re-run.

## Scope

Kelvran's admin/operator experience: dashboard content, virtual-key lifecycle UX, audit-trail
consumption, config hot-reload. Compared against 2026 production practice in LiteLLM Proxy's Admin
UI, Portkey, Helicone, OpenRouter's dashboard, and Cloudflare AI Gateway.

## Corrected baseline

A direct re-read of `gateway/ARCHITECTURE.md`, `DECISIONS.md`, and `THREAT_MODEL.md` (done as part
of this research pass) found Kelvran's admin API is more mature than this research's own task brief
assumed: virtual-key create/delete is already a real, live-mutable admin-API feature
(`POST`/`DELETE /admin/virtual_keys/{name}`, since 2026-09-05), not just config-read + prompt-CRUD.
Whether Kelvran needs a bespoke UI at all is separately settled by a sibling same-day report
(`docs/upgrade-research/admin-dashboard-ui-2026-09-14.md`: `not_yet`) and is not re-litigated here —
every finding below is an admin-API-level content/functionality gap that exists independent of any
future UI.

## Findings

### 1. No dedicated key-rotation primitive — `build_now`

Kelvran's admin API supports virtual-key create/delete
(`gateway/internal/admin/admin.go`, via `dataplane.Pipeline.UpsertVirtualKey`/`DeleteVirtualKey`
swapping an atomic `Pointer[identity.Verifier]`) but has no rotate/regenerate primitive — rotation
today is a manual delete-then-recreate sequence with no continuity. LiteLLM's Enterprise-gated
`/key/{key}/regenerate` supports a `grace_period` so old and new keys both work during cutover;
OpenRouter exposes full key CRUD (including PATCH/DELETE) via a REST Management API. LiteLLM's
OSS/free tier already ships basic self-service virtual-key creation, budgets, teams, and per-key
rate limits without a paid add-on, so plain create/delete (what Kelvran has) is table stakes, and a
rotate-with-continuity primitive is the next, currently-missing rung — not scope creep.

**Verdict: build_now.** A `POST /admin/virtual_keys/{name}/rotate` (optionally with a grace-period
param) is a small, low-risk addition reusing the existing atomic-swap mechanism.

### 2. Admin-mutated state is in-memory-only, lost on restart — `build_now`

Virtual keys, and separately prompts, are explicitly in-memory-only in v1 — a disclosed limitation
named in `gateway/ARCHITECTURE.md` for both subsystems; a restart reverts everything to
`config.yaml`. LiteLLM's production-validated pattern for exactly this problem is a scoped DB-backed
deep-merge overlay (`general_settings`/`router_settings`/`litellm_settings`/`environment_variables`,
DB value always winning), not a full config replacement. Kong's alternative (`/config` full
in-memory replace via DB-less mode) has a real, documented downside: zero cluster coordination, so
each node must be updated independently — a materially worse fit for Kelvran if it ever runs more
than one gateway instance.

**Verdict: build_now**, via a scoped persistence overlay rather than a full-config-replace
mechanism (see the implementation plan's Phase 4/5 for the concrete bbolt-based design, corrected
away from this research's own Redis assumption after direct code grounding).

### 3. Structured audit log has no query surface

Kelvran's shipped structured audit log (every Admin API READ route naming the authenticating
credential tier, since 2026-09-11) is already ahead of LiteLLM OSS, which hard-gates audit logging
behind Enterprise licensing. But AWS's own Well-Architected guidance treats log *querying* as a
separate, required capability from log *emission* — Kelvran has the latter with no built-in way to
do the former beyond scanning raw log lines.

**Verdict:** real gap, not scoped into this implementation round's four `build_now` items — noted
for a future pass.

### 4. No live cost/budget view — `build_now` (folded into the FinOps report)

There's no way to see what a specific virtual key has spent without full admin config-read access.
This maps directly to a gap `THREAT_MODEL.md` already discloses ("why did this agent run cost $X"
has no answer beyond a raw total). Closed in this round's Phase 2 by pairing a new narrow RBAC tier
with one minimal `GET /admin/virtual_keys/{name}/spend` endpoint.

## Sources

- https://docs.litellm.ai/docs/proxy/virtual_keys
- https://docs.litellm.ai/docs/proxy/master_key_rotations
- https://openrouter.ai/docs/features/provisioning-api-keys
- https://docs.litellm.ai/docs/proxy/configs
- https://developer.konghq.com/gateway/db-less-mode/
- `gateway/internal/admin/admin.go`, `gateway/ARCHITECTURE.md`, `DECISIONS.md` `[2026-09-04]`,
  `[2026-09-05]`, `[2026-09-11]`, `THREAT_MODEL.md` (Spoofing, Elevation of Privilege rows)
