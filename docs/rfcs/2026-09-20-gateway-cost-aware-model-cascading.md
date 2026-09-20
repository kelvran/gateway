# RFC: Cost-aware model cascading — design only, no code

- **Status**: Design-only, explicitly deferred pending its own named trigger. Grounded in `docs/upgrade-research/cost-aware-cascading-tier1-2026-09-20.md`, which independently re-verified the prior `[2026-09-14]` `not_yet` verdict against primary sources and found it still correct — for a narrower reason than originally stated. Mirrors the same "de-risk the how, not the when" precedent already established for `docs/rfcs/2026-09-11-gateway-redis-backed-cache-design.md` and `docs/rfcs/2026-09-11-gateway-mcp-outbound-credential-design.md`. This RFC's own recommendation is closer to "probably don't build this soon" than the other two design-only RFCs — see Drawbacks.
- **Date**: 2026-09-20
- **Author(s)**: Session agent (Claude Code), per the Tier-2 backlog synthesized from this round's 21-pass research sweep (11 general-purpose + 10 `/deep-research` workflow runs).

## Summary

Proposes a genuine per-request **cascade**: call a cheap/fast deployment first, and escalate to a pricier deployment for that *same* request only when a real, cheap-to-check signal on the cheap deployment's own response indicates it was inadequate (a refusal, a non-`stop` finish reason, a schema-validation failure for structured output). This is a different mechanism from what already exists in Kelvran today (`Deployment.CostTier` / `Router.activeCostTier`), which prefers the cheapest **healthy** tier across an entire model group but has no concept of a single request's own response quality. The research backing this RFC recommends **not building the cascade decision loop itself yet** — the field's strongest current benchmark shows even well-resourced trained routers can't reliably beat simply picking the best single model — but a narrow, already-shipped-in-this-round step (`FinishReason` reaching `GatewayDecisionEvent`) closes part of the signal gap for whenever this is revisited.

## Motivation

`DECISIONS.md`'s `[2026-09-14]` (fourth) entry recorded a `not_yet` verdict: "Kelvran has no request-level confidence/refusal/complexity signal today to drive a cascade decision at all," citing LLMRouterBench/ACL Findings 2026's finding that OpenRouter's own commercial auto-router underperforms simply picking the best model by -24.7%. This round's research (`docs/upgrade-research/cost-aware-cascading-tier1-2026-09-20.md`) re-verified that claim directly against the primary source (the ACL Anthology PDF itself, not a summary) and found:

1. **The -24.7% figure is real but stale as a description of OpenRouter's *current* router** (Finding 1) — it measured a Not-Diamond-powered mechanism OpenRouter itself replaced on 2026-08-10 with a market-spend-based design. Citing it in the present tense is no longer accurate, though the underlying number was correctly extracted.
2. **The paper's more load-bearing finding is unaffected by that correction** (Finding 2): across a field of 10 baseline routers (RouterDC, EmbedLLM, MODEL-SAT, GraphRouter, Avengers, HybridLLM, FrugalGPT, RouteLLM, Avengers-Pro, OpenRouter), "model-recall failure" is the dominant driver of the remaining gap to an Oracle router — Recall@3 of only 46.1%/50.6% for two tested baselines, and binary escalation routers specifically (HybridLLM, FrugalGPT — the closest published analogues to the cascade this RFC proposes) "struggle" to beat Best-Single at all. This is a real, current, field-wide bottleneck, not an artifact of Kelvran's own missing data.
3. **Kelvran's signal gap is real but smaller than originally stated** (Findings 3–4): `GatewayDecisionEvent` itself still carries no quality/confidence signal (`Outcome`'s 9 non-OK values are all pre-call rejection reasons, never a judgment on a completed response). But two already-computed per-response primitives exist in `adapter.types.go` that no prior research pass named — `Message.Refusal` (populated by 2 of 5 adapters) and `Choice.FinishReason` (normalized across all 5 adapters, previously used only to gate cache writes against truncated responses). Neither was wired to any decision.
4. **The one `build_now`-sized item from that research — threading `FinishReason` onto `GatewayDecisionEvent` as a new additive field — has already shipped this same round**, as Tier-1 item 1 (`4bba3e96`, `dataplane.primaryFinishReason`). This RFC's own Detailed Design below builds on top of that already-shipped field rather than re-proposing it.
5. **No cascade/escalation infrastructure exists anywhere in the codebase today**, confirmed by grep against `internal/router` and `internal/gateway/dataplane`: the only two routing/retry mechanisms that exist are `Router.activeCostTier` (a health-based cost-tier *preference*, described below) and `attemptFallbackChain` (a hard-failure-only deterministic fallback — it only re-picks on a real upstream *error*, never on a successful-but-low-quality response).

### Why this is not the same thing as the existing `CostTier` mechanism

