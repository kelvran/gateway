# Multi-Tenancy & RBAC — Re-Evaluation Against the Live Pilot (2026-09-20)

## Question

Re-evaluate enterprise multi-tenancy/RBAC for Kelvran given what's changed since
`docs/upgrade-research/gateway-enterprise-multitenancy-2026-09-11.md` and
`docs/upgrade-research/multi-tenancy-access-control-2026-09-14.md` (both landed on a "NOT YET"
verdict, blocked on "no enterprise customer yet"): a real Bedrock pilot is now live with real
production traffic; cross-instance config propagation now exists
(`gateway/internal/configpropagation`); the admin API surface has grown substantially (virtual
keys, prompt labels, deployment weight, cache erasure, backup); and `identity.VirtualKey` now
carries a `BillingSubjectID` field. Does either prior report's "NOT YET" trigger still hold, or is
there now a concrete, low-risk, buildable v1 tenant-isolation/RBAC slice worth naming? External
grounding this pass: Kong's RBAC/workspaces, Portkey's org/workspace model, Cloudflare's
account/zone isolation, and Auth0/WorkOS's B2B SSO patterns, each verified against their own
current, live docs — not a secondary write-up of any of them.

This is at least the third research pass to touch multi-tenancy/RBAC specifically
(`gateway-enterprise-multitenancy-2026-09-11.md`, `multi-tenancy-access-control-2026-09-14.md`),
and the first written after a real pilot went into production. This pass's job is to check
whether that production milestone — and the interim admin-surface growth — actually changes either
prior verdict, not to re-litigate settled ground.

