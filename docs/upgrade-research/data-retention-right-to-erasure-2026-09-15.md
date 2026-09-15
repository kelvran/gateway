# Data Retention & Right-to-Erasure — Upgrade Research (2026-09-15)

**Scope.** Kelvran stores real operational data with no disclosed retention or erasure policy: a
structured JSON audit log (`gateway/internal/admin/admin.go`), cached request/response content
across L1/L2/L3 (`gateway/internal/cache/`), and per-key budget-spend history
(`gateway/internal/budget/`, persisted via `boltstore`) — all in local bbolt files or in-process
memory. This report asks what *stored* data should do over time (expire, get disclosed a
retention window, be deletable on request) and how comparable LLM gateways/API platforms handle
that. It is deliberately **not**
`docs/upgrade-research/data-residency-regional-routing-2026-09-15.md`'s territory — that report is
about where a request is *routed* (`Deployment.Region`, `attemptFallbackChain`'s missing region
gate); this one is about how long *already-stored* data lives and whether it can be erased on
demand. 14 claims survived 3-vote adversarial verification against real regulatory primary sources
(UK ICO, California Civil Code/CPPA) and comparable-platform docs (OpenAI, LiteLLM, LLM Gateway).

**Already shipped (not re-litigated).** `cache.Cache.Delete(ctx, key) error`
(`gateway/internal/cache/port.go:38-52`, active in `gateway/internal/cache/inprocess/inprocess.go:159-172`)
shipped 2026-09-14, closing the exact gap
`docs/upgrade-research/ai-compliance-regulatory-readiness-2026-09-14.md` Finding 4 named — see
`THREAT_MODEL.md`'s `2026-09-14` Change Log entry for the record. That report's other findings
(EU AI Act scoping, the SOC 2 category-error argument, `PROVIDERS.md`'s cross-border-transfer-safeguard
gap, the Govern-function third-party-risk framing) are separate compliance axes and are not
re-litigated here. This report's own new ground: no retention *policy* exists for any of the three
stores named above, the erasure mechanism that does exist (cache `Delete`) is single-key-only and
has no counterpart in the other two stores, and comparable platforms treat automated retention/
erasure as more than a single-purpose bug fix — it's a documented, first-class feature.

---

## Executive Summary

Kelvran's cache now has a real, working single-key erasure primitive, but nothing in the codebase
documents *how long* any of its three operational stores (audit log, cache, budget-spend) retains
data, and the audit log and budget-spend boltstore have no erasure or expiry mechanism at all —
`boltstore.Store` (`gateway/internal/budget/boltstore/boltstore.go`) exposes only `Load`/`Save`,
and every admin action is logged unconditionally via `slog.Info` with no retention window, no
disable toggle, and no delete path. Both UK GDPR and CCPA/CPRA treat a documented retention
schedule and a subject-facing deletion right as baseline obligations, not advanced features, and
comparable LLM gateways (LiteLLM, LLM Gateway) already ship exactly this — configurable,
non-enterprise-gated automatic log expiry plus a manual on-request erasure channel — as standard,
not premium, functionality. Kelvran itself already has the *pattern* of a disclosed, automated
retention window elsewhere in the repo (a 90-day S3 lifecycle rule for the separate `evals`
trace-ingestion pipeline, `docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md` §4) — it
just hasn't been extended to the gateway's own admin/cache/budget stores. None of this makes
Kelvran itself the GDPR/CCPA-obligated party (per the prior compliance report's framing, that's
almost always the operator deploying it) — but the operator's ability to comply is entirely gated
on what Kelvran's code exposes, and today it exposes a documented retention window for nothing and
an erasure mechanism for exactly one of three stores.

---

## Findings

### Finding 1 — No retention policy is documented for any of the three stores, even though Kelvran already has this exact pattern elsewhere in the repo
**Confidence: high** (2 merged claims, 3-0 and 2-1, both primary regulatory sources; code-verified absence)

