# Kelvran Response Cache — Legal/Compliance Risk of Caching LLM Outputs (2026-09-22)

**Date:** 2026-09-22
**Scope:** `gateway/internal/cache/` — legal/compliance exposure specific to caching and re-serving *model-generated response bytes* (L1 exact-match, L2 normalized-match, L3-lite lexical near-duplicate), not the gateway's broader routing/adapter surface and not a repeat of prompt/input-side compliance questions. Three angles, per the research brief: (1) copyright exposure of serving cached model output verbatim to more than one recipient, (2) PII regurgitation when a cached response is served to someone other than the person whose prompt produced it, (3) regulatory guidance that speaks specifically to caching *outputs* (as opposed to inputs/training data). Deliberately does not re-litigate `docs/upgrade-research/ai-compliance-regulatory-readiness-2026-09-14.md` (EU AI Act "AI system" scoping, SOC 2, `PROVIDERS.md` cross-border-transfer disclosure, ISO 42001/NIST Govern) — see that report for those.
**Consolidation note:** this document merges two independent research passes run the same day against the same question. **Pass 1** (Findings 1-5) used direct repo-code verification (`gateway/internal/cache/port.go`, `key.go`, `identity/identity.go`, `dataplane.go`) cross-checked against primary EU/US legal sources — the code-grounded, Kelvran-specific half. **Pass 2** (Findings 6-9) is a broader, adversarially-verified (3-vote-per-claim) sweep of external regulatory guidance, litigation, and security research on LLM response caching specifically, independent of Kelvran's own code — the industry/legal-landscape half. Neither pass duplicates the other; Pass 2's claims are additive corroboration and new material, cross-referenced against Pass 1's findings inline. Per-claim vote counts below (e.g. "3-0", "2-1") refer only to Pass 2 claims; Pass 1 findings use the same confidence-tier language the sibling `gateway-2026-09-06.md`/`cache-2026-09-06.md` reports use.
**Method:** Pass 1 — primary-source research (US Copyright Office reports, live US federal court filings/opinions, EU AI Act statutory text and 2026 Commission guidelines, GDPR Article text and EDPB/ICO guidance, California statute and CPPA regulation text, Colorado legislative history) cross-checked against direct repo-code verification, plus current (2026-09) competitor-gateway documentation (LiteLLM, Portkey) as real-world grounding. Pass 2 — claims gathered and put to independent 3-vote adversarial verification against primary sources (EDPB Support Pool of Experts, U.S. Copyright Office, court filings in *NYT v. OpenAI/Microsoft*, the Italian Garante's OpenAI decision, peer-reviewed/preprint security papers, and cache-vendor documentation); 14 claims survived, 10 were refuted and are listed for transparency. Every load-bearing legal claim below is checked against 2+ independent primary or near-primary sources; confidence is graded per finding, not asserted uniformly.

---

## Executive summary

Response caching's compliance risk splits into a well-evidenced Kelvran-specific architectural gap and a near-total regulatory vacuum around caching *as its own risk category*. The single most consequential finding remains architectural: Kelvran's cache is partitioned by **tenant** (`identity.VirtualKey.ID`), not by **end user** — so in the gateway's own intended usage pattern (one VirtualKey per customer application, many human end users behind it), a response generated for End User A's prompt is, by design, eligible to reach End User B on a lexically-similar-enough query. This is not hypothetical: LiteLLM and Portkey both ship opt-in end-user/namespace cache scoping for exactly this failure mode, and 2026 security research independently demonstrates the same failure class is exploitable at scale — a semantic-cache key-collision attack achieving an 86% cross-tenant response-hijacking hit rate in a multi-tenant LLM-agent setting, and a peer-reviewed timing side-channel that lets one tenant infer another's private prompt content purely from cache hit/miss latency. Despite this, no primary regulatory or standards source found — including the EDPB's own dedicated LLM-privacy-risk report and a 2026 KV-cache security paper that explicitly distinguishes its own threat model from cross-tenant cache-sharing attacks — treats "caching across recipients" as a named, addressed risk category; each either mentions caching only as a performance optimization or scopes the cross-tenant question out entirely. On copyright, the U.S. Copyright Office's and *Thaler v. Perlmutter*'s settled position that purely AI-generated text carries no independent U.S. copyright means re-serving a cached response verbatim infringes no copyright *in the output itself* — but caching may still amplify the separate, live, actively-litigated risk that a model regurgitates a third party's *copyrighted training material*; *NYT v. OpenAI/Microsoft* is still pressing that theory after its narrower DMCA §1202(b)(3) distribution claims were dismissed (without prejudice, on fragmentation/substantiality grounds, not on any caching-related theory). The closest real regulatory precedent for cross-user response delivery — Italy's Garante finding that a load-balancing/queueing defect delivered one ChatGPT user's response to another, a GDPR Article 33 breach — is a live-routing bug, not persistent store-and-reuse caching, and critically, none of the resulting €15M fine's actual legal theories were built around caching or output reuse as such (and that fine was itself later annulled on appeal). Net: the technical risk of cross-tenant response-cache leakage is real, demonstrated, and already mitigated-by-others in the gateway-vendor space; the legal exposure is real but almost entirely inferential — organizations caching LLM responses across tenants are operating ahead of explicit, caching-specific regulatory guidance, relying on general privacy-breach and copyright doctrine applied by analogy rather than on-point rules.

---

## Findings, ranked

### 1. Kelvran's cache is tenant-scoped, not end-user-scoped — the real, currently-live cross-recipient regurgitation vector (Medium)

**What:** Direct code verification: `cache.Cache.Get`/`Put`/`Delete` (`gateway/internal/cache/port.go`) and `cache.Key`/`NormalizedKey` (`gateway/internal/cache/key.go`) take exactly one identity parameter, `tenantID` — one organization's/application's API key, not an individual human. `identity.VirtualKey` has no end-user concept nested beneath it, and `adapter.ChatRequest` has no `user`/end-user-identifier field at all. L3-lite's lexical near-duplicate matching is likewise tenant-partitioned, not end-user-partitioned. Every dataplane cache call site threads the same tenant-level ID through L1/L2/L3.

**Why it matters for Kelvran specifically:** the gateway's own intended deployment shape — one VirtualKey per customer *application*, many different human end users authenticating to that application — means a real, currently-undocumented isolation gap: if End User A's prompt contains (or causes the model to echo back) A's own PII, or third-party PII A pasted in for summarization, and End User B later sends a lexically similar (L2/L3) or byte-identical (L1) query through the same VirtualKey, B can receive A's cached response verbatim. Ordinary, non-adversarial multi-user traffic — not a cache-poisoning attack — can trigger this.

**Consistent with settled decisions?** Additive to `THREAT_MODEL.md`'s Cache Tampering/Elevation-of-Privilege rows (those cover a wrong-content hit; this is a right-content hit reaching the wrong recipient) and to the 2026-09-14 compliance report's `Cache.Delete` finding (that covers removing an entry on request; this covers whether the entry should have been eligible to reach a second recipient at all).

**2026 best-practice grounding (Pass 1):**
- LiteLLM's live docs (`docs.litellm.ai/docs/proxy/caching_semantic`, fetched 2026-09-11): a semantic cache key by default leaves the prompt out of tenant scoping, so "every end user behind one virtual key...shares one semantic bucket by default, and a response generated for one of them...can be served to another." Its shipped mitigation is `semantic_cache_scope: end_user`.
- Portkey's docs independently confirm the same default-partition shape and offer a namespace-override mechanism for the same problem.

**Corroborating evidence from Pass 2 (new, 2026 security research — see Finding 6 for full detail):** the CacheAttack key-collision paper (arXiv 2601.23088v2, ICML 2026) demonstrates an 86% cross-tenant response-hijacking hit rate against exactly this class of architecture in a multi-tenant LLM-agent setting; a separate peer-reviewed timing side-channel paper (arXiv 2409.20002, IEEE TIFS) shows semantic response caches leak private prompt content across tenants purely from hit/miss latency. Neither paper is about Kelvran, but both confirm the failure mode Finding 1 names is an active target of 2026 security research, not a theoretical concern this report is manufacturing.

**Concrete next step:** add an optional, client-supplied end-user identifier to `adapter.ChatRequest`, thread it as an additional optional fold-in field into `cache.Key`/`NormalizedKey` and L3's per-tenant partition key. Strictly additive/opt-in — a request that omits it falls back to today's tenant-only scope.

**Effort:** Medium — one new optional request field, wiring into 3 existing key-construction call sites, no interface-breaking change to `cache.Cache` itself.

---

### 2. GDPR: cross-end-user cache serving is a candidate personal-data-breach-by-design (Medium)

**What:** GDPR Article 4(12) defines a "personal data breach" to include "unauthorised disclosure of, or access to, personal data" — the EDPB's Guidelines 9/2022 confirm this covers "disclosure of personal data to...recipients who are not authorised to receive...the data." A cache hit serving End User A's cached response (containing A's own PII, or third-party PII A's prompt caused the model to echo) to End User B fits this directly. Article 33 requires supervisory-authority notification within 72 hours unless the breach is "unlikely to result in a risk"; Article 34 requires notifying the affected individual if the risk is "high."

**Why it matters for Kelvran specifically:** the 2026-09-14 report's `Cache.Delete` fix closes the *erasure* gap (Article 17) but does nothing for *this* gap — Articles 4(12)/33/34 concern whether an unauthorized disclosure happened at all, a controller-side design question a data subject cannot request their way out of after the fact.

**Real-world regulatory precedent (Pass 2, new — see Finding 9 for full detail):** Italy's Garante found, in a binding administrative decision, that a code defect in OpenAI's load-balancing/queueing layer caused "duplication of traffic and a rejoining of the flow on separate queues referable to different users" — i.e., a generated response for one user's prompt reaching a different user — and treated this as an Article 33 violation (confirmed 2-1, primary source: the Garante's own Provvedimento n. 755/2024). This is the closest real regulatory finding that the underlying harm (cross-user response delivery) constitutes a notifiable breach. **Important limit, corroborated 3-0:** the defect was a live traffic/queue-routing bug, not persistent store-and-reuse caching, and none of the fine's own legal theories (training-data legal basis, transparency, age verification, breach notification, non-compliance with a prior order — confirmed 3-0 against the same primary source) were built around caching or output-reuse as a distinct theory; the fine itself was later annulled on appeal by the Rome Tribunal (2026-03-18). So the *harm* has real regulatory recognition; the *caching mechanism specifically* as a legal theory does not yet.

