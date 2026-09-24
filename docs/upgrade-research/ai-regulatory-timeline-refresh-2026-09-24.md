# AI Regulatory Timeline Refresh (2026-09-24)

**Date:** 2026-09-24
**Scope:** AI-specific regulatory compliance timelines relevant to a self-hosted LLM gateway operator — what's newly binding or newly guided on the EU AI Act since Article 50's 2 Aug 2026 binding date (already documented in Kelvran's `THREAT_MODEL.md`); new US state-level AI legislation relevant to an operator handling PII; and whether NIST, ISO/IEC 42001, or SOC 2 have published new AI-specific guidance relevant to self-hosted (not SaaS) AI infrastructure.
**Method:** Adversarial multi-source research (3-vote verification), grounded first against Kelvran's own documented compliance posture (`SECURITY.md`'s "Compliance Evidence for Operators," `THREAT_MODEL.md`'s "EU AI Act Scoping Rationale," `docs/operations/PROCUREMENT.md`) so only genuinely new items — not restatements of Kelvran's existing GDPR/EU-AI-Act/HIPAA/FedRAMP positions — are reported as findings. Every claim below is grounded in an official regulatory source (eur-lex.europa.eu, the EU AI Act's own official text/guidance, a US state legislature's own bill text, or NIST/AICPA's own publications) — never a law firm's marketing blog post as the sole source; where a claim originated from a law-firm aggregator, it was independently re-verified against a primary government source before being included below. 20 of 25 extracted claims survived adversarial verification; 5 were refuted and are excluded.

**Recovery note:** this report's synthesis was completed by the research workflow (grounded, live-verified, 20 confirmed claims) but the workflow's own file-write step did not persist it to this path despite the task-completion notification reporting success. This document was reconstructed directly from that already-completed synthesis (recovered from the workflow's own journal), not re-researched — no findings below are new since the original run, only the persistence step was missing.

---

## Executive summary

Since Kelvran's existing EU AI Act note (the 2 Aug 2026 binding date for Article 50), the EU adopted **Regulation (EU) 2026/1744**, the "Digital Omnibus on AI" (in force 27 July 2026), which grandfathers Article 50(2) marking/detection obligations for pre-existing generative AI systems until **2 December 2026** — a genuinely new, imminent phase-in date not yet in `THREAT_MODEL.md` — alongside finalized Commission Article 50 guidelines and an endorsed (voluntary) Code of Practice. On the US side, four new/amended state laws are relevant to a PII-handling gateway operator: Connecticut's SB5/PA 26-15 (employer AI-decision disclosure, effective **1 October 2026** — days away from this report's date), Colorado's SB189 (a narrower ADMT transparency regime replacing the 2024 Colorado AI Act, effective 1 January 2027), and California SB53/New York's RAISE Act (frontier-model-developer safety laws, likely inapplicable to Kelvran since it neither trains nor fine-tunes models near their 10^26-FLOP/$500M-revenue thresholds). NIST published a new, distinct Cyber AI Profile (IR 8596, Initial Preliminary Draft, Dec 2025) mapped onto CSF 2.0 — not yet in Kelvran's NIST AI 600-1 crosswalk — while its AI RMF 1.0 revision remains pending and its crosswalks page has had no new entry in ~13 months. AICPA/SOC 2 still has zero AI-specific Trust Services Criteria or SOC 2 addendum as of the research window, confirming and extending (with new specifics) Kelvran's existing "SOC 2 doesn't apply to OSS" position.

A live grep against `THREAT_MODEL.md`, `SECURITY.md`, and `docs/operations/PROCUREMENT.md` confirmed none of these items (Digital Omnibus, the December 2026 grace period, IR 8596, the four state laws, or the AICPA gap) appear anywhere in Kelvran's current documented compliance posture — all are genuinely new findings, not restatements.

---

## Findings, ranked

### 1. The Digital Omnibus's 2 December 2026 grace-period deadline is the concrete "what's next" EU milestone — not yet in `THREAT_MODEL.md` (High confidence)

**What:** The EU adopted Regulation (EU) 2026/1744, the "Digital Omnibus on AI" (amending the AI Act 2024/1689 plus the EASA and Machinery Regulations), signed 8 July 2026, published in the OJ 24 July 2026, in force since 27 July 2026. It created a targeted grandfathering rule: generative AI systems already on the market before 2 August 2026 get until **2 December 2026** to bring Article 50(2) marking/detection into conformity (new systems still had to comply from 2 August 2026, with no grace period).

**Why it matters for Kelvran specifically:** this is the concrete next phase-in milestone after the already-documented 2 August 2026 date, and it is imminent — about 10 weeks out from this report's date. Live grep confirmed zero hits for "omnibus," "december 2026," or "2026/1744" anywhere in `THREAT_MODEL.md`, `SECURITY.md`, or `docs/operations/PROCUREMENT.md` — this is a genuine gap in Kelvran's documented compliance posture, not a restatement.

**Do NOT reuse as established:** a related claim asserting Article 6(1)/high-risk classification on 2 August 2027 as the "next" milestone was refuted 0-3, and a claimed February 2027 watermark-interoperability deadline was refuted 1-2 — 2 December 2026 is the only confirmed next date.

**2026 best-practice grounding:**
- Verified directly against the EUR-Lex primary text and procedure-history page — number, date, title, and amended instruments all cross-corroborate with no contradiction [eur-lex.europa.eu — confirmed 3-0 on 2 of 3 sub-claims, 2-1 on exact publication/in-force dates].
- The 2 December 2026 grandfathering date is stated in the Commission's own official guidance PDF (C(2026) 5054 final), its quick-facts factpage, and its own FAQ page — three independent primary EC sources agree [ai-act-service-desk.ec.europa.eu, digital-strategy.ec.europa.eu — confirmed 3-0, 3-0, 3-0].

**Concrete next step:** add a dated entry to `THREAT_MODEL.md`'s EU AI Act Scoping Rationale section noting the 2 December 2026 grandfathering deadline for pre-existing generative-AI-system operators, distinct from the already-documented 2 August 2026 date for new systems.

**Effort:** Small — a docs-only addition, no code change.

---

### 2. The Commission finalized Article 50 transparency guidelines and an endorsed voluntary Code of Practice, both post-dating the binding date (Medium/High confidence)

**What:** The European Commission finalized its Article 50 transparency guidelines (following a draft dated 8 May 2026, consultation closed 3 June 2026) as C(2026) 5054 final (20 July 2026), with a public-facing page last updated 6 August 2026 — genuine post-2-August-2026 Commission guidance issued after Article 50 became binding. Separately, the Commission/AI-Board-endorsed voluntary Code of Practice on Transparency of AI-generated Content is a compliance-demonstration pathway for Article 50, not an independent source of new legal obligation — adherence is voluntary, but the underlying Article 50 duties remain binding regardless.

**Why it matters for Kelvran specifically:** confirms genuine, dated regulatory-guidance movement since the binding date, worth citing alongside the Finding 1 update, though it does not itself change what Kelvran's compliance posture needs to do — the underlying obligation is unchanged, only elaborated.

**2026 best-practice grounding:**
- Both EC pages independently confirmed mutually consistent, including a "Last update: 6 August 2026" footer [digital-strategy.ec.europa.eu — confirmed 2-1, medium confidence given split votes and the general caution about sandboxed-fetch mocking noted in this project's own prior research].
- The Code of Practice's own page explicitly states it "does not replace the AI Act or the Commission's guidelines on Article 50" [digital-strategy.ec.europa.eu — confirmed 3-0].

**Concrete next step:** cite the finalized guidance (C(2026) 5054 final) alongside the Finding 1 `THREAT_MODEL.md` update as the current authoritative Article 50 reference, superseding the draft version if one was previously cited anywhere.

**Effort:** Small — a citation update alongside Finding 1's docs change.

---

### 3. Connecticut's SB5 (employer AI-decision disclosure) takes effect within days of this report's date (High confidence)

**What:** Connecticut's SB5 (Public Act No. 26-15, signed May 2026) imposes automated-employment-decision AI disclosure obligations with staggered effective dates of **1 October 2026** and 1 January 2027.

**Why it matters for Kelvran specifically:** this is the most time-urgent item in this report — a binding obligation lands within days of the 2026-09-24 research date. Directly relevant to a self-hosted gateway operator processing employment-related PII (a real, if unresolved, scoping question — see Open Questions).

**2026 best-practice grounding:** Originally sourced from a law-firm aggregator (DLA Piper); independently re-verified against Connecticut's own official bill-status record (via Wayback Machine snapshot of `cga.ct.gov`) and a second non-law-firm tracker (multistate.us), closing the "sole law-firm source" gap in the original citation [confirmed high after upgrade from an initial 2-1 vote].

**Concrete next step:** flag this date explicitly in `docs/operations/PROCUREMENT.md` or `THREAT_MODEL.md` as a near-term US state-law watch item — but see Open Questions on whether it actually reaches a routing/gateway operator at all, as distinct from the entity making the employment decision downstream.

**Effort:** Small — a docs-only flag, pending the scoping question below.

---

### 4. Colorado's SB189 replaces the 2024 Colorado AI Act with a narrower ADMT transparency regime (High confidence)

**What:** Colorado's SB189 (SB26-189, signed 14 May 2026, effective 1 January 2027) repeals and replaces the 2024 Colorado AI Act's broad risk-based framework with a narrower automated-decision-making-technology (ADMT) transparency regime, while preserving individual rights to access/correct data used in automated decisions and request human review of adverse decisions.

**Why it matters for Kelvran specifically:** a materially different (narrower) obligation than what Kelvran may have previously scoped against the 2024 Colorado AI Act, if that was ever considered — worth a scoping re-check given the same open question as Finding 3 (does an ADMT definition reach a routing/gateway operator, or only the downstream decision-maker).

**2026 best-practice grounding:** Independently verified against the Colorado General Assembly's own bill page (a primary source superior to the cited secondary DLA Piper page) — signing date, Chapter 131 designation, 1 January 2027 effective date, and the repeal-and-reenact structure relative to SB24-205 all confirmed [leg.colorado.gov — confirmed 3-0].

**Concrete next step:** same as Finding 3 — flag as a watch item pending the ADMT-scope question in Open Questions.

**Effort:** Small — a docs-only flag, pending the scoping question below.

---

### 5. California SB53 and New York's RAISE Act — frontier-developer safety laws, likely inapplicable to Kelvran (Medium confidence)

**What:** California's SB53 (operative 1 January 2026) is the first US state frontier-AI-safety law (15-day incident reporting, flat $1M-per-violation cap). New York's RAISE Act (as amended by Ch. 96, signed 27 March 2026) is the second, covering "frontier developers" at a 10^26-FLOP training-compute threshold, with heightened duties for "large frontier developers" exceeding $500M annual revenue.

**Why it matters for Kelvran specifically:** both are likely inapplicable to Kelvran, which neither trains nor fine-tunes models anywhere near these thresholds — consistent with Kelvran's existing GPAI-provider scoping rationale already in `THREAT_MODEL.md`. Reported here as a confirmed non-gap, not a new obligation.

**2026 best-practice grounding:**
- California SB53 details independently corroborated against the bill's own statutory text and the Governor's signing announcement [leginfo.legislature.ca.gov — confirmed 3-0].
- New York RAISE Act's final $500M-revenue threshold independently verified against the actual chapter-amendment bill text (A9449/Ch. 96), correcting an earlier draft version's compute-spend threshold — a related claim asserting NY still uses a compute-spend rather than revenue threshold was refuted 1-2 [confirmed 2-1].
- Medium confidence overall because the applicability-to-Kelvran conclusion is an inference from the same GPAI/no-training logic already in `THREAT_MODEL.md`, not a directly sourced legal holding specific to Kelvran.

**Concrete next step:** none required — confirms the existing scoping rationale extends cleanly to these two new laws. Worth a one-line cross-reference in `THREAT_MODEL.md` if/when that section is next touched, purely for completeness.

**Effort:** N/A (no gap) / Trivial (optional cross-reference).

---

### 6. NIST's IR 8596 Cyber AI Profile — new, distinct from Kelvran's existing NIST AI 600-1 crosswalk (High confidence)

**What:** NIST published IR 8596, the "Cybersecurity Framework Profile for Artificial Intelligence (Cyber AI Profile)" — a joint NIST/MITRE community profile (Initial Preliminary Draft, 16 December 2025) that maps AI-specific outcomes onto the existing CSF 2.0 Functions/Categories/Subcategories across three focus areas (Securing AI components / AI-enabled defense / Thwarting AI-enabled attacks).

**Why it matters for Kelvran specifically:** distinct from and not currently referenced in Kelvran's NIST AI 600-1 crosswalk. Live grep confirmed zero mentions of "8596," "Cyber AI Profile," or "Cybersecurity Framework Profile" anywhere in `THREAT_MODEL.md`/`SECURITY.md`/`docs/operations/PROCUREMENT.md` — genuinely new-to-Kelvran. Still only an Initial Preliminary Draft as of the research window (no superseding version found).

**2026 best-practice grounding:** Verified directly against the NIST primary publication page [csrc.nist.gov — confirmed 3-0 on both existence/novelty and structural description].

**Concrete next step:** name as a candidate for a future NIST-crosswalk addition analogous to Kelvran's existing NIST AI 600-1 crosswalk — not urgent given the draft's still-preliminary status, but worth tracking for when/if it reaches final status.

**Effort:** N/A now (tracking only); Small-Medium if/when a crosswalk section is eventually written.

---

### 7. NIST's AI RMF 1.0 revision remains pending; the Crosswalks page has been stale for ~13 months (High confidence)

**What:** NIST's AI RMF 1.0 is still under revision (per the White House AI Action Plan) with no published successor, and NIST's own AI RMF Crosswalks page has had no new entry since 14 August 2025 (~13 months stale as of 2026-09-24).

**Why it matters for Kelvran specifically:** confirms there is no material new NIST cross-mapping guidance to react to beyond Finding 6's IR 8596 — a "nothing new here" finding worth stating explicitly so it isn't re-researched needlessly in a future round.

**2026 best-practice grounding:** Both independently re-verified via live fetch on the research date itself (not a cached snapshot) — the RMF 1.0 revision is still pending, and the crosswalks page's newest entries (ISO/IEC 23894 revised, ISO/IEC 42005 new) both remain dated 14 August 2025 with nothing newer added [nist.gov, airc.nist.gov — confirmed 3-0].

**Concrete next step:** none — a confirmed non-finding, useful mainly to close the question for this round.

**Effort:** N/A.

---

### 8. AICPA/SOC 2 still has zero AI-specific criteria — extends Kelvran's existing position with new specifics (Medium confidence)

**What:** As of the research window, AICPA has published no AI-specific Trust Services Criteria, points of focus, or SOC 2 addendum; auditors are improvising ad hoc AI evidence requests against the unchanged 2017 TSC. The only 2026 AICPA attestation-standard rulemaking (AT-C 105/205/210) is sustainability-focused, doesn't mention AI, and wouldn't take effect before mid-2029. The closest thing to an official AICPA AI statement — the Forensic and Valuation Services Executive Committee's early-2026 guidelines — explicitly disclaims authoritative status and addresses only CPAs' own use of AI tools, not auditing a client's AI system.

**Why it matters for Kelvran specifically:** extends (does not contradict) Kelvran's existing `SECURITY.md` position that SOC 2 is an attestation an operating organization produces, not a property of the OSS project itself — this finding adds concrete, dated specifics (the AT-C timeline, the Forensic/Valuation guidance's explicit non-authoritative disclaimer) that weren't previously documented.

**2026 best-practice grounding:** Sourced from a single CSA research note (self-disclosed as AI-assisted, not CSA-reviewed), but every specific fact within it was independently re-verified against AICPA-affiliated primary sources — AICPA's own Journal of Accountancy (quoting AICPA staff on the gap) and AICPA's own Forensic & Valuation Services guidance page directly [labs.cloudsecurityalliance.org, aicpa-cima.com — confirmed 3-0, 3-0, 2-1]. Medium confidence reflects the primary evidentiary document's own AI-assisted, non-reviewed disclosure.

**Concrete next step:** optional — add the specific AT-C timeline and Forensic/Valuation disclaimer as supporting detail if `SECURITY.md`'s existing SOC 2 position is next revised, purely for citation strength.

**Effort:** Trivial (optional docs enrichment).

---

## Top takeaways

1. **Add the 2 December 2026 Digital Omnibus grandfathering deadline to `THREAT_MODEL.md`** (Finding 1) — the single most concrete, dated, actionable item in this report, and genuinely missing today.
2. **Flag Connecticut SB5's 1 October 2026 effective date as urgent** (Finding 3) — it lands within days of this report, though whether it actually reaches Kelvran as a gateway operator (vs. the downstream decision-maker) is an open scoping question, not yet resolved.
3. **Two frontier-developer safety laws (Finding 5) and the NIST RMF status quo (Finding 7) are confirmed non-gaps** — useful to record so they aren't re-researched next round.
4. **NIST IR 8596 (Finding 6) and the AICPA specifics (Finding 8) are lower-urgency tracking items**, not action items — both still in draft/status-quo states.

---

## Caveats

- This is a synthesis of a completed adversarial-verification research pass (20 confirmed claims, 5 refuted), independently re-grounded by live-grepping Kelvran's `THREAT_MODEL.md`, `SECURITY.md`, and `docs/operations/PROCUREMENT.md` to confirm none of the above topics appear there today.
- The Connecticut and NY RAISE Act findings originated from a law-firm aggregator (DLA Piper) and required independent verification against primary government sources to meet this project's "no law-firm-blog-as-sole-source" bar — that verification was done in each case and is noted per-finding above.
- The AICPA finding's primary evidentiary document (a CSA research note) discloses it was AI-assisted and not institutionally reviewed — every specific fact within it was independently cross-checked against AICPA-affiliated primary sources regardless.
- Two adjacent claims were explicitly refuted during verification and must not be reused: "Article 6(1)/high-risk classification 2 August 2027 is the next EU AI Act milestone" (0-3) and "a February 2027 watermark-interoperability deadline" (1-2) — the only confirmed next EU date is 2 December 2026.
- Time-sensitivity: the Connecticut SB5 1 October 2026 effective date lands within days of this research's as-of date (2026-09-24) and should be treated as urgent for any downstream action.

## Open questions

- Given Kelvran neither trains nor fine-tunes models, does Colorado SB189's ADMT definition (or Connecticut SB5's employment-decision-tech definition) reach a routing/gateway operator at all, or only entities making the actual employment/consumer decision downstream of the gateway? This determinative-scope question wasn't resolved by the research and needs a Kelvran-specific legal read, not just the bill text.
- What is the actual next concrete EU AI Act phase-in milestone after 2 December 2026, given that the two candidate claims found (Article 6/high-risk 2 August 2027; February 2027 watermark interoperability) were both refuted during verification?
- Will NIST's IR 8596 Cyber AI Profile reach a non-draft (final) status before its next review cycle, and if so, should Kelvran add a crosswalk section analogous to its existing NIST AI 600-1 crosswalk?
- Are there other 2026 US state AI bills (beyond CT, CO, CA, NY) with employment- or PII-disclosure provisions that this research's narrower four-state focus might have missed — e.g., Texas TRAIGA or other states following the SB/AB pattern?
