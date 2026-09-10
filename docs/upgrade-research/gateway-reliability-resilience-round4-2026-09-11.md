# Gateway Reliability/Resilience-Engineering Research — Round 4 (2026-09-11)

Scope: `gateway/`'s resilience-engineering lens — passive/active health signal, load
shedding, retry/backoff, bulkheading, chaos-engineering readiness. Grounded against
`gateway/ARCHITECTURE.md`, `PRD.md`, and `docs/upgrade-research/gateway-next-upgrade-round3-2026-09-11.md`;
externally verified against Envoy, Istio, Kong (incl. Kong's LLM-gateway-specific
ai-proxy-advanced), LiteLLM, gRPC's retry design, Google's SRE book, Azure's Bulkhead
pattern, and the chaos-engineering community manifesto, each claim adversarially
3-vote-verified before inclusion here.

## Executive Summary

Kelvran's gateway already implements a materially larger slice of 2026 resilience
best-practice than a first glance suggests: active synthetic health-probing (N-of-M
consecutive thresholds, ramp-gated recovery) mirrors Envoy/Istio's active-check design, and
its 2026-09-07 retry-storm-mitigation RFC already ships equal-jitter inter-hop backoff
(25ms/400ms, nearly identical shape to Envoy's 25ms/250ms default), a per-key concurrency
cap, a per-key client-facing Retry-After backoff streak, and a per-request fallback-chain
circuit breaker. The genuine, well-precedented gaps are: (1) passive/real-traffic health
signal — real across Envoy, Istio, Kong (incl. Kong's own LLM-gateway-specific
ai-proxy-advanced circuit breaker), and Kong Ingress Controller, all of which are strictly
asymmetric (passive real-traffic signals only ever narrow/eject; only active/synthetic
probes widen/re-admit) — a pattern Kelvran's own prior research already scoped correctly but
left unwired pending a real traffic-volume floor; (2) a fleet-wide retry budget (Envoy
`retry_budget` + a dedicated suppressed-retry counter, gRPC's token-bucket retry throttle,
SRE's 10%-ratio cap) that Kelvran's per-request/per-key mechanisms don't yet provide, though
this too is arguably meaningless without real traffic volume; (3) priority/criticality-tiered
load shedding (Google SRE's four-tier model, echoed by a real but Enterprise-paywalled/
beta-labeled LiteLLM implementation) which is real precedent but blocked on the same "no
multi-tenant concept yet" trigger that already deferred hierarchical budgets twice; and (4) a
live, hypothesis-driven fault-injection harness — Kelvran's `routing_chaos` corpus is real
postmortem-grounded scenario-mining but is a static regression fixture, not the four-step
live-experiment methodology chaos engineering's own canon defines, and "run in production" is
an advanced/aspirational principle, not a baseline requirement, so a staging-level
fault-injection harness is a genuine build-now candidate independent of production traffic.
Bulkheading beyond per-deployment concurrency ceilings was not covered by any surviving
verified claim this round and remains a real open question, not a confirmed gap.

For each: the primitives (filters, gates, harnesses) are frequently "build now" as
inert/unwired code; the actual activation/wiring of nearly every one of these mechanisms
recurs on the exact same trigger Kelvran has already named elsewhere — a real
production-traffic-volume floor it does not yet have.

## Findings

### Finding 1 — Passive/real-traffic health signal: universally asymmetric, correctly unwired
**Confidence: high**

Passive (real-traffic-driven) health/outlier detection is a mainstream, cross-vendor 2026
pattern — including in an LLM-gateway-specific product (Kong `ai-proxy-advanced`) — but it is
universally asymmetric: passive signals only ever narrow (eject a target), never widen
(re-admit); re-admission is exclusively the job of active/synthetic probing.

**Verdict: NOT YET** for wiring real traffic into Kelvran's `ReportProbeResult` (named
trigger: a real production-traffic-volume floor, identical to the trigger already named in
`docs/upgrade-research/gateway-router-health-real-traffic-2026-09-09.md`). **BUILD NOW** for
the underlying status-class filter + minimum-sample-size gate as real, tested, unwired
primitives.

Evidence: Envoy's outlier detection is explicitly passive, designed to run alongside (not
replace) active health checking, with a disableable flag governing whether active-check
success can un-eject a target. Istio's `consecutive5xxErrors` ejects from real traffic on the
same Envoy mechanism. Kong's `ai-proxy-advanced` (an LLM-gateway product, not a generic
proxy) ships a real `max_fails`/`fail_timeout` circuit breaker driven by actual request
failures, plus an EWMA latency-based load balancer that deliberately keeps 0.1%-5% of traffic
on slower targets to keep their scores current. Kong Gateway and Kong Ingress Controller:
passive checks can ONLY disable, never re-enable, a target. Kelvran's own
`gateway-router-health-real-traffic-2026-09-09.md` research independently reached the same
architecture as its recommended design — a real-traffic 5xx/local-origin failure should only
ever trigger an out-of-band ACTIVE probe, never call `ReportProbeResult` directly — matching
the industry's narrow-only/widen-via-active-probe pattern, but explicitly declined to wire
it, citing the same missing traffic-volume floor Envoy/Linkerd/Hystrix/resilience4j/Polly all
gate their own passive mechanisms behind.

