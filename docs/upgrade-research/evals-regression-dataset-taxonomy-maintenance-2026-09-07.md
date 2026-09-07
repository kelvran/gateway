# Regression-Tier Eval Dataset — Taxonomy & Maintenance Workflow (2026-09-07)

## Question

What taxonomy/categorization structure should a regression-tier eval dataset use, and what real, documented maintenance workflows keep such a dataset from silently going stale (drift, outdated "expected" answers, category imbalance) over time? This is 1 of 4 parallel deep-research passes on Kelvran's "build a real regression-tier eval dataset" backlog item — scoped to taxonomy/categorization and staleness-maintenance workflow only. Kelvran already has the categorization mechanism (`EvalCase.tier`/`tags`/`flaky`, the `category:`-tag convention used by the just-shipped `--category-fail-under` gate) — this research is about what to *populate* it with, not new code.

## Summary

Real eval platforms categorize regression suites along **multiple orthogonal axes at once**, not one flat list — HELM crosses a fixed 7-metric set against a core-vs-targeted scenario split; DeepEval mixes granularity (trajectory vs. component-level), system-component (retriever vs. generator), and explicit failure-mode names. Promptfoo's per-case `metadata` + `filterMetadata` (arbitrary key=value, AND-combined filtering) is the closest real documented analog to Kelvran's `category:`-tag convention. On staleness: practitioners document concrete *reactive* triggers (incidents, complaint spikes, metric drift, major system changes) rather than a fixed calendar, and vendor guidance explicitly names "frozen golden set, never refreshed" as a real anti-pattern. On category imbalance: the strongest finding is qualitative — automated flagging only generates *candidates*; human/SME judgment at review time is what actually keeps a set balanced, not an automatic mechanism. On versioning: **no real platform was found with a structural analog to Kelvran's append-only, promote-creates-a-new-case-never-edits mechanic** — Braintrust auto-versions per write but does per-record *updates* as merge-upserts against a stable ID (the opposite pattern); LangChain's golden-dataset workflow is append-only for new entries but silent on correcting a stale existing one. **Kelvran's "how do we correct a stale expected answer without editing history" question has no documented industry precedent — this is genuinely open design territory, not a pattern to adopt from elsewhere.**

## Findings

### Finding 1 — Real taxonomies cross multiple orthogonal axes (confidence: high, 3-0 on all 4 sub-claims)

HELM crosses a fixed 7-metric set (accuracy, calibration, robustness, fairness, bias, toxicity, efficiency) against a core (16, run on all models) vs. targeted (26, probing specific capabilities/risks) scenario split — 42 total in the original paper snapshot (the live project has since split into multiple parallel leaderboards). DeepEval separately splits agent metrics by granularity (trajectory vs. component-level tool-calling), RAG metrics by system component (retriever vs. generator), and Safety metrics by explicit failure-mode name (bias, toxicity, non-advice, misuse, PII leakage, role violation).

### Finding 2 — Promptfoo's `metadata`/`filterMetadata` is the closest real analog to Kelvran's tag convention (confidence: high)

Arbitrary key=value pairs per test case, AND-combined CLI filtering — distinct from Promptfoo's separate top-level `tags` field (documented for whole-suite dimensions like environment/application, not task-type/failure-mode). This gives Kelvran's existing `tags` mechanism a concrete external precedent as a flexible, filterable per-case bag.

### Finding 3 — One documented (single-source) starter taxonomy by provenance and purpose (confidence: medium)

Four buckets — production sample, adversarial, edge cases, failure replays — with production samples stratified by intent/persona/retrieval-shape, and adversarial cases sub-tagged by attack type (jailbreak, injection, malformed, poisoned_retrieval). Single vendor blog, not an industry-standard framework — a credible practitioner proposal, not documented consensus.

### Finding 4 — Staleness is handled by reactive triggers, not a fixed calendar (confidence: medium-high)

Practitioners (Hamel Husain/Shreya Shankar) document concrete reactive triggers for re-running error analysis: incidents, user-complaint spikes, detected metric drift, and any major system change (new features, prompt updates, model switches, major bug fixes) — high confidence (3-0). Separately, vendor guidance names "frozen golden set built at launch, never refreshed" as a real anti-pattern that eventually scores a system that no longer exists (2-0, lower confidence, single source). **A more specific tiered monthly/quarterly/annual cadence claim was explicitly refuted (0-3) — don't cite a fixed calendar as documented practice.**

### Finding 5 — Braintrust: ongoing maintenance loop, not one-time build (confidence: medium)

Re-run experiments against the dataset after changes, continuously add newly discovered failures, periodically prune duplicate/stale cases (optionally converting recurring failure patterns into automated scorers). A more specific "dedicated multi-reviewer calibration queue" claim from the same vendor was refuted (1-2) — don't cite that specific mechanism.

### Finding 6 — LangChain: append-only + human-gated, the strongest real precedent for Kelvran's promotion gate (confidence: high, 3-0)

An online evaluator flags high-quality candidate production traces; these route to a human annotation queue where subject-matter experts decide whether each trace should be added to the golden set. Critically: **automated evaluator-score filtering only generates candidates — human judgment afterward is what keeps the resulting eval set balanced and representative**, not an automatic mechanism. This is the strongest, most directly relevant precedent found for gating Kelvran's own promotion mechanic: by human review of evaluator-flagged candidates, not automatic promotion.

### Finding 7 — A stratify + oversample + reweight recipe for category imbalance (confidence: medium, 2-1)