**Kelvran ground truth referenced** (confirmed via direct code/doc read this pass, not re-derived
from the prior two reports' own text):

- The admin API has exactly **three global, non-tenant-scoped credential tiers** today —
  `Credentials{Admin, Viewer, CostViewer}` (`gateway/internal/admin/admin.go` lines 46-62). Every
  route on the mux is gated by one of these three tiers directly; none is scoped to a subset of
  virtual keys, a tenant, or any grouping construct. `CostViewer` — added 2026-09-15
  (`DECISIONS.md`'s `[2026-09-15]` entry, Phase 2) — authenticates exactly one route,
  `GET /admin/virtual_keys/{name}/spend`, via a new `requireAnyBearerToken` middleware, and is
  explicitly cited in its own doc comment as implementing "the narrow-third-tier recommendation"
  from the 2026-09-14 report.
- `identity.VirtualKey.ID` is, today, the only tenant concept in the codebase: its own doc comment
  states it is "used... as the tenant dimension in cache keys" (`gateway/internal/identity/identity.go`
  lines 43-49). There is no grouping layer above a virtual key anywhere in the code.
- `identity.VirtualKey.BillingSubjectID` (added 2026-09-17, `gateway/internal/identity/identity.go`
  lines 123-135) is an "opaque, operator-supplied external billing identifier... never read by any
  enforcement path in this codebase (`budget.Reserve`/`Reconcile` never touch it); purely metadata
  threaded onto `GatewayDecisionEvent` for a future export consumer" — proven by a dedicated
  negative test, `TestBudgetReserveAndReconcileAreUnaffectedByBillingSubjectIDField`.
- `gateway/internal/configpropagation` (added 2026-09-17) is a real Redis pub/sub mechanism for
  propagating live admin mutations across gateway instances, but its own doc comment states "v1
  scope: only deployment-weight mutations" — virtual-key/prompt mutations are named as "a natural,
  disclosed follow-on," not yet built.
- The real pilot is a single virtual key: `pilot-team`, against AWS account `324481424504`, with a
  "$5 budget cap" (`DECISIONS.md`'s `[2026-09-13]` entry, reconfirmed live through the
  `[2026-09-16]`/`[2026-09-18]`/`[2026-09-19]` dry-run entries). `DECISIONS.md`'s `[2026-09-14]`
  (fifth) entry states plainly, one day after the pilot stood up: "no second tenant/operator on any
  real roadmap." No entry between then and `[2026-09-20]` revisits or contradicts that.
- `PRD.md` line 51 still states the "exact virtual-key/budget data model... not finalized."
- `THREAT_MODEL.md`'s Gateway Elevation-of-Privilege row — last substantively touched 2026-09-14 —
  still frames "a compromised admin credential" narrowly as one that "can create a virtual key with
  no budget cap and no model allow-list." It has not been updated to name any of the five
  higher-blast-radius admin routes shipped since (backup, cache erasure, deployment-weight
  mutation, prompt-label promote/rollback, key rotation) — a real, disclosed doc-staleness gap this
  pass surfaces, matching `AGENTS.md`'s own named recurring defect class.

## Executive Summary

The core **NOT YET** verdict for org/team hierarchy, tenant-scoped admin credentials, and
Admin-API SSO/OIDC holds unchanged — the specific trigger every prior pass named (a customer with
its own sub-customers/teams wanting self-serve scoped keys, or a concrete enterprise
procurement/compliance ask) has not fired. The pilot is real production validation of ONE customer
on ONE $5-capped virtual key, not evidence of multi-tenant demand; `DECISIONS.md`'s own most recent
explicit statement on this exact question, written the day after the pilot stood up, says so
directly and has gone unchallenged for six days of subsequent, heavy shipping activity. What this
pass adds: (1) the narrow third-RBAC-tier item both prior reports called "build-now-adjacent" is no
longer a recommendation — it shipped as `CostViewer` on 2026-09-15, closing that specific finding
outright; (2) `BillingSubjectID`, despite sounding tenant-adjacent, is confirmed by direct code read
to be explicitly NOT an authorization or grouping construct — worth stating plainly so it is never
later mistaken for progress on the deferred `tenant_id`-scoped-credential design; (3) config
propagation is a multi-instance operational-consistency fix, orthogonal to the authorization
question, and is itself narrower today than full admin-mutation propagation; (4) the admin API's
blast radius has grown materially since either prior report was written, which is genuine new
evidence — but it argues for a *different*, narrower, buildable-now item than tenant isolation:
splitting the single flat `Admin` tier into risk-tiered operational roles, an extension of the
exact route-allowlist pattern Kong and Kelvran's own `CostViewer` already validate. Newly
researched this pass: WorkOS's real pricing shows enterprise SSO can be *bought* per-connection
($125→$65/mo sliding scale, or bundled free in AuthKit up to 1M MAUs) rather than built from
scratch — this changes what "cheap" means for the SSO trigger's eventual build, without changing
whether that trigger has fired. Cloudflare's actual access-control primitive (a zone-scoped,
action-scoped API token) turned out to be a fourth, independently-arrived-at vendor validating the
same narrow-credential shape as Kong/Portkey/Kelvran's own `CostViewer` — not an org/workspace
hierarchy, and its flagship multi-tenancy product (Workers for Platforms) is a compute-isolation
answer to a problem Kelvran doesn't have.

## Findings

### Finding 1 — The core NOT YET verdict holds: no multi-tenant demand signal or enterprise-compliance ask has fired
**Confidence: high**

`DECISIONS.md`'s `[2026-09-13]` entry confirms the real pilot is exactly one virtual key
(`pilot-team`) with a real $5 budget cap against one AWS account. The very next dated entry,
`[2026-09-14]` (fifth entry, same day as an unrelated admin-UI decision), states outright: a
bespoke admin dashboard stays deferred because there is "no second tenant/operator on any real
roadmap." This sentence was written with full knowledge the pilot already existed — it is not a
stale pre-pilot assumption — and nothing in the six subsequent dated entries through
`[2026-09-20]` (spanning five more shipping rounds, two version releases, and three separate live
production dry-runs against the same pilot) revisits or reopens the question. The pilot demonstrates
real production traffic and real revenue-adjacent validation, which is a meaningfully different
fact than "zero customers exist" — but the trigger every prior pass named was never "a customer
exists"; it was specifically "a customer with its own sub-customers/teams wanting self-serve scoped
key management" (2026-09-11 Finding 4; 2026-09-14 Finding A) or "a specific enterprise
compliance/procurement process requires centralized SSO" (2026-09-11 Finding 1; 2026-09-14 Finding
C). One pilot team on one budget-capped key satisfies neither.

**Verdict: NOT YET, unchanged.** Named trigger unchanged from both prior passes.

### Finding 2 — The narrow third-RBAC-tier recommendation from both prior passes has been fully realized, not merely reinforced
**Confidence: high**

