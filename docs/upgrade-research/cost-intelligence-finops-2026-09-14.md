# Cost Intelligence / FinOps — Upgrade Research (2026-09-14)

Recovered by hand from the deep-research workflow's own returned JSON — its file-write step failed
silently, a known recurring class per `AGENTS.md`'s Gotchas section. Reconstructed from the
workflow's `summary`/`findings` fields, not re-run.

**Correction found post-hoc (see this round's own implementation plan, Phase 6):** a SEPARATE,
earlier same-day research pass, `docs/upgrade-research/llm-cost-optimization-finops-2026-09-14.md`
(15:59, before this report), already found and shipped a real CUSUM spend-velocity anomaly detector
(`gateway/internal/telemetry/spendvelocity/`, commit `3a7ab5c9`) — contradicting Finding 2 below's
"no verified pattern exists" conclusion for statistical anomaly detection specifically. The detector
exists and is tested but has zero callers outside its own package — not yet wired into the live
request path. Budget forecasting (Finding 1) is unaffected by this correction; it addresses a
different question (burn-rate-to-cap-date projection, not spend-velocity change-point detection).

## Scope

Compared against 2026 production practice in FinOps-for-LLM tooling (CloudZero, Vantage, native
OpenAI/Anthropic usage dashboards, Helicone's cost analytics, Portkey's budget alerts).

## Findings

### 1. Budget forecasting — `not_yet`

No verified, low-effort "burn-rate → days-to-cap" forecasting pattern exists in 2026 FinOps-for-LLM
practice. The one primary source documenting a concrete alerting mechanism (finopsllm.com) uses
purely static percent-of-budget thresholds (50/75/90/100%) with zero forecasting/statistical
machinery — a full-text keyword sweep of the raw article found zero forecast/predict/statistical/
moving-average terms anywhere. The FinOps Foundation's own FinOps-for-AI guidance explicitly states
AI cost predictability is "generally lower, especially for the Crawl and Walk phases" and
recommends more frequent budget *revision* over a point forecast — the opposite of a "compute once,
get an ETA" pattern. Five distinct blog-sourced candidate forecasting formulas (linear
month-to-date projection, percentile-based P50/P90/P99 budgeting, a "Burn Map" 30-day projection,
and others) were checked against their cited sources and all refuted (0-3 or 1-2 votes) — these
specific "days to cap" formulas circulating in this space did not survive verification.

**Verdict: not_yet.** Named trigger: either (a) enough production traffic history to empirically
validate a trend model, per the Foundation's own Crawl→Walk→Run maturity framing, or (b) a specific
tenant request for an ETA-style metric. The verified low-effort win available today — percent-of-
budget threshold alerting off existing `budget.Tracker` spend data, no new plumbing — is this
round's Phase 1 (the alert ladder).

### 2. Spend-anomaly/spike detection — `not_yet` at the time of this report; superseded

No verified low-effort *statistical* anomaly-detection pattern was found by this report to copy —
the FinOps Foundation's guidance recommends pairing hard usage limits with anomaly-detection tooling
and tracks "Anomaly Detection Rate" as a suggested KPI (guidance-level, not a specified algorithm);
the one primary source with a concrete alerting mechanism (finopsllm.com) is purely threshold-based
with no anomaly/moving-average/stddev/spike logic anywhere in its text.

**Superseded, see correction above**: a real CUSUM change-point detector was independently found
and built the same day by a parallel research effort. This report's own "not_yet" verdict for
statistical anomaly detection specifically no longer holds — the algorithm exists; only wiring it
into the live request path remains.

### 3. Model cost/quality tradeoff recommendation surface — `not_yet`

Zero verified external precedent exists for a "shift Y% of traffic to a cheaper model at Z
similarity" recommendation surface. Every blog claim touching model-routing savings numbers checked
during this research was refuted. This is squarely `not_yet` — no evidence anyone has built this in
production, and Kelvran's own evals judge/panel infrastructure, while real, has never been pointed
at this specific question.

### 4. Chargeback/showback and cost allocation dimensions

The two standard external taxonomies checked (FOCUS v1.4, the FinOps Framework's Allocation
capability) both stop at account/project/tag-level dimensions and have no agent-run or AI-workload
dimension at all — meaning Kelvran's already-shipped `cost_usd`/`savings_usd` on
`GatewayDecisionEvent` at agent-run-id + virtual-key-id granularity already *exceeds* what these
standards define. Portkey's fully-generalized "group by any custom metadata key" API is a real (if
optional) precedent for going further than Kelvran's two fixed dimensions, should a real need arise.

**Verdict: no action needed** — Kelvran's existing granularity already meets or exceeds the
external standard.

## Meta-finding

Roughly a dozen plausible-sounding blog claims proposing concrete forecasting formulas,
anomaly-detection heuristics, and routing-savings percentages were checked against primary sources
and refuted. This vertical's blog content is an unreliable source for the specific numbers these
research questions needed — primary sources (FinOps Foundation, vendor docs, this codebase's own
sibling research) are what survived verification.

## Sources

- https://www.finops.org/wg/finops-for-ai/
- https://finopsllm.com/research/cap-inference-costs
- `docs/upgrade-research/llm-cost-optimization-finops-2026-09-14.md` (sibling report, same day)
- `gateway/internal/telemetry/spendvelocity/` (CUSUM detector, commit `3a7ab5c9`)
