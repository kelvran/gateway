# Gateway Enterprise Multi-Tenancy Research (2026-09-11)

Scope: enterprise-grade multi-tenancy (RBAC/SSO, tenant-scoped policy at scale, audit/compliance)
for Kelvran gateway's NEXT-VERSION (v2) upgrade. Baseline: a flat 2-tier bearer-token Admin API
(Admin full read/write, optional Viewer read-only), audit-logging on every admin write AND read
(route + credential tier, never secret values), CompareAndSwap-based concurrent-safe virtual-
key/prompt mutation, per-virtual-key budget/rate-limit/model-allowlist policy, and
`THREAT_MODEL.md`'s own accepted-risk framing that a compromised admin credential is a full
privilege escalation. Explicitly not built: RBAC beyond the 2-tier model, SSO/OIDC/SAML,
per-tenant-scoped admin credentials, formal audit-log retention/export/SIEM integration.
Externally verified against LiteLLM Enterprise, Portkey, and Kong Gateway's own published
enterprise-auth documentation, AWS's prescriptive multi-tenant SaaS authorization guidance,
Stripe's scoped-credential model, and the CSA Cloud Controls Matrix v4.1 — each claim
adversarially 3-vote-verified before inclusion here.

## Executive Summary

Across LiteLLM, Portkey, and Kong — the three vendors with verifiable, current enterprise-auth
documentation — SSO/OIDC, per-route RBAC granularity, and org/tenant-scoped credentials are
consistently gated behind a paid Enterprise license, never shipped free beyond a token allowance
(LiteLLM: SSO free for ≤5 users only, since v1.76.0, then a license is required). This confirms
SSO is a monetization-tier feature triggered by paying-enterprise demand, not something mature
competitors build ahead of enterprise revenue — the correct build trigger for Kelvran is a
concrete customer/procurement requirement, not "before any enterprise customer exists." The real
RBAC ceiling even at these vendors' Enterprise tiers is a small fixed role catalog plus
route/endpoint-level permission toggles (LiteLLM: ~6 named roles + a fixed key-route allowlist;
Kong: a `(workspace, endpoint-pattern, action)` 3-tuple with wildcard path support) — no surveyed
vendor ships a full custom/attribute-based policy engine as a baseline gateway feature, so
Kelvran's flat 2-tier model is one bounded increment away from competitive parity, not several.
No primary source located a concrete SOC2/ISO-mandated audit-log retention or export/SIEM figure
(CSA CCM v4.1's own overview names an Audit & Assurance domain but gives no day/month/year
number), meaning retention/export specifics are a customer- or auditor-negotiated detail to defer
until a real audit or contract demands a number. Portkey's live workspace-scoped API-key pattern
(org-wide Admin Keys vs. workspace-confined Keys) is a well-precedented, cheap analogue for
per-tenant-scoped admin credentials — but AWS's own guidance confirms authentication/authorization
alone is never equivalent to tenant isolation, so this is only worth building the moment Kelvran
itself becomes multi-tenant (customers who themselves have sub-customers/teams), not preemptively.

## Findings

### Finding 1 — SSO/OIDC/SAML is real, but it's a paid-tier feature gated behind enterprise revenue, not a pre-revenue build
**Confidence: high**

LiteLLM Enterprise ships SSO with SCIM provisioning and separate OIDC/JWT auth as named,
technically-documented features (real OIDC providers — Okta, Google, Microsoft, Generic — with
concrete env vars, a debug endpoint, and a dedicated SAML 2.0 page; SCIM has real REST endpoints
and documented 409/deactivation behavior). Portkey likewise ships SSO integrations with named
providers (Okta, Microsoft) plus separate OIDC and SAML 2.0 setup guides and SCIM provisioning
docs for both. Critically, in both cases SSO/SAML is packaged as an Enterprise-tier line item
bundled with audit logs, RBAC, and multi-team management — the base/self-serve flow uses plain
email+password, with SSO explicitly flagged as the enterprise upgrade path. LiteLLM's own source
code enforces this: SSO is free only for up to 5 billable users (`_raise_if_sso_exceeds_free_user_limit`,
returning HTTP 403 beyond that), a change shipped in v1.76.0 (2025-08) and still enforced on the
current `main` branch as of this research. No vendor in this survey ships full SSO as an
unconditional free/OSS feature at real scale.