Stratify by the dimensions that matter (intent, persona, language, latency bucket), oversample the strata where the judge/signal is most informative (refusals, errors, tail intents), then reweight back to the population distribution when reporting an aggregate metric. Single-maintainer source but citation-grounded (footnotes cite Hamel Husain, Chip Huyen, Shreya Shankar, Arize Phoenix, Langfuse) and maps onto established survey-sampling theory (post-stratification/Neyman allocation). **A more specific 3-5x failure-oversampling ratio was explicitly refuted (0-3) — only the qualitative pattern survived, no defensible number.**

### Finding 8 — No real platform matches Kelvran's append-only, never-edit-in-place versioning (confidence: high — this is a confirmed *absence*)

Braintrust auto-creates a new dataset version on every row write (via a shared transaction ID), with soft-deleted rows still retrievable from a historical version — but per-record *updates* (correcting an `expected` field) are merge-upserts against a stable row ID, the opposite of "new ID per change." A competing claim that LangSmith matches Kelvran's exact pattern was explicitly refuted (0-3). **Kelvran's exact mechanic — `evals promote` always creates a new case, never mutates the source — has no confirmed real-world precedent.**

## Kelvran-Specific Recommendation

**Starter taxonomy** (populate the existing `tags` field with `category:` values, following the naming convention the CI-gate work already established with `category:safety`), grounded in Kelvran's own real product surface:
- `category:routing` — deployment selection, fallback-chain correctness
- `category:cache` — L1/L2/L3 hit/miss correctness, tenant isolation
- `category:cost` — Decimal cost-accounting correctness, budget-threshold behavior
- `category:guardrail` — PII/secrets/prompt-injection detection accuracy
- `category:streaming` — SSE/streaming correctness
- `category:safety` — already in use by the CI-gate example
- `category:judge` — judge-scoring accuracy itself (a meta-eval category, scoring the evaluator, not the gateway)

Cross this with a second, orthogonal `provenance:` tag axis (per the methodology report's Finding 1: `provenance:dogfood` / `provenance:hand-written` / `provenance:synthetic`) — matching Finding 1's "real taxonomies cross multiple axes" pattern, not a single flat list.

**Maintenance workflow, sized for a small pre-1.0 team, not enterprise process:**
1. **Reactive triggers, not a calendar** (Finding 4): re-review affected regression cases whenever a scorer/prompt/model change ships, a real bug is found, or CI gate behavior looks surprising — not on a fixed monthly/quarterly schedule (that specific cadence framing was refuted, don't adopt it).
2. **Human review gate before `evals promote --tier regression`** (Finding 6, the strongest precedent found): a run's output becomes a candidate, a human confirms it's genuinely correct-enough before promotion — matching the methodology report's own recommendation independently.
3. **Category balance: track qualitatively via the already-built per-category report output, not a numeric target** — no verified numeric imbalance-correction target exists anywhere (every attempt to verify one was refuted); periodically eyeball the per-`category:` breakdown `report_cmd` already prints and notice gaps, rather than building an automated rebalancing mechanism this research found no real precedent for.
4. **The stale-expected-answer problem — genuinely unprecedented, so this is original design, not adopted practice**: since no real platform documents how to correct a stale case in an append-only system, the reasoned answer for Kelvran is: tag the stale case `deprecated` (never delete or edit it — preserving history, matching the append-only mechanic's own spirit), promote a new corrected case via the normal `evals promote` path, and record the superseding relationship in the new case's own metadata (e.g. `supersedes: <old-case-id>`). This is a **new design decision this report is making explicitly**, not a pattern borrowed from research — flagged as such rather than presented as external best practice.

## Caveats

- Sourcing skews toward single-author/vendor blogs (Braintrust, LangChain, futureagi.com, aievals.co) for maintenance/staleness/imbalance findings — verified directly against primary text, but treat specific numbers as one practitioner's synthesis, not industry consensus.
- Several numeric prescriptions were explicitly refuted and must not be cited as documented practice: a tiered monthly/quarterly/annual refresh cadence, a 3-5x failure-oversampling ratio, a 40-60%-per-class imbalance target, a frequency-weighted-aggregate-hides-regressions mechanic.
- HELM's 16/26/42 counts are the original Nov 2022 paper snapshot — the live project has since split into several parallel leaderboards with different scenario counts.
- The claimed LangSmith-matches-Kelvran's-versioning analog was explicitly refuted — no real platform was found with a direct structural match.
- Web-search tooling hit rate limits during several verification passes, so single-source blog claims rest on direct primary-fetch verification rather than independent cross-search.

## Open Questions (carried forward)

- Is there any real, documented eval-maintenance workflow sized specifically for a small, fast-moving pre-1.0 team, as opposed to enterprise-scale processes (Braintrust/LangChain/HELM)? No source addressed this directly — the recommendation above is an inference, not a documented precedent.
- Is there any real precedent anywhere for the specific "expected answer went stale in an append-only system" problem? None found — Kelvran's `deprecated`/`supersedes` proposal above is original design, worth revisiting if a real precedent surfaces later.
- Do Inspect AI or DeepEval have a dedicated category-imbalance remediation workflow, as opposed to the general stratified-sampling advice found on a single blog? Not confirmed — Inspect AI wasn't reachable/verified in the surviving claim set for this specific question.
- Is there real, verified evidence for a specific numeric imbalance-correction target used by any named platform? Every attempt to verify one was explicitly refuted.