Both 2026-09-11 (Finding 2) and 2026-09-14 (Finding B) called a narrow, fixed-permission credential
tier between Viewer and full Admin "BUILD-NOW-ADJACENT," modeled on LiteLLM's own free-tier
`team_member_permissions`/`proxy_admin_viewer` split and Kong's endpoint-allowlist RBAC ceiling.
`CostViewer` (`gateway/internal/admin/admin.go` lines 46-61, 171-181), shipped 2026-09-15, is
exactly that pattern: it authenticates ONE route (`GET /admin/virtual_keys/{name}/spend`) via a new
`requireAnyBearerToken` middleware, and cannot reach config, prompts, or any write route. Its own
doc comment cites the 2026-09-14 report by name as the source of the recommendation.

**Verdict: DONE.** This specific item moves from "recommended, not yet built" to "built and live" —
a real status change from both prior reports, not a repeat of their own already-stated conclusion.

### Finding 3 — `BillingSubjectID` is tenant-adjacent in appearance only; it is explicitly not an authorization or grouping construct
**Confidence: high**

`identity.VirtualKey.BillingSubjectID`'s own doc comment states it is "never read by any
enforcement path in this codebase" — confirmed by a dedicated negative regression test,
`TestBudgetReserveAndReconcileAreUnaffectedByBillingSubjectIDField`, added in the same commit
specifically to prove `budget.Reserve`/`Reconcile` never consult it. It is a per-key opaque string
threaded onto `GatewayDecisionEvent` field 16 for a future external billing-platform export
consumer — it does not group multiple virtual keys under one external entity for any *policy*
purpose, does not scope any admin credential, and is not consulted by any authorization check
anywhere in the codebase. It is, however, a reasonable field to reuse as the join key if the
2026-09-11 report's Finding 4 (a Portkey-Workspace-Key-style `tenant_id`-scoped credential) is ever
actually triggered — an operator-supplied external identifier is exactly the shape a real `tenant_id`
field would also need — but that is a design note for a future trigger, not evidence the trigger
has fired.

**Verdict: does not move Finding 4 (2026-09-11) / Finding A (2026-09-14) off NOT YET.** No new
authorization capability exists where none did before.

### Finding 4 — Config propagation is an operational-consistency fix, orthogonal to the tenant/RBAC question, and is itself narrower than full admin-mutation propagation
**Confidence: high**

`configpropagation.PubSub` (Redis pub/sub, shipped 2026-09-17) is real, but its own doc comment
states "v1 scope: only deployment-weight mutations (`TypeDeploymentWeight`)" — virtual-key
create/delete/rotate and prompt mutations still do not propagate across gateway instances at all;
the package's doc comment names this as a "natural, disclosed follow-on," not yet built. Even fully
built out, this closes an operational-consistency gap (do all instances observe the same live
config) rather than an authorization gap (who is allowed to see or change what) — it has no bearing
on org/team hierarchy, tenant-scoped credentials, or SSO.

**Verdict: not relevant to reopening any of the three prior NOT YET verdicts.** Noted here only
because the originating question named it explicitly; it does not change the analysis.

### Finding 5 — The admin API's growing blast radius is real new evidence — but it argues for risk-tiered operational roles, not tenant isolation
**Confidence: high**

Five materially higher-blast-radius admin routes have shipped since either prior report was
written: `POST /admin/backup` (a full bbolt export — every virtual key's hash plus budget/spend
state across the *entire* deployment, not scoped to any one key or tenant); `POST
/admin/cache/erase` (a DoS/anti-forensic primitive); `POST /admin/deployments/{name}/weight` (can
silently redirect all traffic for a model to a chosen deployment); `PUT`/`DELETE
/admin/prompts/{id}/labels/{label}` (production-prompt promote/rollback — a supply-chain-into-
prompts vector); and `POST /admin/virtual_keys/{name}/rotate`. All five are gated by the single flat
`Admin` tier, with no finer split. `THREAT_MODEL.md`'s own Elevation-of-Privilege row has not been
updated to name any of them since 2026-09-14 — a real, disclosed doc-staleness gap.

This is a genuinely new argument that did not exist when either prior report was written, since
most of this surface didn't exist yet: a single flat `Admin` credential now bundles capability
classes of meaningfully different severity and reversibility — rotating a key (reversible, single-
resource) sits in the same tier as exfiltrating a full cross-tenant backup (irreversible once
leaked, global). Two independent vendor precedents, both directly verified against current docs
this pass, validate that a narrower, named-route/resource credential split — not a full policy
engine, not an org hierarchy — is the industry's real granularity ceiling: Kong Gateway's RBAC
(`developer.konghq.com/gateway/entities/rbac/`, fetched live) grants permissions via an
`(endpoint, workspace, actions)` 3-tuple, each independently wildcardable, illustrated by "a user
can have read permissions on `/foo/bar` and write permissions on `/foo/bar/far`" — genuinely
current (2026) confirmation of the same shape the 2026-09-11 report already found, this time
without needing the word "Enterprise" to signal gating (the same page requires a
`KONG_LICENSE_DATA` environment variable and is scoped `works_on: on-prem`). Cloudflare's API
tokens (Finding 7, below) independently confirm the identical shape from a categorically different
product. Kelvran has already built exactly one instance of this pattern itself (`CostViewer`,
Finding 2 above) — this is not a novel design, it's the next application of a pattern already
proven in this codebase.