It is easy to assume Kelvran already does cost-aware cascading because `Deployment.CostTier` / `Router.activeCostTier` (`internal/router/health.go`) exist. They do not solve this problem. `activeCostTier` computes, once per `Select` call, the lowest cost tier that has *at least one currently healthy deployment* in a model group, then `selectHealthy` prefers a deployment matching that tier. This is a **group-level, health-based preference** — it has no visibility into any individual request's own response, and it never calls more than one deployment for a given request unless that deployment's call itself fails (handled by the separate `attemptFallbackChain`). A cascade, as this RFC uses the term, is a **per-request, response-quality-based** decision: call the cheap tier, look at what it actually returned, and only then decide whether a second, real call to a pricier tier is warranted. The two mechanisms are complementary, not overlapping — a future cascade would still respect `CostTier`/health preferences when picking *which* cheap and *which* escalation deployment to use, it just adds a genuinely new decision axis on top.

## Detailed Design

This section is written so a future implementer could start from it directly, once the trigger below fires — not because building it now is recommended.

### Config surface: a `cascade_target` field, mirroring `Sticky`'s existing precedent

Add `CascadeTarget string` to `controlplane.DeploymentConfig` and `router.Deployment` (parsed exactly like `Sticky bool` and `CostTier int` already are — a per-deployment, operator-opt-in field, empty/unset meaning "no cascading configured," zero behavior change for every deployment that doesn't set it). `CascadeTarget` names another deployment (by `Name`) within the same `Model` group — the deployment to escalate to if the cascade signal below fires. Validated at config-load time exactly like `fallback_chains` targets already are (must reference a real, configured deployment name; a self-reference is rejected).

### Escalation signal: two already-computed, cheap-to-check primitives — deliberately not a trained classifier

Per the research's Finding 2, a trained classifier or embedding router is explicitly **not** what this design proposes — the field's own best benchmark shows that class of mechanism struggling to beat Best-Single even with far more resources than Kelvran has. Instead, a new pure function:

```go
// escalationSignal reports whether resp, from the CHEAP deployment, warrants
// escalating this same request to its configured CascadeTarget. Deliberately
// cheap: no network call, no LLM-as-judge, no trained model — only the two
// real, already-computed per-response primitives this round's research named
// (adapter.types.go's Message.Refusal and Choice.FinishReason).
func escalationSignal(resp adapter.ChatResponse) (escalate bool, reason string) {
    if len(resp.Choices) == 0 {
        return false, ""
    }
    c := resp.Choices[0]
    if c.Message.Refusal != "" {
        return true, "refusal"
    }
    switch c.FinishReason {
    case "content_filter":
        return true, "content_filter"
    // "length" (truncation) is deliberately NOT auto-escalated here --
    // responseWasTruncated already gates this response out of the cache;
    // whether a truncated-but-otherwise-fine answer should also escalate
    // to a cascade target is a real, separate, deliberately unresolved
    // question (see Unresolved Questions).
    }
    return false, ""
}
```

This is honest about its own limits: `Message.Refusal` is populated by only 2 of 5 adapters (OpenAI, openaicompat) today — Anthropic/Gemini/Bedrock signal the analogous condition through `FinishReason` instead, so the escalation rate would be systematically uneven across providers until (if ever) that adapter gap is separately closed. This is disclosed here, not silently worked around.

### Call-site integration: a new decision point in `runMissPath`, after a successful cheap-tier response

`dataplane.Pipeline.runMissPath` would gain a new step, positioned **after** a successful call to the initially-selected deployment and **before** returning that response to the client:

1. If the deployment that was just called has a non-empty `CascadeTarget` (looked up via `p.deploymentsByName`), call `escalationSignal(resp)`.
2. If it returns `escalate=false`, return the cheap-tier response exactly as today — zero added latency or cost for the common case.
3. If `escalate=true`, call the `CascadeTarget` deployment via the **same** `call`/`rateLimitOK`/`deploymentCapacityOK`/`capabilityOK`/`regionOK` closures `attemptFallbackChain` already threads through (reusing that plumbing, not duplicating it) — but this is a **cost-additive** second real call, never a replacement: the cheap-tier call already happened and is already billable. Both calls' costs must be attributed; `GatewayDecisionEvent` would need a new field (see below) so this is visible in cost-report/observability, not silently doubled with no explanation.
4. Return the escalation target's response to the client (or, if the escalation call itself fails, fall back to the original cheap-tier response — never a hard failure caused by the cascade attempt itself, matching this codebase's existing fail-open convention for every other optional enhancement).

### Observability: a new additive `GatewayDecisionEvent` field

`cascaded: bool` plus `cascade_reason: string` (empty when `cascaded` is false), mirroring `cost_is_estimated`'s own precedent for a disclosure-only additive field. **Requires the same `AGENTS.md` "ask first" confirmation this round's item 1 (`finish_reason`) already went through before touching `api/gatewayevents.proto`** — flagged here explicitly rather than assumed, per that rule.

### Explicitly out of scope for this design