**Concrete next step:** once Finding 1's end-user scoping option exists, add an operator-facing note in `THREAT_MODEL.md` naming this disclosure risk explicitly, citing both the GDPR text and the Garante precedent as the closest real-world analog, with the caveat that the analog is imperfect (routing bug vs. persistent cache).

**Effort:** Medium (documentation once Finding 1 ships).

---

### 3. US state privacy law: California already treats AI output as PI, and has separately excluded "caching" from ADMT (Small)

**What:** **AB 1008** (in force) amended Cal. Civ. Code § 1798.140(v)(4)(C) so "personal information" explicitly includes "artificial intelligence systems that are capable of outputting personal information." **The CPPA's ADMT regulation** (effective 2026-01-01) explicitly excludes "caching" from the definition of regulated automated-decisionmaking technology, provided it does not replace human decisionmaking.

**Why it matters for Kelvran specifically:** the first gives real statutory teeth to Finding 1/2's disclosure concern under a second legal regime (a cross-user-served cached response containing PI is now unambiguously a disclosure of "personal information" under California's own definition); the second forecloses a theory this report would otherwise have had to leave open — that caching itself could be swept into a state ADMT-style regime.

**Concrete next step:** add a short note to `PROVIDERS.md` cross-referencing AB 1008 against Finding 1/2's disclosure risk, and add the CPPA carve-out as a US-law parallel to `THREAT_MODEL.md`'s existing EU AI Act scoping note.