**Recommended v1 slice** (buildable now; requires no tenant concept and no customer trigger): split
`Admin` into two named operational tiers along reversibility/blast-radius lines — e.g. an
`Operator` tier for reversible, single-resource-scoped writes (rotate a key, set one deployment's
weight, erase one cache entry) versus a narrower true-`Admin`/`SuperAdmin` tier reserved for
irreversible-or-cross-tenant routes (backup, prompt upsert/delete, virtual-key delete). This is
explicitly *not* the org/team/tenant hierarchy question from Finding 1/4 — it's an admin-operational
role split, motivated by the admin surface's own growth, not by any multi-tenancy signal.

**Verdict: BUILD-NOW-ADJACENT** — a new, distinct recommendation from this pass, separate from the
already-shipped Finding 2 (`CostViewer`).

### Finding 6 — WorkOS reframes the SSO trigger's eventual build cost as a buy decision, without changing whether the trigger has fired
**Confidence: high**

WorkOS's `Organization` object (`workos.com/docs/reference/organization`, fetched live) is
explicitly documented as usually representing "one of your customers," with `external_id`,
`metadata`, and `stripe_customer_id` fields purpose-built for real-world tenant mapping — a third,
independently-arrived-at confirmation (alongside Auth0 Organizations and Portkey's own Workspaces,
both already covered by the prior two passes) that "one grouping container per B2B customer" is the
standard shape across categorically unrelated vendors. Auth0 Organizations
(`auth0.com/docs/manage-users/organizations`, fetched live) confirms the identical B2B-tenant-
container pattern and states plainly that "availability varies by Auth0 plan" — paid/plan-gated,
consistent with every vendor surveyed across all three research passes to date.

The materially new data point is WorkOS's own pricing (`workos.com/pricing`, fetched live): SSO and
Directory Sync (SCIM) connections are priced on a per-connection volume ladder — "1–15 $125/ea,"
sliding to "51–100 $65/ea," "101+ Custom" — or bundled free within AuthKit's own user-management
product for the "First 1M MAUs," with paid overage at "$2,500/mo" per additional 1M. This means the
SSO build cost both prior reports scoped — a from-scratch OIDC/SAML federation effort, mirroring
LiteLLM's or Portkey's own in-house engineering — is not the only real option: a specific enterprise
SSO ask could instead be served by integrating a paid third-party Organizations/SSO provider (WorkOS
or Auth0) behind Kelvran's existing admin bearer-token surface, at a cost of one or a handful of
$65–125/mo connections plus integration work, categorically cheaper than building IdP federation
in-house.

**Verdict: NOT YET, trigger unchanged** — but the eventual build, once triggered, is now known to be
cheaper than either prior report scoped, since neither considered a buy-vs-build option at all.

### Finding 7 — Cloudflare's real tenant-isolation primitive is a scoped API token, not an org/workspace hierarchy — and independently reinforces Finding 5, not org/team hierarchy
**Confidence: medium** (Cloudflare's own docs proved thinner and more scattered on this specific
question than Kong's or Portkey's — see Caveats)

Cloudflare's Account (top-level, billing/org-wide container) versus Zone (one per domain) split,
and its API tokens — confirmed live via `developers.cloudflare.com/fundamentals/api/get-started/
create-token/` — are explicitly scoped to one of Account, User, or Zone level, restricted to a named
resource ("`Zone DNS Read` access to a zone `example.com`... [a]ny other zone will return an error"),
and to an action level ("`Edit`" = full CRUDL vs. "`Read`"). No "workspace" or "org" vocabulary
appears anywhere in Cloudflare's own access-control docs, unlike Kong, Portkey, Auth0, or WorkOS.

