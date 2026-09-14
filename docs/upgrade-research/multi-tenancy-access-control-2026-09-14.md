# Multi-Tenancy & Access-Control Model — Deep Research (2026-09-14)

## Question

Comparing Kelvran gateway's current multi-tenancy/access-control model against 2026 production
practice at LiteLLM Proxy, Portkey, Helicone, and Domino's LLM Gateway: (1) is a flat virtual-key
model a real gap at Kelvran's stated scale; (2) is admin/viewer RBAC too coarse; (3) is there a
low-effort SSO/OIDC pattern for the Admin API specifically; (4) are per-tenant budget
rollups/hierarchies a real gap; (5) are tenant-level cost dashboards a real gap; (6) anything else
a 2026 survey flags. This is at minimum the **third** research pass to touch this territory —
`docs/upgrade-research/gateway-enterprise-multitenancy-2026-09-11.md` (RBAC/SSO/audit/tenant-scoped
admin credentials) and `docs/upgrade-research/gateway-hierarchical-budgets-2026-09-07.md` (org→team→
user→key→session cascading budgets, twice-deferred, reaffirmed a third time 2026-09-08) already
cover large parts of this ground in depth. This pass's job is to extend that record with genuinely
new evidence, not re-litigate settled verdicts.

**Kelvran ground truth referenced** (do not re-verify — already real, per `PRD.md`, `THREAT_MODEL.md`,
`gateway/ARCHITECTURE.md`): tenant namespace baked into the cache partition itself, enforced at
lookup/write/retry/fallback/connection-pool-reuse (`gateway/ARCHITECTURE.md` lines 469-477,
`THREAT_MODEL.md`'s Cache STRIDE table); a flat virtual-key model (`PRD.md` line 51: "exact
virtual-key/budget data model... not finalized") with admin/viewer as the only two Admin API
credential tiers; per-key RPM/TPM (`internal/ratelimit`) and per-key USD budget caps
(`internal/budget`); `DeploymentConfig.SharedAcrossTenants` force-disabling provider-side
prompt-cache auto-populate for pooled upstream credentials.

## Executive Summary

Nothing in this pass overturns either prior deferral — org/team hierarchy above a flat virtual key,
per-tenant budget rollups, and Admin-API SSO/OIDC all remain **NOT YET** for Kelvran, each still
missing the same named trigger (a real multi-tenant demand signal or a specific enterprise
procurement ask) that two and three prior passes already established. What this pass adds is
sharper texture on three fronts: Portkey's actual hierarchy shape is Account→Organization→Workspace
(not org→workspace→team as an earlier, now-refuted claim framed it), and — like every other
surveyed vendor — the grouping layer above a flat key is Enterprise-tier-gated, never a free
baseline; LiteLLM's OSS/free tier already ships a small, real role catalog including a genuine
read-only `proxy_admin_viewer`, reinforcing (not creating) the prior pass's recommendation that a
narrow, fixed-permission third RBAC tier is closer to build-now than previously scoped work; and
Domino's LLM Gateway offers a third, materially different concurrency/design pattern for budget
enforcement — strict-priority first-match-wins across isolated tiers, explicitly rejecting rollup
semantics — worth filing alongside Bifrost's and LiteLLM's already-collected patterns for if/when
hierarchical budgets are ever triggered. Tenant-level cost dashboards (question 5) surfaced zero
surviving claims this round and remain a genuinely open gap in the research record, not a settled
"no."

## Findings

### Finding A — Org/team hierarchy above a flat virtual key is real 2026 precedent, but universally Enterprise-tier-gated
**Confidence: high** (3 unanimous primary-source claims: [1], [2], [13])

Portkey's real hierarchy is **Account → Organization → Workspace**, where each Organization can
hold multiple Workspaces (which in turn hold Teams/Managers/Members and Workspace API Keys) — but
Portkey's own docs state plainly that "Workspaces is currently a feature only enabled on the
Enterprise Plans," with lower tiers (Open Source, Developer, Production) getting no workspace
subdivision at all. LiteLLM's real hierarchy is **Organizations → Teams → Users → Keys** (four
nested levels, each a spend-attribution and isolation boundary) — but LiteLLM's own docs flag
Organizations and org-scoped admins as themselves an Enterprise feature, so the OSS-available
hierarchy tops out at Teams → Users → Keys (three levels), not four. Two adjacent, more specific
claims about this territory were explicitly **refuted** at 0-3 this round and should not be relied
on: that LiteLLM ships a full "three-level key/team/org model... not a flat virtual-key model" as
an unqualified capability, and that Portkey's hierarchy is ordered org→workspace→team (the verified
order is Account→Org→Workspace, with Team nested inside Workspace, not a peer of it).

**Verdict: NOT YET.** This directly extends, and does not contradict, the 2026-09-11 research's
Finding 4 verdict and the three-times-reaffirmed hierarchical-budgets deferral
(`gateway-hierarchical-budgets-2026-09-07.md`). Named trigger, unchanged: Kelvran itself becomes
multi-tenant in practice — a customer with its own sub-customers/teams wanting self-serve
scoped key management — or a specific enterprise procurement ask. Every vendor surveyed across all
three research passes now agrees this grouping layer is monetization-tier functionality, not a v1
baseline; none ships it free at real scale.