**Effort:** Small — documentation only.

---

### 4. Copyright: no U.S. copyright attaches to the cached output itself, but caching plausibly amplifies a real, distinct, actively-litigated regurgitation theory (Unscoped — needs legal-counsel-grade analysis, not a code fix)

**What:** Two separate questions, kept apart deliberately:
1. *Does caching-and-re-serving create new copyright exposure in the AI-generated text itself?* The U.S. Copyright Office's Part 2 report and *Thaler v. Perlmutter* (130 F.4th 1039) together hold that purely AI-generated text carries no U.S. copyright absent sufficient human authorship — confirmed further by Pass 2 (3-0): "prompts alone" do not provide sufficient human control to make a user the author of an LLM's output, so the mere fact that a user supplied the prompt behind a cached response does not give that user (or anyone) a copyright claim over the cached text. Re-serving it verbatim to a second tenant therefore infringes no one's copyright *in that text*.
2. *Does caching amplify the risk of a model regurgitating a third party's copyrighted training material?* This is real and live. Pass 2 confirms (3-0) the EDPB Support Pool of Experts report states outputs replicating protected training content "create legal risks for both providers **and deployers**" — not the model vendor alone, a point directly relevant to anyone operating a caching gateway. *NYT v. Microsoft/OpenAI* (S.D.N.Y., ongoing) alleges GPT-family models can output "near-verbatim copies (memorizations)" of Times articles (confirmed 2-1, corroborated by a peer research doc that independently fetched the original complaint and 2026 third amended complaint). A concrete, newer procedural detail (confirmed 3-0): the court dismissed the DMCA §1202(b)(3) *distribution* claims specifically because the cited regurgitations were "partial excerpts...generated across multiple prompts, reordered relative to the original," not substantial/entire reproductions — dismissed **without prejudice**, and on fragmentation/substantiality grounds, not on any theory touching caching or redistribution mechanics. **The genuinely open question this report surfaces and does not answer, because no authority answers it:** if a single regurgitation is generated once and then served via cache to N different recipients instead of each separately triggering (and separately risking) their own generation, does that change the distribution-law analysis — turning one act of generation into what functions as N acts of republication? No court, regulator, or Copyright Office report addresses this.
3. **Scope note confirmed by Pass 2 (2-1):** the EDPB's own LLM-privacy report mentions caching exactly once — "cache-augmented generation (CAG)," framed purely as a latency/cost/consistency optimization — and its recommended mitigation for memorization risk (differential privacy + regular regurgitation testing, confirmed 2-1) is explicitly scoped to *training-data* memorization, not to caching or prior-session-input reuse. Neither the EDPB report nor any other primary source found treats output caching itself as a privacy or copyright risk vector; the risk this finding describes is inferred by applying the report's general regurgitation concern to a caching context, not stated by the report itself.