The ICO's own storage-limitation guidance states plainly: "You need a policy setting standard
retention periods wherever possible, to comply with documentation requirements" — reinforced twice
more on the same page (a checklist item, and a "Do we need a retention policy?" section explaining
retention schedules "list the types of record or information you hold, what you use it for, and
how long you intend to keep it"). CCPA/CPRA (Cal. Civ. Code §1798.100(a)(3)) goes further and makes
this an affirmative *disclosure* duty: a business must state "the length of time [it] intends to
retain each category of personal information... or if that is not possible, the criteria used,"
and separately may not retain personal or sensitive personal information "for longer than is
reasonably necessary for that disclosed purpose."

Grepping `docs/` for any retention document scoped to the gateway's own operational data (audit
log, cache, budget-spend) turns up nothing — no `PRIVACY.md`, no `DATA-RETENTION.md`, no section of
`SECURITY.md` or `THREAT_MODEL.md` stating a retention window for any of the three stores this
report covers. This is a real, checkable gap, not a hypothetical: `PROVIDERS.md` already documents
per-provider data flows and `docs/upgrade-research/ai-compliance-regulatory-readiness-2026-09-14.md`
already flagged the *erasure* half of this (Finding 4); nothing has flagged the *retention-schedule
disclosure* half until now.

**Kelvran already has this exact pattern elsewhere, just not applied here.** The `evals` deployable's
trace-ingestion pipeline (`docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md` §4) ships
"a single S3 Lifecycle rule on the `gatewayevents/v1/` prefix: expire (delete) objects 90 days
after creation" — a real, automated, disclosed retention window. That same RFC is admirably honest
about why: "90 days is chosen as a reasoned, explicitly provisional number... not a number derived
from a compliance requirement, since none is documented anywhere in `THREAT_MODEL.md`/`SECURITY.md`
for this data class." That sentence, read today, is the gap this finding names directly — the
pattern (disclosed, automated, time-bounded retention) exists and works in this codebase; it simply
hasn't been asked of the admin audit log, the cache, or the budget-spend store, all three of which
hold data closer to a real data subject (a virtual key's spend history, an admin's own actions,
cached prompt/response content) than `gatewayevents_v1`'s metadata-only records do.

**Verdict: build_now.** A short retention-policy note (mirroring the `evals` RFC's own §4 in shape)
per store — even a provisional, explicitly-uncalibrated number, as that RFC's own honest framing
models — closes the documentation-obligation half of both regimes cheaply, without needing new
code for the audit log or budget-spend store yet.

### Finding 2 — The GDPR Article 17(3) legal-obligation/legal-claims exemption is a real, available basis for keeping audit-log entries past a deletion request — but Kelvran documents no statutory basis today, so it isn't currently claimable
**Confidence: high** (3-0, primary UK GDPR/ICO source)

The right to erasure "does not apply if processing is necessary... to comply with a legal
obligation... [or] for the establishment, exercise or defence of legal claims" (UK GDPR Art.
17(3)(b)/(e), per the ICO's own guidance). This is a real, narrow exemption a structured audit
trail can invoke — but every source describing it agrees it must be affirmatively claimed and
documented, not assumed: it requires a genuine statutory basis, not internal policy, and courts
have reinforced (a June 2026 CJEU judgment, C-312/24) that the basis must be "clear, foreseeable
and proportionate," not retrofitted after the fact.

Kelvran's admin audit log is real and unconditional: `Handler`'s own doc comment
(`gateway/internal/admin/admin.go:20-21`) states "Every successful virtual-key create/delete is
logged," and every write route (`upsertVirtualKeyHandler`, `deleteVirtualKeyHandler`,
`rotateVirtualKeyHandler`, `upsertPromptHandler`, `deletePromptHandler`) plus every read route calls
`logger.Info(...)` with no surrounding feature flag — a repo-wide grep for `EnableAudit`/
`AuditEnabled`/`DisableAudit` or any audit-toggle config field returns nothing. Nothing in
`THREAT_MODEL.md`, `SECURITY.md`, or any RFC states a legal or contractual basis (SOC 2, a
retention regulation, a specific customer contract term) requiring these entries to be kept.