- Any trained classifier, embedding-similarity router, or per-request LLM-as-judge call on the hot path (Finding 2's model-recall-failure bottleneck applies to all of these; a judge call would also add real latency/cost to every cascaded request, on top of the escalation call itself).
- Session-start task classification (Databricks Unity Gateway "Smart Routing" — Finding 6) — a different problem (session-level model *tier* selection to preserve prompt-cache locality across turns), not per-request quality-based escalation.
- Any online eval-feedback loop from `evals` back into a live routing decision — Kelvran's `evals`-to-gateway link is a one-way, offline, human-triggered report (`cost-report --agent-run-id`) today; building a live feedback loop is a separate, larger effort this design does not attempt.

## Drawbacks

- **May not pay off even if built well.** Finding 2's field-wide result — binary escalation routers (HybridLLM, FrugalGPT, the closest published analogues to this exact design) "struggle" to beat Best-Single — means there's a real, currently-unsolved risk that a correctly-implemented version of this design still doesn't win economically. This is not a straw-man objection to dismiss; it is the single strongest piece of evidence against building this now.
- **Latency and cost, honestly stated.** Every escalated request pays for two full model calls sequentially — the cascade only wins when escalation is genuinely rare, the cost delta between tiers is large, and the signal is precise enough to avoid escalating on merely-different-but-still-fine answers. A noisy signal (and `Message.Refusal`'s uneven 2-of-5-adapter coverage is a real source of noise) directly erodes whatever savings the cheap tier would otherwise provide.
- **No way to measure whether it's working, once built.** Kelvran has no live judge/eval signal wired to any routing decision (Finding 6) — without one, there's no way to tell, post-hoc, whether escalation decisions were actually correct (i.e., whether the escalated answer was in fact better), only whether the mechanism fired.
- **Uneven signal coverage across providers**, disclosed above — a real, non-cosmetic caveat, not a minor implementation detail.
- **New `api/` field** (`cascaded`/`cascade_reason`) is a small but real cross-language-contract change requiring the ask-first gate every time this class of change is made.

## Alternatives Considered

- **A trained classifier or embedding router (RouteLLM/Not-Diamond/Martian/Unify-style)** — rejected per Finding 5: every commercial option surveyed requires either a labeled training set Kelvran does not have, or a vendor-run offline profiling process Kelvran cannot compute itself. Even setting the data gap aside, Finding 2 means this class of router may not be worth building at all.
- **`semantic-router`-style hand-curated exemplar routing** — the cheapest bootstrap path found (Finding 5, no production traffic needed, just a small hand-authored exemplar list), but it is designed and evaluated for intent/tool routing, not quality-cost model selection; no evidence was found of this pattern being used for this specific problem at production scale. Named as the cheapest option to revisit, not adopted here.
- **Databricks Unity Gateway's session-start "Smart Routing" pattern** (Finding 6) — a real, live, beta-production precedent, but solves a different problem (preserving prompt-cache locality across a session's turns by picking a tier once at session start) and depends on an online eval-feedback loop Kelvran does not have. Worth revisiting once/if that loop exists, not adopted now.
- **Do nothing beyond what already exists** (`CostTier` health-based preference + `attemptFallbackChain`'s hard-failure-only fallback) — this is, in effect, the recommended path until the trigger below fires. Explicitly included per the RFC template's own instruction to consider it.

## Unresolved Questions

Carried directly from the research report's own Open Questions, since none were resolved by writing this design:

- Now that `FinishReason` reaches `GatewayDecisionEvent`, would `evals`' judge-panel pipeline show enough correlation between non-`stop` finish reasons and actual judged quality to make `escalationSignal` a useful trigger at all — or is it too coarse (a `stop` finish reason still frequently accompanying a low-quality-but-complete answer)?
- What would it take to get a real requests/day (or per-deployment request-count) figure out of the live Bedrock pilot, and does it already clear the traffic-volume floor the 2026-09-09/2026-09-13 research named as the actual trigger for wiring any traffic-derived signal — or is that a separate, still-open gap?
- Should a truncated (`finish_reason == "length"`) response also be a candidate for escalation, or does that conflate two different problems (truncation vs. genuine inadequacy)? Left unresolved in the Detailed Design above rather than guessed at.
- Is closing the `Message.Refusal` 2-of-5-adapter coverage gap (getting Anthropic/Gemini/Bedrock to signal an equivalent condition) worth doing on its own, independent of whether this whole cascade is ever built — since it would also make `escalationSignal`'s signal quality more even across providers?

**Trigger to revisit**: per the research's own recommendation table — do not treat "the pilot account is live" as evidence the traffic-volume floor has been met; get an actual requests/day or per-deployment sample-count figure from the live pilot first. Separately, re-verify Finding 2 (model-recall failure) against any *newer* routing benchmark before starting implementation, since this design's core justification for staying deferred rests on that specific, dated finding.