**Why it matters for Kelvran specifically:** Kelvran's cache is provider-agnostic and stores exactly what an upstream model returned — it has no visibility into whether a cached response verbatim-reproduces a third party's copyrighted work. Kelvran's cache is a pure *amplifier* of whatever infringement risk already exists in a generation; it cannot introduce new infringing content, but can multiply how many recipients receive an already-infringing one for as long as it stays cached.

**Concrete next step:** a short, honestly-scoped compliance note stating: (a) no incremental US copyright liability in the cached text as a work — high confidence; (b) caching may amplify an existing upstream regurgitation risk the cache layer cannot detect or filter, and the specific caching-amplification-of-distribution-liability question is legally unresolved anywhere — flag as open for outside counsel, not a house position.

**Effort:** Unscoped for code — a documentation/disclosure item only.

---

### 5. EU AI Act Article 50 (content-marking transparency) is now binding — a cache-specific check that hadn't been run before (Small)

**What:** Article 50(2) became legally applicable 2 August 2026. Direct code verification confirms `cache.Cache.Get`/`Put` deal only in opaque `resp []byte` — the cache never parses or modifies response content, so any machine-readable mark a provider embeds in a response body survives a cache hit unchanged, for every recipient.

**Concrete next step:** a one-paragraph addition to `THREAT_MODEL.md` stating cache hits preserve response-body content byte-for-byte, so Article 50(2) marking travels correctly through every cache layer.

**Effort:** Small — no code change; the underlying property already exists.

---

### 6. Response/semantic-cache cross-tenant leakage is empirically demonstrated by 2026 security research, not just vendor-doc theory (Medium-High confidence)