Cloudflare's actual flagship multi-tenant SaaS product — Workers for Platforms
(`developers.cloudflare.com/cloudflare-for-platforms/workers-for-platforms/`, fetched live) — is a
compute-isolation answer ("[e]ach customer runs code in their own Worker, a secure and isolated
environment," with per-customer CPU/subrequest limits and customer-ID tagging) to a categorically
different problem than Kelvran's: Kelvran proxies API calls to upstream LLM providers, it does not
execute untrusted customer-supplied code. This is a real negative finding worth stating plainly —
Workers for Platforms is not a usable analogy for Kelvran's tenant-isolation question, despite being
Cloudflare's most prominent multi-tenancy product by far.

The transferable Cloudflare precedent is narrower and more mundane: a credential restricted to
exactly one resource plus one action level is architecturally the same shape as Finding 5's
recommended operational-role split, and as the still-deferred `tenant_id`-scoped-credential idea
from the 2026-09-11 report (Portkey's Workspace Key). This is a fourth independent vendor — after
Kong, Portkey, and Kelvran's own shipped `CostViewer` — converging on the same narrow-credential
shape as the real granularity ceiling, not a full policy engine and not an org hierarchy.

**Verdict: no new tenant-isolation trigger evidence; reinforces, but does not add to, Finding 5's
already-buildable recommendation.**

## Caveats

- **Methodology difference from the two prior passes**: this pass was a single-agent direct
  primary-source verification (`WebFetch` against each vendor's own current documentation, plus
  direct reads of Kelvran's live code and `DECISIONS.md`), not the multi-agent 3-vote adversarial
  verification loop the 2026-09-11 and 2026-09-14 passes used. Disclosed as a real methodological
  difference, not a claim of equivalent rigor.
- Cloudflare's own docs proved thinner on the specific account-vs-zone permission-boundary question
  than expected — two fetch attempts (the Members/Roles overview page, the "Find account and zone
  IDs" page) explicitly stated they lacked the definitional detail being asked for; Finding 7's
  account/zone and token-scoping picture is assembled from what did survive fetch (the API-token
  creation guide and the Workers for Platforms overview), not a single authoritative conceptual
  page. A deeper read of Cloudflare's full Permissions Reference or Roles pages (not reached this
  pass) could sharpen or correct this finding.
- Kong's `developer.konghq.com/gateway/entities/rbac/` page confirmed the endpoint/workspace/action
  3-tuple and built-in role list directly and currently, but never used the literal word
  "Enterprise" — the licensing signal here is inferred from the page's `KONG_LICENSE_DATA`
  requirement and `works_on: on-prem` frontmatter, consistent with but not a verbatim restatement of
  the 2026-09-11 report's own "Kong Enterprise, licensed" finding (which used a different,
  now-possibly-stale URL that 404s as of this pass).
- WorkOS's and Auth0's pricing/plan-gating figures are 2026 snapshots of current commercial policy,
  per the same durable-pattern-not-exact-numbers caveat both prior passes already logged for
  LiteLLM/Portkey/Kong — the pattern (B2B org container = standard; SSO federation = monetized
  everywhere surveyed, buy-or-build) is the durable takeaway, not the exact dollar figures.
- This pass did not re-verify LiteLLM's own current SSO/RBAC gating — reused from the two prior
  passes' own already-verified record (unchanged since 2026-09-14), not re-derived.
- `THREAT_MODEL.md`'s Elevation-of-Privilege row's staleness (Finding 5) is disclosed here, not
  fixed — updating that document is out of this research pass's own scope.

## Open Questions

1. (carried forward, unchanged from both prior passes) What's the real threshold — ARR, contract
   stage, or a specific compliance mandate — at which Kelvran's actual prospective customers would
   require SSO or org/team hierarchy in practice?
2. If Finding 5's operational-role split is built, should the two-tier boundary be drawn by
   reversibility (as proposed here) or by data-sensitivity (secrets/backup-adjacent vs. everything
   else)? Kelvran doesn't yet have enough real incident or support-hire history to know which axis
   actually matters in practice — unresearched, since no real trigger event exists yet to design
   against.
3. If/when the SSO trigger fires, is WorkOS's or Auth0's Organizations/B2B model the better
   integration fit for Kelvran's own bearer-token-based Admin API — or does bearer-token auth's own
   shape make either integration less natural than this pass's framing suggests? Unresearched, since
   no real ask exists yet.
4. Does a deeper read of Cloudflare's full Permissions Reference and Roles documentation (not
   reached this pass — see Caveats) sharpen, correct, or add nuance to Finding 7's account-vs-zone
   characterization?