### Finding 2 — Retry/backoff already matches best-practice shape; fleet-wide retry budget is the real gap
**Confidence: high**

Kelvran's retry/backoff behavior already matches 2026 best-practice shape (jittered
exponential backoff nearly identical to Envoy's default) but lacks a fleet-wide retry
BUDGET (a circuit breaker on retries themselves, independent of any single request or key)
that Envoy, gRPC, and Google SRE all treat as a standard, separate mechanism from per-attempt
backoff.

**Verdict: NOT YET** for the budget itself (same missing-traffic-volume trigger as Finding
1 — a ratio-of-total-requests metric is meaningless without real aggregate volume), but the
mechanism's SHAPE is well-precedented enough to design/scaffold now.

Evidence: gRPC applies ±20% jitter on every backoff delay, not just a plain exponential
value. Envoy's default retry backoff is fully jittered exponential, 25ms base/250ms cap.
Kelvran's own `docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md` implements equal-jitter
backoff at 25ms base/400ms cap between fallback-chain hops — nearly identical to Envoy's
default — plus a separate per-key client-facing `RetryBackoff` (500ms/30s, streak-capped at
10) and a per-request `maxConsecutiveChainFailures` circuit breaker. What's absent: gRPC's
token-bucket retry-throttle (suppresses ALL retries/hedged RPCs fleet-wide once
`token_count <= maxTokens/2`) and Envoy's `retry_budget` circuit-breaker threshold plus a
dedicated `upstream_rq_retry_overflow` counter for retries suppressed by budget rather than
issued-and-failed, and SRE's hard 3-attempt cap PLUS a separate per-client 10%-of-total
retry-ratio budget. Kelvran's existing controls all operate at the single-request or
single-key scope, never at the fleet/global scope these three sources treat as a standard
second layer.

### Finding 3 — Priority/criticality-tiered load shedding: real precedent, genuinely premature
**Confidence: high**