**What:** Three independent, primary-sourced findings converge on the same conclusion Finding 1 reaches from Kelvran's own code, from the opposite direction — external security research treating this as a live attack surface:
- **CacheAttack** (arXiv 2601.23088v2, ICML 2026-accepted): "the first key collision attack to semantic caching in a multi-tenant setting of LLM agents," achieving an 86% hit rate in LLM response hijacking — an attacker plants a cache entry that gets served to a different, benign victim user via a semantic-cache key collision (confirmed 2-1).
- **Semantic-cache mechanics generally**: systems like GPTCache "store prior requests and their corresponding responses" and serve the cached response to a later, *different* user whose query is merely semantically similar, not identical — true output/response caching is shared across users by construction, not just KV/prefix state (confirmed 3-0, arXiv 2409.20002).
- **Timing side channel**: the same architecture creates a measurable latency gap between cache hit and cache miss that can be exploited to expose another user's or application's private prompt content across tenants — a peer-reviewed (IEEE TIFS) "Peeping Neighbor Attack" (confirmed 2-1).

**Why it matters:** these are independent confirmations, from the security-research literature rather than gateway-vendor marketing, that the exact failure class Finding 1 names in Kelvran's own architecture is a real, currently-exploitable, and currently-studied risk across the industry in 2026 — not a hypothetical this report invented.

**Important scope limit, also confirmed (3-0):** a separate, closely-related paper on KV-cache security (arXiv 2508.09442v4, NDSS 2026) explicitly casts the *inference service provider itself* as its adversary (direct architectural access to KV-cache) and explicitly distinguishes this from cross-tenant cache-sharing side-channel attacks, attributing those to prior work. That paper does **not** study a scenario where one tenant's cached content is later served to a different tenant — the mechanism central to this research question. This is a useful negative finding: not every "LLM cache security" paper addresses cross-tenant response serving, and citing one that doesn't (as if it did) would overstate the evidence base.

**Effort:** N/A — this is external corroboration for Finding 1's `Concrete next step`, not a separate action item.

---

### 7. No regulator or standards body has yet named "response-cache cross-tenant serving" as its own risk category — a real gap, not a "no risk" finding (Medium confidence)

**What:** Despite Finding 6's demonstrated technical risk, no primary source found treats caching-across-recipients as a distinct compliance category. The EDPB's dedicated LLM-privacy-risk report mentions caching exactly once, as a pure performance optimization (Finding 4, item 3, confirmed 2-1). The KV-cache security paper in Finding 6 explicitly scopes cross-tenant cache-sharing attacks out of its own threat model, attributing that class to a different paper it does not itself extend (confirmed 3-0).

**Why it matters:** this is an absence-of-evidence finding, not evidence of absence of risk — it means any argument that caching-across-tenants is compliant, low-risk, or already addressed by existing guidance is not supportable by anything found in this research. Operators (and Kelvran's own documentation) should not cite "no regulator has flagged this" as reassurance; it more likely reflects that response-cache-specific rules simply haven't been written yet.

**Concrete next step:** state this gap explicitly in whatever compliance note Finding 1/2 produces, rather than silently relying on the absence of caching-specific regulatory language as implicit permission.

**Effort:** Documentation framing only.

---

### 8. Popular LLM-cache/gateway vendors confirm the cross-tenant-by-design pattern is industry-wide, not a Kelvran-specific oversight (Medium confidence, with an unresolved Portkey-specific caveat)

**What:** Beyond Finding 1's LiteLLM/Portkey citations, Pass 2 independently confirms the underlying mechanics:
- **Portkey**: semantic cache matching ignores the system prompt entirely and matches on cosine similarity of the user-message content — a cached response originally produced for one prompt can be served to a different, merely semantically-similar query rather than only an identical one (confirmed 3-0, cosine threshold default 0.95).
- **Helicone**: its cache key is a hash of the request URL, complete request body, and "relevant headers" including `Authorization` (plus an optional cache seed) — scoped per API key by default, not per end-user identity. In a multi-tenant app sharing one backend API key, a cached response generated from one user's prompt (which may contain that user's PII) can be served verbatim to a different end user with no additional isolation unless the developer opts in to a cache seed (confirmed 3-0).

**Unresolved caveat, worth stating plainly:** two related, more specific Portkey claims did **not** survive adversarial review and are refuted: that Portkey's cache *defaults* to partitioning by all request headers (hence isolating tenants by default) was refuted (0-3), and that Portkey's `cache_namespace` override is specifically documented for "per-user caching" was also refuted (0-3). A related Helicone claim — that Helicone's docs *explicitly* instruct developers to add a cache seed per user/context — was refuted on a closer 1-2 vote. Net: the *matching mechanism* (similarity-based, not identity-based) is well-confirmed for both vendors; the *exact default isolation behavior* is not settled either way by the surviving evidence in this pass, and should not be asserted confidently in either direction without a dedicated follow-up read of each vendor's current docs.