**Verdict: NOT YET.** Named trigger: a specific paying (or in-procurement) customer's
compliance/IT/security-review process contractually or contractually-adjacently requires
centralized SSO — e.g., their security questionnaire lists SSO as a required control, or they
have more admin users than is practical to manage as individual bearer tokens. Building SSO
before that point is enterprise theater: it's expensive relative to Kelvran's current scale
(new auth flow, session model, IdP integration surface) and every surveyed comparable shipped it
only after gating it behind paid enterprise revenue, not before.

### Finding 2 — The real-world RBAC ceiling is a small role catalog + route/endpoint-level toggles, not a custom policy engine
**Confidence: high**

LiteLLM's self-serve RBAC ships a fixed catalog of roles (`proxy_admin`, `proxy_admin_viewer`,
`internal_user`, `internal_user_viewer`, plus team-scoped admin/user roles) — no custom policy
DSL. Below full-admin, LiteLLM's actual permission mechanism is a Premium
(paid-Enterprise-only) `team_member_permissions` toggle over a fixed, enumerable list of
key-management routes (`/key/info`, `/key/health`, `/key/list`, `/key/generate`, `/key/update`,
`/key/delete`, `/key/regenerate`, `/key/block`, `/key/unblock`) — the default restricted set
allows only `/key/info` and `/key/health`. Kong Gateway's Enterprise (also licensed, not OSS)
RBAC grants permissions at a per-resource/per-endpoint level — a `(workspace, endpoint-pattern,
action)` triple, with wildcard support for partial path segments (e.g. `/services/*/plugins`) —
which is more granular than LiteLLM's fixed route list but is still not a general condition/
attribute policy language. Org- and team-scoped admin roles (`org_admin`, `team_admin`) — the
closest LiteLLM analogue to tenant-scoped admin credentials — likewise require a paid Enterprise
license, and this gating was only backend-enforced in code as of one day before this research
(2026-09-10), meaning unlicensed deployments could previously use these roles for free despite
the docs saying otherwise — a reminder that vendor "gating" claims should be checked against
enforced code, not just documentation, before being treated as a hard floor.

**Verdict: NOT YET for a full custom/attribute-based policy engine** — no surveyed vendor ships
one as a baseline feature, so building one now would be building beyond what even funded
competitors consider necessary. **A narrower, cheap increment is closer to build-now-adjacent**:
a fixed, enumerable per-route permission allowlist on top of the existing 2-tier model (the same
pattern LiteLLM and Kong both use) — e.g., a third credential tier that can call read-only
routes plus a specific write route (rotate-key) but not others. Trigger: the first time a
support/ops hire or automation needs API access broader than Viewer but narrower than full Admin
(e.g., "can rotate keys and read audit logs, cannot change budget policy or model allowlists").

### Finding 3 — No concrete SOC2/ISO-mandated audit-log retention/export figure exists at the framework level; treat it as a customer/auditor-negotiated detail
**Confidence: medium**

The CSA Cloud Controls Matrix (CCM) v4.1 — the closest thing to an industry-standard cloud
control catalog — contains 197 control objectives across 17 domains, including distinct "Audit &
Assurance" and "Logging & Monitoring" domains, but its own overview page specifies no concrete
retention period (days/months/years) and no SIEM/export-integration requirement within those
domains. This is a negative finding scoped to the public overview page rather than the full paid
control-language spreadsheet, so it should be read as "no framework-level universal number was
found," not "no such requirement could ever apply" — a customer's own SOC2 Type II auditor, or a
specific enterprise contract's security addendum, can and does impose its own concrete number
(commonly framed around the audit observation window), but that number is negotiated per
engagement rather than fixed by the framework itself.