**Verdict: build_now, cheap.** If the intent is for this audit log to survive an erasure request by
design (a defensible, common choice for security/audit trails), that basis needs to be written
down explicitly — e.g. "this log exists to satisfy [X]; entries are retained for [Y] and are exempt
from erasure requests under [legal basis]" — rather than left implicit. Without that documentation,
an operator asserting the exemption today would be asserting something Kelvran's own repo doesn't
support asserting.

### Finding 3 — Erasure capability is asymmetric across the three stores: cache has single-key `Delete`; the audit log and budget-spend store have no erasure, expiry, or disable mechanism at all
**Confidence: high** (code-verified directly; comparable-platform gap corroborated 3-0)

`cache.Cache.Delete` (Finding, already-shipped section above) gives the cache a real, working
per-key erasure path. Nothing comparable exists for the other two stores this report covers:

- `boltstore.Store` (`gateway/internal/budget/boltstore/boltstore.go`) exposes exactly two methods,
  `Load(ctx) (map[string]decimal.Decimal, error)` and `Save(ctx, keyID, spent) error` — no `Delete`,
  no `Purge`, no TTL of any kind. A virtual key's cumulative spend, once written, persists in the
  bbolt file forever, with no code path to remove it short of taking the file offline and hand-editing
  it.
- The admin audit log (Finding 2) has no delete, redaction, or retention/TTL mechanism anywhere —
  it is `slog.Info` calls writing to `os.Stdout` (`gateway/cmd/gateway/main.go:167`,
  `slog.New(slog.NewJSONHandler(os.Stdout, nil))`), whose actual on-disk lifetime is entirely a
  function of whatever log-rotation/retention the operator's own infrastructure applies —
  nothing Kelvran ships controls or discloses it.