**Why it matters:** two more real, currently-shipping gateway products exhibit the same structural pattern Finding 1 identifies in Kelvran — reinforcing that end-user-level cache scoping is an emerging, not yet universal, industry mitigation, and that "similarity-based matching crossing distinct-but-similar prompts" is normal, intended cache behavior across vendors, not a bug specific to any one of them.

**Effort:** N/A — corroborating context for Finding 1.

---

### 9. The closest real regulatory precedent for cross-user response delivery is a routing defect, not caching — and its own fine was later annulled (Medium confidence)

**What:** Covered in Finding 2 above; restated here as its own finding because it cuts two ways. Italy's Garante's official investigation attributes OpenAI's March 20, 2023 ChatGPT breach to "errors in some lines of code" causing "duplication of traffic and a rejoining of the flow on separate queues referable to different users" — a real, regulator-documented instance of one user's generated response reaching a different user, treated as a GDPR Article 33 violation (confirmed 2-1). But the €15,000,000 fine that resulted was built entirely on training-data legal basis, transparency/notice, age-verification, breach-notification, and non-compliance-with-a-prior-order theories — **not** on caching or output-reuse as a distinct legal theory (confirmed 3-0) — and that decision was subsequently overturned on appeal by the Rome Tribunal (2026-03-18, noted in the same verification pass).