### Finding B — A lightweight third RBAC tier is free-tier-precedented, not enterprise-only; org/team-scoped admin roles remain enterprise-gated
**Confidence: high** (3 unanimous claims: [3], [7]; reinforces existing 2026-09-11 Finding 2)

LiteLLM's **OSS/free tier** ships four global roles today — `proxy_admin` (full admin),
`proxy_admin_viewer` (read-only platform access, cannot create/delete keys or add users),
`internal_user` (standard self-service), and a deprecated-but-still-shipped
`internal_user_viewer` — meaning a genuine read-only tier below full admin is not itself an
enterprise concept, contrary to one closely-related claim ("a dedicated read-only/auditor role...
named `proxy_admin_viewer`, explicitly targeted at finance/audit stakeholders") that was refuted
0-3 for overreaching on *why* it exists, even though the role itself is real. Enterprise-only,
confirmed separately: `org_admin` and `team_admin` scoped roles plus a `team_member_permissions`
layer that restricts default team members to a fixed read-only endpoint allowlist (`/key/info`,
`/key/health` by default, configurable up to `/key/generate`/`/key/delete`) — described at the
marketing layer as "delegated admin roles" layered on an Organizations→Teams→Projects→Keys
hierarchy. A related claim that this cascades into *budget* enforcement (blocking a request if any
ancestor level is over budget) did **not** survive verification this round (1-2 split) — treat any
assumption that LiteLLM's RBAC hierarchy and its budget-cascade behavior are the same mechanism
with caution.

**Verdict: split.** A narrow, fixed-permission third credential tier — something between Viewer and
full Admin, e.g. can rotate keys and read audit logs but not touch budget policy or model
allowlists — is **BUILD-NOW-ADJACENT**, unchanged from the 2026-09-11 recommendation, and now
additionally corroborated by a real free-tier precedent (LiteLLM's own `proxy_admin_viewer`/
`internal_user` split ships without a license). Trigger, unchanged: the first support/ops hire or
automation that needs API access broader than Viewer but narrower than full Admin. Org/team-scoped
admin roles (`org_admin`/`team_admin`) remain **NOT YET** — enterprise-tier-gated everywhere
surveyed, and moot for Kelvran regardless until Finding A's own trigger fires (there is no
org/team concept yet to scope a role to).

### Finding C — Admin-plane SSO/OIDC is enterprise-tier-gated everywhere surveyed; the low-effort default is bearer/master-key auth with SSO as an additive, not a replacement
**Confidence: high** (8 claims converging: [0], [5], [6], [8], [9], [10], [11], [12])

Both LiteLLM and Portkey gate SSO/OIDC/SAML behind their paid Enterprise tier, packaged alongside
SCIM provisioning, audit logs, and RBAC as a named line item — Portkey's own docs state SSO
protocols are supported "for enterprise customers" specifically, and its public pricing page shows
SSO absent from Open Source, Developer, and Production tiers. LiteLLM's default, low-effort Admin
UI auth is master-key/bearer-token-adjacent: it requires the proxy master key to be set, falls back
to a username/password pair sourced from `UI_USERNAME`/`UI_PASSWORD` env vars (defaulting to the
master key itself if `UI_PASSWORD` is unset, per live source inspection of `login_utils.py`), and
keeps that path live at a documented `/fallback/login` route even after SSO is enabled — SSO is
architecturally additive, never a hard replacement, for exactly the reason a low-effort default
needs to remain: something must still work if the IdP integration breaks. The one real, low-effort
carve-out found: LiteLLM has shipped SSO free for up to 5 billable users since v1.76.0 (2025-08),
still enforced on current `main` — a genuine small-scale exception, not evidence that SSO
integration work itself (OIDC/SAML federation with a real IdP — Okta, Azure AD, Google Workspace,
or generic OIDC — plus a separate JWT mode for data-plane requests) is inherently cheap to build.

**Verdict: NOT YET.** Reaffirms the 2026-09-11 research's Finding 1 verdict outright. This pass
refines the trigger rather than changing it: build when a specific customer's compliance/procurement
process requires centralized SSO (unchanged), **or** when Kelvran's own admin headcount exceeds
what's practical to manage as individual bearer tokens (new, but currently unmet — Kelvran has no
evidence today of approaching even LiteLLM's 5-user free-tier threshold). Bearer-token-only Admin
API auth remains the correct low-effort default until one of those triggers fires; there is no
vendor-precedented "cheap SSO" shortcut that skips real IdP-integration engineering.

### Finding D — Domino's budget model explicitly rejects rollups in favor of isolated, priority-ordered caps — a third distinct concurrency pattern, not new demand evidence
**Confidence: medium** (1 claim, 2-1 vote: [4])