**Verdict: NOT YET.** Named trigger: a SOC2 Type II audit is actually scheduled for Kelvran (or
a customer's own auditor formally requests a specific retention/export commitment in a security
questionnaire). At that point, build exactly what that audit/contract requires (e.g., configurable
retention window, export to S3/a customer's SIEM) rather than guessing a number now — building a
generic "audit retention/export" feature ahead of a real audit risks solving the wrong requirement.

### Finding 4 — Per-tenant-scoped admin credentials have a real, cheap, well-precedented pattern — but only once Kelvran itself is multi-tenant
**Confidence: high**

Portkey ships a genuine, live precedent for scoped admin credentials: org-wide "Admin API Keys"
(Owner/Admin-only, can act across all workspaces or target one via `workspace_id`) versus
"Workspace API Keys" (creatable by workspace-level Admin/Manager roles, strictly confined to
operations within that one workspace — cannot reach other workspaces' virtual keys, configs, or
prompts). Workspaces are explicitly sold as Portkey's tenant-separation mechanism. This maps
workspace≈tenant almost exactly onto Kelvran's stated question. AWS's own prescriptive guidance
for multi-tenant SaaS API authorization is unambiguous on the underlying principle: authentication
and authorization alone never provide tenant isolation — "a hypothetical user could be
authenticated and authorized, and still access the resources of another tenant... you need to
implement tenant isolation to achieve this objective." This is exactly the gap that would open up
in Kelvran's current flat 2-tier bearer-token model if multiple tenants ever each managed their
own virtual keys under one Admin credential space. AWS separately confirms RBAC, ABAC, or a
hybrid are all equally valid architectural choices — there's no mandated granularity — which
means a simple `tenant_id`-scoped RBAC extension (not a full ABAC/policy engine) is architecturally
sufficient and precedented, not an under-build. Stripe's Restricted API Keys pattern reinforces
the same principle from a different angle: Stripe explicitly frames scoped credentials as the
mitigation for a stolen/compromised-credential blast radius and now actively steers integrators
away from unrestricted secret keys for exactly that reason — directly paralleling
`THREAT_MODEL.md`'s own framing of a flat/unrestricted admin credential as a full
privilege-escalation risk.

**Verdict: NOT YET, but cheap and precedented once triggered.** Kelvran has no tenant concept
today, so building this preemptively would be the definition of enterprise theater. Named
trigger: Kelvran itself becomes multi-tenant in practice — i.e., a customer that has its own
downstream sub-customers/teams wants to self-serve create/rotate/scope its own virtual keys
without visibility into other tenants' keys. At that point the build is bounded, not a rebuild:
add a `tenant_id` to virtual keys and to a new scoped-credential type, and filter every admin
list/read/write query by the caller's `tenant_id` when the credential carries one — reusing the
existing CompareAndSwap mutation path and per-virtual-key policy infrastructure rather than
introducing a new subsystem.

## Caveats

- Cloudflare AI Gateway and Higress were named in the original research scope, but no claim
  about either vendor's enterprise auth/RBAC offering survived verification in this pass — this
  research has no confirmed data on what they actually ship, only on LiteLLM, Portkey, and Kong.
- The Finding 3 result is limited to CSA CCM v4.1's public overview page, not the full paid
  control-language document; a deeper look at the full CCM spreadsheet or at the SOC2 Trust
  Services Criteria directly might surface more specific guidance that this pass didn't reach.
- Vendor Enterprise-gating facts (LiteLLM's 5-user SSO threshold, Kong's license requirement) are
  a 2026 snapshot of current pricing/licensing policy and can change; the *pattern* (SSO/fine-
  grained RBAC/org-scoping are monetized, not free-tier) is the durable takeaway, not the exact
  numbers.
- LiteLLM's `org_admin`/`team_admin` Enterprise-license enforcement in code is very recent
  (merged one day before this research) — this doesn't change the finding, but it's a reminder
  that "documented as Enterprise-only" and "enforced as Enterprise-only" can lag each other by
  months even at a funded, actively-maintained competitor.

## Open Questions

1. What do Cloudflare AI Gateway and Higress actually ship for admin RBAC/SSO — unresearched in
   this pass, and potentially a materially different (e.g., edge-network-native SSO) pattern.
2. Does the full CSA CCM v4.1 control-language document, or the SOC2 Trust Services Criteria
   directly, specify a concrete audit-log retention/export figure that a vendor's admin API would
   actually be held to during a real audit?
3. What's the real threshold (contract stage, ARR, or specific compliance mandate) at which
   Kelvran's actual prospective customers would require SSO in practice — is LiteLLM's 5-seat
   free-tier cutoff a useful proxy, or is it more a function of a customer's own IT policy
   regardless of Kelvran's team size?
4. If Kelvran later adds a `tenant_id`-scoped credential tier modeled on Portkey's Workspace Keys,
   does the existing CompareAndSwap mutation layer need schema changes beyond adding a `tenant_id`
   field, or are there per-tenant budget/rate-limit aggregation edge cases not covered by the
   current per-virtual-key policy model?