**Why it matters:** this is simultaneously the best available regulatory evidence that regulators *will* treat cross-user response delivery as a notifiable breach (supporting Finding 2's GDPR framing), and clear evidence that no regulator has yet built an enforcement theory specifically around *caching* as opposed to a live routing defect — and that even this closest analog no longer stands as a final, un-appealed decision. Cite it as informative precedent for the underlying harm, not as settled law on caching.

**Effort:** N/A — informs Finding 2's framing; no independent action item.

---

## Refuted claims (for transparency)

The following claims were checked in Pass 2 and did **not** survive adversarial verification — listed so they are not later re-asserted as if confirmed:
- U.S. Copyright Office's conclusion on no-copyright-for-AI-output, reasserted in a broader/cached-response framing (0-3 — the narrower framing in Finding 4 item 1 did survive; this broader restatement did not).
- Norton Rose Fulbright's claim that infringement exposure is highest when output is used/exposed publicly vs. privately (0-3).
- A DMCA §1202(b)(1) CMI-at-dissemination theory in the NYT complaint, framed as a caching-and-redistribution analog (0-3).
- A "contributory infringement via ongoing relationship" theory distinguishing *NYT v. OpenAI* from *Sony* (0-3).
- Semantic response caching being deployed specifically in cross-tenant configurations as a deliberate cost-cutting measure (0-3).
- GPTCache/NDSS paper explicitly flagging response-cache sharing as unstudied future work, plus a claimed March 24, 2023 ChatGPT Redis-connection-pool title-leak incident (0-3) — note this is a *different*, unconfirmed incident from the Garante-documented load-balancing breach in Finding 9, which did survive verification.
- A claim that ChatGPT retained PII verbatim in 57.4% of cover-letter-summarization cases, sourced to `arxiv.gg` (0-3) — the domain itself (not `arxiv.org`) is a red flag; treat as likely fabricated/non-existent and do not re-cite.
- Portkey defaulting to per-header (hence per-tenant) cache partitioning (0-3).
- Portkey's `cache_namespace` being documented specifically for "per-user caching" (0-3).
- Helicone's docs explicitly instructing developers to add a cache seed per user/context (1-2).

---

## Top 4 do next

1. **Add an optional end-user identifier to the cache key (Finding 1).** Now doubly motivated: LiteLLM/Portkey ship this as a real mitigation (Pass 1), and 2026 academic security research (Finding 6) independently demonstrates the failure mode at an 86% hit rate. Everything else in this report is documentation-only or genuinely unscoped; this is the one item that closes a live gap.
2. **Document the cross-end-user disclosure risk, cross-referenced against GDPR (Finding 2), CCPA/AB 1008 (Finding 3), and the Garante precedent (Finding 9) — explicitly noting the precedent is a routing-bug analog, not on-point caching law, and that no regulator has yet named caching-across-tenants as its own risk category (Finding 7).**
3. **Write the honestly-scoped copyright note (Finding 4)**, now sharpened by the confirmed EDPB deployer-liability framing and the NYT case's specific (and narrower-than-assumed) DMCA dismissal — state plainly that the caching-amplifies-distribution-liability question is open, not resolved either way.
4. **Do not cite the absence of caching-specific regulatory guidance as reassurance (Finding 7).** Flag it as a gap in the same document as items 1-3, so a future reader doesn't mistake regulatory silence for regulatory clearance.

---

## Caveats

- This report consolidates two independently-run passes at the same file path on the same day; Pass 1's Kelvran-code findings (1-5) were not re-verified against Pass 2's claims, and Pass 2's external claims were not checked against Kelvran's code — the cross-references above are a synthesis-time judgment call, not independently re-verified overlap.
- Pass 1 deliberately did not re-read `data-retention-right-to-erasure-2026-09-15.md` or `multi-tenancy-access-control-2026-09-14.md` before writing Findings 1-3 — recommend a follow-up cross-read to reconcile Finding 1's end-user-scoping recommendation against whatever those docs already concluded about tenancy boundaries.
- Findings 2, 3, and 9's litigation/breach-notification characterizations are reasoned applications of settled statutory text and one regulatory decision to a new fact pattern (LLM response caching), not citations to a case or ruling squarely on point — no such on-point authority was found. Treat the underlying statutory/decision text as high confidence and the specific application to caching as medium confidence.
- Finding 4's central question (does caching's amplification of a single generation to many recipients change copyright's distribution analysis) is genuinely open — no primary source, across a deliberately broad search in either pass, addresses it either way.
- Portkey's actual default tenant-isolation behavior for cache hits is unresolved by this research (Finding 8) — two specific claims about it were refuted on review. Do not assert Portkey's default isolation posture in either direction without a dedicated, current-docs-only follow-up.
- The Garante ChatGPT breach (Finding 9) is a live traffic/queue-routing defect, not the store-and-reuse-over-time caching pattern Kelvran implements — an analogy, not a literal precedent — and the resulting fine was itself later annulled on appeal (Rome Tribunal, 2026-03-18).
- The CacheAttack paper (Finding 6) is very recent (ICML 2026-accepted, updated June 2026); this is current, not stale, evidence, but it is also new enough that no defensive best-practice has yet crystallized around it industry-wide.
- One candidate source (`arxiv.gg`, claiming a 57.4% PII-retention rate) was correctly refuted during verification and appears to be a fabricated or non-existent citation — flagged explicitly so it is not mistakenly re-surfaced in a future pass.
- Pass 2's Method (3-vote adversarial verification) and Pass 1's Method (single-path primary-source research plus direct code verification) are different rigor profiles; per-finding confidence labels reflect this honestly rather than applying one uniform standard across both.

## Open questions

- Does caching-and-re-serving a single AI-generated output to multiple recipients change the copyright distribution-law analysis relative to each recipient separately triggering their own generation (Finding 4)? No authority answers this.
- Should `semantic_cache_scope`-style end-user scoping (Finding 1) be opt-in per request, opt-in per VirtualKey/tenant configuration, or eventually the default?
- Does any regulator (EDPB, CPPA, ICO, or a national DPA) plan to address LLM response-caching-across-recipients as its own named risk category, given both the EDPB's LLM report and the KV-cache security paper explicitly scope it out or ignore it (Finding 7)?
- What is Portkey's actual current default tenant/end-user isolation behavior for semantic-cache hits — unresolved by this pass after two related claims were refuted (Finding 8)?
- Would a regulator characterize a semantic-cache cross-tenant hit (of the kind Finding 6's academic papers demonstrate) as a notifiable GDPR Article 33 breach the way the Garante treated OpenAI's 2023 load-balancing bug, given the underlying harm (response A reaching user B) is functionally similar even though the causal mechanism (routing bug vs. cache design) differs (Finding 9)?
- Does `multi-tenancy-access-control-2026-09-14.md` or `data-retention-right-to-erasure-2026-09-15.md` already address or contradict any part of Findings 1-3?
