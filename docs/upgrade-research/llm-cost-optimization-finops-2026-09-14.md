# LLM Cost Optimization / FinOps Beyond Existing Cost-Attribution — Research (2026-09-14)

> **Recovery note:** the synthesizing subagent's own file-write step failed silently (a known recurring class); recovered by reading the workflow's own returned JSON result and reconstructing this report by hand.

**Scope:** FinOps patterns beyond Kelvran's existing cost-attribution work — model cascading, prompt compression, batch-API arbitrage, and spend anomaly detection.

**Already shipped (not re-litigated):** per-request cost/savings accounting with `agent_run_id`-level attribution; opt-in per-deployment cost-tier routing; real cache hit-rate/savings metrics; cross-provider prompt-cache `CacheControl` auto-population; the semantic-cache (embedding L3.5) question, settled `not_yet`.

## Findings

1. **Model cascading is a real, well-defined pattern, but 2026 benchmarks show it captures only a fraction of the theoretical ceiling — and Kelvran has no telemetry to plug into one yet.** Large-scale evaluation (LLMRouterBench, ACL Findings 2026, 400K+ instances/33 models/21 datasets) found OpenRouter's own commercial auto-router performed **worse** than simply always picking the single best model (-24.7%), driven by "model-recall failures" — routers failing to identify the one model that could actually answer correctly. Cascading ("try cheap, verify, escalate") trades an explicit latency tax for not needing an accurate upfront classifier. **`build_now`** as a documented pattern definition; **`not_yet`** as a validated production win — Kelvran has no request-level confidence/refusal/complexity signal to drive one.

2. **Every vendor headline cascading/routing claim checked turned out to be unverifiable marketing, not an independent benchmark.** Martian's "up to 98% cost reduction, 300+ companies," Not Diamond's "39% average accuracy improvement" (traced to a single customer testimonial republished as a vendor stat), and OpenRouter's Auto Router rankings (based on aggregate 7-day platform *spend* — "wisdom of the market" — not evaluated routing accuracy) are all self-reported or single-anecdote. **Do not cite any of these numbers as validated.**

3. **Prompt compression (LLMLingua/LLMLingua-2) is structurally disqualified for a transparent gateway, not just immature.** It requires hosting a separate ML-serving microservice (Kong ships it as its own private Docker image with HTTP/JSON-RPC endpoints — not an in-process transform); LLMLingua v1 misses its own target compression ratio by >0.15 error above 8,000 tokens; LLMLingua-2 causes severe, task-dependent accuracy collapse (passage-counting accuracy falls below 4.5%; few-shot classification drops up to 52%) because compression destroys structural cues a proxy cannot detect or exclude in advance. This directly violates Kelvran's own hard transparency requirement. **Disqualified, not `not_yet`.**

4. **Even prompt compression's latency benefit (as opposed to token-cost benefit) doesn't transfer to Kelvran's real deployment shape.** The stated latency win is confined to unoptimized inference frameworks, prompts ≥2,000 tokens, and high-end GPUs. Under optimized serving (vLLM) or commercial provider APIs — the closest analog to what Kelvran actually proxies to — compression showed no reliable speedup and sometimes made things worse, with speedups dropping below 0.5x for long prompts via commercial APIs. *(Medium confidence — 2-1 vote.)*

5. **Batch-API cost arbitrage (a narrower, opportunistic version short of full batch-proxying) is a genuine open research gap, not a considered verdict.** Zero claims on this topic survived adversarial verification in this pass — not even among refuted claims. Whether a background job re-routing low-priority/retryable traffic to a batch endpoint has real precedent, or structurally requires full batch-proxying first, remains unanswered. *(Low confidence — insufficient evidence either way.)*

6. **CUSUM (cumulative sum) sequential change-point detection is a real, lightweight, decades-old extension of the same statistical family Kelvran's evals toolkit already uses (Wilson interval / mixture-SPRT) — directly applicable to gateway-side spend-velocity monitoring.** It tracks small deviations from a reference mean and only alarms once cumulative evidence crosses a decision threshold `h`, rather than flagging single-point outliers. The false-alarm-vs-detection-latency tradeoff is concrete and tunable: at `h=4σ`, expected false-alarm interval (ARL0) is ~170 points with a 1σ shift caught in ~6 points; at `h=8σ`, ARL0 rises to 4,000+ points at the cost of ~12 points of detection delay. **`build_now`** — but the specific threshold needs a deliberate choice, not a default.

7. **Production FinOps practice validates a "heavyweight forecast, lightweight decision gate" design, not "heavyweight decision-making."** Azure Cost Management's built-in anomaly detection uses a deep-learning forecaster (WaveNet, trained on 60 days of history) to predict expected spend — but the actual anomaly *decision* is still gated by a simple statistical rule: flag only if actual usage falls outside a confidence interval around the forecast. This supports pairing a cheap statistical forecast (not a neural model) with an interval-based decision gate at Kelvran, matching Finding 6's own CUSUM approach.

## Open Questions

- What would a validated cost/quality number for a "try cheap, escalate" cascade look like on Kelvran's own real traffic? The two concrete academic case-study numbers found (MixLLM 97.25%/24.18%, R2-Reasoner 84.46% savings) were both refuted under adversarial verification as insufficiently tied to a production gateway.
- Does a narrower batch-API arbitrage design have any real precedent, or does it structurally require full batch-proxying first? No evidence either way.
- Would Kelvran's own request/response telemetry show enough separability between "needs the expensive model" and "cheap model would work" to close the model-recall-failure gap even well-resourced commercial routers can't close?
- Is there an opt-in, application-visible (header-flagged) version of prompt compression that would satisfy the transparency requirement — or does the task-dependent accuracy-collapse risk disqualify it even as an explicit opt-in?

## Caveats

Two findings (prompt-compression latency scope; CUSUM's specific ARL/threshold numbers) rest on 2-1 split votes, not unanimous verification — the underlying sources still support them, but a dissenting verifier flagged residual generalization risk. Several Q1 sources are vendor/independent blogs rather than peer-reviewed papers; mitigated by cross-checking each against its underlying primary source directly, but source-tier quality is lower than the Q2/Q4 academic/first-party sources. The academic sources are all within ~5 months of this research date, in a fast-moving field. Several claims that looked plausible on first read (MixLLM/R2-Reasoner cascade numbers, "most routers are indistinguishable," a static-signal "routing plateau") were explicitly refuted under adversarial verification and excluded above.

## Synthesis: build_now vs not_yet

| Item | Verdict | Why |
|---|---|---|
| Model cascading (pattern definition/design doc) | **build_now** | Real, well-defined pattern |
| Model cascading (actual production deployment) | **not_yet** | No request-level confidence/complexity signal exists yet; benchmarks show even commercial routers underperform |
| Prompt compression (LLMLingua-style) | **disqualified** | Structural transparency violation + task-dependent accuracy collapse a proxy can't detect |
| Batch-API cost arbitrage (narrower version) | **unresolved** | Zero surviving evidence either way — genuine research gap |
| CUSUM spend-velocity anomaly detection | **build_now** | Lightweight, same statistical family as existing evals toolkit, concrete tunable tradeoff |