Domino's LLM Gateway evaluates five grant tiers (User > Organization > Service-account > All-users
> BYOT — Bring-Your-Own-Token) in strict priority order with first-match-wins semantics, and states
explicitly: "User budgets do not roll up into org budgets, and do not fall back to them." A
documented worked example (a user capped at $200 is denied even though her org has $5k of headroom)
frames this as deliberate design intent, not an accidental limitation, with a documented workaround
(cloning the alias into separate shared-pool vs. exception-user paths) for operators who want
different behavior. This is a third, materially different pattern from the two already collected
in `gateway-hierarchical-budgets-2026-09-07.md`: Bifrost/LiteLLM both check *every* applicable
hierarchy level independently and block if *any* one fails (closer to a true cascade, even without
being a single atomic transaction), whereas Domino picks exactly *one* matching tier and ignores the
rest entirely — an override model, not a cascade model.

**Verdict: NOT YET** — unchanged. This is new "how" evidence, not a new demand signal, so it does
not overturn the deferral that has now been reaffirmed three times (`DECISIONS.md`, 2026-09-07 x2,
2026-09-08). Worth filing directly alongside the existing Kelvran-specific implementation-design
notes in `gateway-hierarchical-budgets-2026-09-07.md` for if/when a real multi-tenant demand signal
finally fires — Kelvran would then have three distinct, evidence-backed shapes to choose from
(independent-per-level-block-if-any-fails, reserve-then-settle-per-level, or
priority-ordered-single-match), not just two.

### Finding E — Tenant-level usage/cost dashboards beyond aggregate reporting: no surviving evidence this round
**Confidence: n/a — open gap, not a finding**

No claim addressing research question 5 (tenant-level cost/usage dashboards beyond aggregate
reporting) survived 3-vote verification this round. This is a genuine gap in the research record,
not a "checked and found nothing wrong" result — it should not be read as evidence that Kelvran's
current aggregate-only reporting is sufficient, only that this pass did not produce verified
evidence either way.

## Caveats

- Vendor licensing/pricing facts cited here (LiteLLM's 5-user SSO threshold, Portkey's
  Enterprise-only Workspaces, Domino's grant-tier priority order) are 2026 snapshots of current
  commercial policy, consistent with the same caveat already logged in
  `gateway-enterprise-multitenancy-2026-09-11.md`; the durable takeaway is the *pattern*
  (grouping/SSO/fine RBAC monetized, not free), not the exact numbers or thresholds.
- The claim that LiteLLM's hierarchy cascades budget enforcement across ancestor levels (blocking
  on any ancestor being over budget) did **not** survive verification (1-2 split) — do not conflate
  LiteLLM's RBAC hierarchy (real, confirmed) with a confirmed budget-cascade mechanism (unconfirmed)
  when reasoning about precedent for Kelvran's own deferred hierarchical-budgets question.
  `gateway-hierarchical-budgets-2026-09-07.md` remains the authoritative source on budget-cascade
  mechanics specifically (Bifrost/LiteLLM's independent-per-level-counter finding stands unchanged).
- Cloudflare AI Gateway and Higress remain unresearched for RBAC/SSO/tenancy — an open question
  carried forward unchanged from the 2026-09-11 pass; this round did not add coverage of either.
  Helicone (named in this round's research brief) also produced zero surviving claims.
  Zero surviving claims ≠ zero relevant precedent; simply unresearched to a verified standard.
- Question 5 (tenant dashboards) is a genuine open gap, not a checked negative — see Finding E.

## Open Questions

1. What's the real threshold (ARR, contract stage, or a specific compliance mandate) at which
   Kelvran's actual prospective customers would require SSO or org/team hierarchy in practice — is
   LiteLLM's 5-seat free-tier cutoff a useful proxy for Kelvran's own admin headcount, or is it
   purely a function of a customer's own IT policy regardless of Kelvran's size? (carried forward
   from `gateway-enterprise-multitenancy-2026-09-11.md`)
2. If/when the build-now-adjacent third RBAC tier (Finding B) is built, should its permission set be
   a literal copy of LiteLLM's default `team_member_permissions` allowlist shape (`/key/info`,
   `/key/health` plus opt-in grants), or does Kelvran's own Admin API surface (audit-logged reads
   and writes, per-key budget/rate-limit/model-allowlist policy) suggest a different minimal set?
3. Tenant-level usage/cost dashboards beyond aggregate reporting (research question 5) — genuinely
   unanswered this round; needs a dedicated, focused research pass rather than folding into a
   broader multi-tenancy sweep.
4. Given three now-distinct vendor concurrency/design patterns for tiered budget enforcement
   (Bifrost/LiteLLM's independent-per-level-block, LiteLLM's session-level reserve-then-settle, and
   Domino's priority-ordered single-match override) — if Kelvran's hierarchical-budgets deferral is
   ever triggered, which pattern best composes with the existing flat `budget.Tracker.Record`/
   `Reserve`/`Reconcile` primitives without a redesign? Not answered by any research pass to date.