For comparison, LiteLLM — the OSS gateway closest to Kelvran's own shape — makes exactly this
distinction explicit for its own audit table: `LiteLLM_AuditLog` ("tracks changes to system
configuration... records who made changes and what was modified") is **"off by default"**, an
opt-in feature an operator turns on deliberately, rather than something that always runs. Kelvran's
audit log runs the opposite way — always on, with no toggle — which is a defensible product choice
(more visibility by default is arguably safer, per `THREAT_MODEL.md`'s own "compromised admin
credential is a privilege escalation" framing) but means the retention-schedule/exemption
discipline in Finding 1/2 matters *more* here, not less, since there's no way to simply switch this
data source off.

**Verdict: build_now** for the retention-window/policy half (Finding 1 covers this); **not_yet**
for a full `Delete`/`Purge` on `boltstore.Store` specifically, pending Finding 1's policy decision —
adding delete-by-key to a store that's supposed to be an append-only ledger of "what was spent" is
a more consequential design change than the cache's `Delete` was (a cache entry is disposable by
design; a spend ledger arguably shouldn't be, without a stated retention policy governing when and
why an entry may legitimately disappear).

### Finding 4 — LiteLLM ships configurable, automated log retention/cleanup as a plain OSS feature, not enterprise-gated — a concrete, low-effort template for `boltstore.Store` specifically
**Confidence: high** (3 merged claims, all 3-0, corroborated against LiteLLM's own current source code)

LiteLLM's proxy supports `maximum_spend_logs_retention_period` (e.g. `7d`) and a companion
`maximum_spend_logs_retention_interval` (e.g. `1d`) that together delete spend-log rows older than
the configured period on a configurable cadence. Critically, this is **opt-in with no default**:
per LiteLLM's own docs, "with neither set, nothing is ever deleted, no matter what the batch,
budget, or interval settings say" — confirmed directly against LiteLLM's current source
(`spend_log_cleanup.py`'s `_should_delete_spend_logs()` returns false unless the retention period
is explicitly configured, with an early-return before touching any row otherwise). And this
retention/cleanup capability ships fully open-source: "Retention and cleanup are open source. Every
setting on this page works without an enterprise license" — verified against the codebase itself,
which has zero `premium_user`/license checks anywhere in the cleanup call path (in contrast to a
genuinely enterprise-gated feature sitting in the same file, which does check).

This maps almost exactly onto the gap Finding 3 names for `boltstore.Store`: LiteLLM's
`LiteLLM_SpendLogs` table is the direct structural analog of Kelvran's per-key spend bucket, and
LiteLLM's answer — an explicit, operator-configured, off-by-default retention window with a
separate cleanup cadence, shipped as a baseline feature rather than a paid add-on — is a
ready-made design template Kelvran could adopt without inventing a new pattern.

**Verdict: not_yet as a build item on its own** — this is the concrete "what to build" answer *once*
Finding 1's policy question (what should the retention window actually be, and does deleting spend
history conflict with the ledger's audit purpose) is settled, not before. Recorded here so the
design, when it happens, doesn't have to be invented from scratch.

### Finding 5 — Commercial LLM platforms treat "automatic default-window expiry, plus a manual/approval-gated exception channel" as the accepted two-tier erasure model — a viable interim posture for Kelvran ahead of a full per-subject sweep
**Confidence: high** (4 merged claims, all high-confidence primary sources: OpenAI's own docs, LLM Gateway's own docs)

Two real platforms in this space show the same two-tier shape, independently:

- **OpenAI**: a Zero Data Retention (ZDR) mode exists for eligible endpoints (`/v1/chat/completions`,
  `/v1/responses`) that excludes customer content from abuse-monitoring logs entirely — but "these
  controls are subject to prior approval by OpenAI," reached by contacting sales, not a self-service
  toggle. Separately, for stateful objects (Assistants, Threads, Vector Stores) that are *not*
  ZDR-eligible, OpenAI runs a two-stage model: a user-initiated logical delete via API/dashboard,
  followed by an actual server-side purge only 30 days later — "Objects... are deleted from our
  servers 30 days after you delete them."
- **LLM Gateway** (docs.llmgateway.io): "Data is retained for 30 days for all users" by default,
  auto-deleted at expiry, with enterprise customers able to negotiate longer/custom windows — and
  *separately*, "you can request immediate deletion of specific records through support," an
  explicitly manual, human-mediated erasure channel distinct from the automatic 30-day expiry.

Neither platform treats "instant, fully self-service, per-subject erasure" as the bar — both pair
an automatic time-bound default with a manual escape hatch for anything the automation doesn't
reach. That's directly relevant to Kelvran's own posture: the ai-compliance-regulatory-readiness
report already named a fully automated "find every entry that might belong to subject X" sweep as
`not_yet` (it needs a write-time PII-provenance index, a real design commitment). This finding says
that gap doesn't need to block a documented interim policy — "the cache's known-key `Delete` plus a
manual, support-style procedure for anything else, until an automatic window exists" is exactly the
LLM-Gateway/OpenAI shape, not a uniquely weaker position.

**Verdict: build_now** for documenting the manual/interim procedure explicitly (cheap — a short
`docs/operations/DATA-SUBJECT-REQUESTS.md`, which the prior compliance report already suggested as
a placeholder but which doesn't yet exist); **not_yet** for a ZDR-equivalent approval-gated mode or
a fully automated per-subject sweep — both are real, larger investments neither comparable platform
treats as table-stakes either.

### Finding 6 — If an operator running Kelvran had to honor a UK-GDPR-style one-month deadline or CCPA's third-party-cascade duty today, none of the three stores could support a complete, subject-scoped response
**Confidence: high** (3 merged claims, 2-1/2-1/2-1, all primary legal sources)

Two concrete legal mechanics matter here, independent of retention policy:

- **The clock.** Under UK GDPR, once a valid erasure request is received, an organization "must
  respond... without undue delay and at the latest within one month of receipt." This is the SLA
  any erasure process would need to hit.
- **The right and its reach.** CCPA/CPRA (Cal. Civ. Code §1798.105(a)) gives consumers "the right
  to request that a business delete any personal information... which the business has collected
  from the consumer" — and §1798.105(c)(1) goes further, requiring the business to delete from its
  own records **and** "notify any service providers or contractors to delete... and notify all
  third parties to whom the business has sold or shared the personal information to delete... unless
  this proves impossible or involves disproportionate effort."

Today, none of Kelvran's three stores could satisfy a subject-scoped version of either duty
completely: cache `Delete` only removes an exact key the caller already knows — there is no
tenant-or-subject-scoped bulk purge across L1/L2/L3's different keying schemes; `boltstore.Store`
has no delete path at all (Finding 3); and nothing anywhere in the codebase tracks or notifies
upstream model providers about a deletion request, so the CCPA cascade obligation has zero
mechanical counterpart in any of the three subsystems. A single-known-cache-key request could be
served well within a one-month window trivially; a real "everything about subject X, including
what was shared with upstream providers" request could not be served completely by any deadline,
because the tooling to even *find* everything doesn't exist yet.

**Verdict: not_yet for full compliance tooling** (this is the same `not_yet` the prior compliance
report already gave the PII-provenance-index problem — this finding doesn't reopen that, it
confirms the same gap from the deadline/cascade angle specifically); **build_now** for disclosing
the current limitation explicitly (an operator relying on Kelvran for this today should know the
gap exists before a real request arrives, not discover it under a one-month clock) — folds into
Finding 5's suggested `docs/operations/DATA-SUBJECT-REQUESTS.md`.

---

## Synthesis: build_now vs not_yet

| Item | Verdict | Why |
|---|---|---|
| Retention-policy note per store (audit log, cache, budget-spend) | **build_now** | Cheap, documentation-only; closes a real ICO/CCPA disclosure gap; Kelvran already has this exact pattern (the `evals` 90-day S3 lifecycle rule) to mirror |
| Explicit legal-basis statement for the audit log's Art. 17(3) exemption | **build_now** | Cheap; the exemption is real but currently unclaimed/undocumented |
| `docs/operations/DATA-SUBJECT-REQUESTS.md` (manual/interim erasure procedure + known limitations) | **build_now** | Cheap; mirrors OpenAI's/LLM Gateway's own accepted "automatic-window-plus-manual-channel" shape; the prior compliance report already named this file, never written |
| `boltstore.Store.Delete`/configurable spend-log retention window (LiteLLM-template) | **not_yet** | Real design work — needs Finding 1's policy decided first (does deleting spend history conflict with the ledger's own audit purpose?) |
| Full automated per-subject/tenant erasure sweep across all three stores | **not_yet** | Needs a write-time PII-provenance index — already correctly named `not_yet` by the prior compliance report; this pass found no new evidence to revisit that |
| A ZDR-equivalent approval-gated no-retention mode | **not_yet** | Bigger investment than either comparable platform treats as baseline; no operator has requested it |
| Kelvran itself signing DPAs/executing CCPA cascade notices on an operator's behalf | **not_yet** | Not applicable — self-hosted infrastructure, not a data processor with its own customer contracts (consistent with the prior compliance report's framing) |

---

## Open Questions

- Should `boltstore.Store` gain a delete/purge method at all, or is a spend ledger's own audit
  purpose (proving what was actually billed) a legitimate reason to treat it differently from the
  cache and *not* offer per-subject deletion — relying instead on a documented, shorter
  aggregation/anonymization window rather than true deletion? This report surfaces the tension
  (Finding 3) without resolving it.
- Should the eventual retention-policy note (Finding 1) live as one new doc covering all three
  stores, or be split — mirroring how `PROVIDERS.md` and the `evals` RFC each own their own slice —
  so each store's owner can revise its own number independently?
- Has any real Kelvran operator (including the live Bedrock pilot) actually received a deletion or
  access request yet, or is this pass, like the prior compliance report, still anticipatory ahead of
  real demand?
- If the deferred write-time PII-provenance index (needed for a real per-subject sweep) is ever
  built, should it also cover the budget-spend boltstore and audit log, or only the cache — given
  the three stores now have three different plausible answers to "should this be deletable at all"?

## Caveats

Several claims investigated this round did **not** survive adversarial verification and are
excluded above: the ICO's "beyond use" standard for data that can't be instantly purged (e.g.
backups) as a model for Kelvran's caches (refuted 0-3); a blanket "storage limitation requires
periodic review + an absolute erasure right once data is no longer needed" claim (refuted 1-2); a
CCPA "debugging exception" claim that would have directly covered Kelvran's own use of cached
content/audit logs for debugging (refuted 0-3 — do not rely on this as a justification); Anthropic's
own ZDR/default-retention framing as directly comparable precedent (three separate claims, all
refuted 0-3/1-2 — Anthropic's specific figures should not be cited here); a stronger "OpenAI's
default abuse-monitoring retention is capped at 30 days" claim (refuted 1-2 — only the ZDR and
stateful-object figures above are confirmed, not a general 30-day cap on everything); and two
LiteLLM/Kong claims about spend-log TTL defaults and semantic-cache TTL granularity that did not
hold up (both 0-3). Time-sensitivity: UK GDPR's erasure-clock start point is affected by the Data
(Use and Access) Act 2025 (effective 5 Feb 2026, a new Article 12A "relevant time" concept) — this
doesn't change the one-month *duration* Finding 6 cites, but revisit if that Act's implementation
guidance hardens further. The LiteLLM source-code verification (Finding 4) was done via a shallow
clone of the current `main` branch on 2026-09-16 — a point-in-time check, not a pinned-version
guarantee. As with the prior compliance report, this pass deliberately treats Kelvran-the-project
and "an operator running Kelvran" as distinct parties with distinct obligations — most of the legal
citations above describe what the *operator* would need to satisfy, not Kelvran itself, which has
no customers or data-subject relationships of its own.

## Sources

- https://ico.org.uk/for-organisations/uk-gdpr-guidance-and-resources/individual-rights/individual-rights/right-to-erasure/ (Findings 2, 6)
- https://ico.org.uk/for-organisations/uk-gdpr-guidance-and-resources/data-protection-principles/a-guide-to-the-data-protection-principles/storage-limitation (Finding 1)
- https://leginfo.legislature.ca.gov/faces/codes_displaySection.xhtml?lawCode=CIV&sectionNum=1798.105 (Findings 6, and cascade half of Finding 3)
- https://cppa.ca.gov/pdf/20260101_ccpa_statute.pdf (Finding 1, retention-disclosure/storage-limitation text)
- https://developers.openai.com/api/docs/guides/your-data (Finding 5)
- https://docs.llmgateway.io/features/data-retention (Finding 5)
- https://docs.litellm.ai/docs/proxy/spend_logs_deletion (Finding 4)
- https://docs.litellm.ai/docs/proxy/db_info (Finding 3, LiteLLM_AuditLog default)
- https://docs.litellm.ai/docs/proxy/ui_logs (Finding 4)
- `gateway/internal/cache/port.go` (`Cache.Delete`, lines 38-52) — verified directly against source
- `gateway/internal/cache/inprocess/inprocess.go` (`Delete`, TTL/`expiresAt` fields, lines 33-41, 159-172) — verified directly against source
- `gateway/internal/budget/boltstore/boltstore.go` (`Store.Load`/`Store.Save`, no `Delete`) — verified directly against source
- `gateway/internal/admin/admin.go` (unconditional `logger.Info` audit calls on every write/read route, no toggle) — verified directly against source
- `gateway/cmd/gateway/main.go:167` (`slog.NewJSONHandler(os.Stdout, nil)`) — verified directly against source
- `docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md` §4 (90-day S3 lifecycle retention precedent, and its own "no compliance requirement is documented" admission) — verified directly against source
- `THREAT_MODEL.md` `2026-09-14` Change Log entry (`Cache.Delete` shipped) — verified directly against source
- `docs/upgrade-research/ai-compliance-regulatory-readiness-2026-09-14.md` (prior, narrower pass on the cache-erasure half of this question — not re-litigated, only extended)