Priority/criticality-tiered load shedding has real, foundational precedent (Google SRE's
four-tier model with automatic propagation) and one real-but-immature competitor
implementation (LiteLLM's priority-reservation system), but is genuinely premature for
Kelvran today — blocked on the same "no demonstrated multi-tenant/contention signal" trigger
that already deferred hierarchical virtual-key budgets twice.

**Verdict: NOT YET.** Named trigger: any real, concrete multi-tenant demand signal (not
calendar time) — the identical trigger `gateway-hierarchical-budgets-2026-09-07.md` already
established.

Evidence: Google SRE's `CRITICAL_PLUS`/`CRITICAL`/`SHEDDABLE_PLUS`/`SHEDDABLE` tiers,
propagated automatically through the call stack, are the canonical, decades-stable
framework. LiteLLM's dynamic-rate-limiter priority-reservation is real and shipped/documented
— but its actual reservation-by-priority mechanism is gated behind a paid Enterprise license
in the OSS codebase itself (verified directly against `BerriAI/litellm`'s main-branch
source), and LiteLLM's separate priority-QUEUE scheduler feature is explicitly labeled "Beta
feature. Use for testing only" in its own current docs. Even a leading competitor either
paywalls or beta-labels this exact capability.

### Finding 4 — A staging-level fault-injection harness is a real build-now item, independent of production traffic
**Confidence: high**

Kelvran's `routing_chaos` corpus is real, primary-source-grounded scenario mining (concrete
regression fixtures from AWS DynamoDB, Slack, Twilio/Segment, and Cloudflare/Facebook-outage
postmortems) but is a static, fixed-assertion test corpus, not chaos engineering by its own
canon's four-step live-experiment methodology — and "run in production" is an advanced/
aspirational principle, not a baseline requirement, meaning a staging-level live
fault-injection harness is a genuine, low-risk build-now candidate independent of Kelvran's
missing production traffic.

**Verdict: BUILD NOW.**

Evidence: the canonical chaos-engineering methodology requires four live steps — define a
measurable steady state, hypothesize both control/experimental groups hold it, inject real
variables, disprove by comparison — a live, comparative, hypothesis-driven loop. "Run
Experiments in Production" is explicitly one of five ADVANCED principles, not a defining
requirement (a stronger "must be in production to count" framing was checked and explicitly
did NOT survive verification, 1-2 vote). Kelvran's own
`evals-corpus-routing-chaos-patterns-2026-09-08.md` confirms the corpus mines fixed scenarios
into static `EvalCase` fixtures with pass/fail assertions — real, valuable regression
protection, but not a live steady-state hypothesis run continuously against a control group.
Since production experimentation is explicitly not required, a staging/integration-test-level
live fault-injection harness (inject real timeouts/5xx into the actual dataplane code path
during test runs, not just assert against static fixtures) is a defensible, lower-risk
build-now step.

## Open Questions

- Is there a real, checkable bulkhead gap beyond Kelvran's existing per-deployment
  concurrency ceilings (e.g. no reserved-capacity-per-tenant on a shared deployment, so a
  noisy-neighbor virtual key within its own per-key cap could still consume a
  disproportionate share of a shared deployment's aggregate ceiling)? No confirmed claim
  addressed this in this round; a dedicated follow-up pass is needed.
- What is the right numeric traffic-volume floor for Kelvran's passive-health-signal gate
  and/or a future retry budget once real production traffic exists — cross-vendor precedent
  ranges from 20 to 100 requests over 10s-30s windows with no consensus magnitude, so this
  will need Kelvran's own real request-volume data, not a borrowed default.
- Should Kelvran scaffold the retry-budget primitive (Finding 2) now as real, tested, unwired
  code — mirroring the "build the how now, defer the when" pattern
  `gateway-router-health-real-traffic-2026-09-09.md` already used for the passive-health
  filter/gate — or does even the scaffolding lack sufficient design grounding until real
  traffic exists?
- Is there any concrete demand signal (a specific customer, a specific documented overload
  incident) that would flip priority/criticality-tiered shedding from NOT YET to BUILD NOW?

## Caveats

Several sources are first-party vendor docs (Kong, LiteLLM) rather than neutral third
parties; where possible these were cross-checked against source code (LiteLLM's
Enterprise-gate confirmed directly in `BerriAI/litellm`'s main branch) or a second
independent vendor. Kong's `ai-proxy-advanced` circuit breaker (v3.13+) and LiteLLM's
dynamic-rate-limiter/scheduler are comparatively recent features — re-verify continued
availability/maturity before citing as durable precedent in a spec. By contrast, the
Envoy/Istio/Google-SRE-book/gRPC/principlesofchaos.org sources describe long-stable,
foundational mechanisms with low time-sensitivity risk. All "not yet" verdicts in this round
converge on the same underlying trigger Kelvran has already named repeatedly in its own docs:
a real production-traffic-volume floor, which does not yet exist.
