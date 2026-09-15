# Progressive Rollout / Canary Config — Deep Research (2026-09-15)

**Question:** Kelvran is a Go+Python LLM gateway/cache/evals platform (real Bedrock pilot in
production). Its `config.yaml` (deployments, models, rate limits) is loaded once at startup with
no hot-reload, and its Admin API already supports live virtual-key mutation. What upgrade/best-
practice opportunities exist for a progressive rollout / canary release framework for gateway
config and feature changes — e.g. routing a percentage of traffic or a subset of virtual keys to a
new deployment/model/config before it's fully live — and how do comparable API gateways and
feature-flag systems (LaunchDarkly, Envoy's traffic-splitting, Kong's canary releases, LiteLLM)
structure this?

## Summary

Kelvran's router already implements the same primitive every industry canary/traffic-splitting
system reviewed here is built on — a per-deployment weight selecting a share of traffic
(`gateway/internal/router/wrr.go`'s smooth-WRR over deployments per canonical model) — but that
weight is frozen at `Router.New()` from `config.yaml` and has no live-mutation path, unlike
Kelvran's own Admin API pattern for virtual keys (`UpsertVirtualKey`, confirmed live in
`dataplane/virtualkey_admin_test.go`) or the runtime-adjustable weights in Envoy, Kubernetes
Gateway API, and Kong. Those three infra-layer systems converge on one vendor-neutral pattern: a
weight field per backend that is a proportional ratio (not required to sum to 100), dynamically
adjustable via a runtime variable/API without redeploying the static route config — directly
portable to Kelvran's `Deployment.Weight`. LaunchDarkly adds the two layers infra routers don't
provide themselves: a deterministic, coordinator-free hash-based bucketing scheme for
sticky/consistent percentage assignment, and automated staged rollout with regression-triggered
auto-rollback (Progressive/Guarded Rollouts) — both of which would need to be built as a Kelvran
control-plane feature on top of a weight-mutation primitive, not borrowed wholesale. LiteLLM is the
closest existing LLM-gateway precedent and shows hot-reload is retrofittable onto a config-file-
based gateway via simple per-pod DB polling (no push infra required) — but LiteLLM's own router
canary capability is just a static weight ratio with no staging or rollback, meaning even the most
comparable competitor hasn't solved the progressive half of progressive rollout either.

## Findings

### Finding 1 — LaunchDarkly's staged/guarded rollouts are the closest analog for safely graduating a canary to fully live
**Confidence: high**

LaunchDarkly's "Progressive Rollouts" and "Guarded Rollouts" provide an automated, staged
percentage-escalation model (e.g. 10%→25%→50%→100%) for shifting traffic to a new variation, and
Guarded Rollouts additionally layer real-time experimentation-based regression monitoring with
automatic rollback before broad user impact — the closest analog to safely graduating a new
deployment/model/config from canary to fully live without manual step-by-step promotion. No
infra-layer system reviewed (Envoy, Gateway API, Kong) provides this staging/rollback layer itself
— it would be Kelvran control-plane logic sitting on top of a weight-mutation primitive, not
something borrowed from the routing layer.

Sources: launchdarkly.com/docs/tutorials/ld-arch-deep-dive; docs.launchdarkly.com/home/releases/guarded-rollouts

**build_now / not_yet:** not_yet — needs the weight-mutation primitive (Finding 5) first.

### Finding 2 — LaunchDarkly's percentage rollout uses deterministic hash-bucketing for sticky assignment; Kelvran's WRR cursor cannot reproduce this today
**Confidence: high**

LaunchDarkly's percentage rollout uses a deterministic, coordinator-free hash of (flag key/salt +
context key/kind) — SHA1-based, divided into 100,000 numbered buckets — to assign each context a
stable bucket; increasing a rollout's percentage only adds the newly-included bucket range to the
existing variation (sticky/monotonic expansion) rather than re-randomizing already-assigned
contexts. This is the pattern Kelvran would need if canarying should be sticky per virtual
key/tenant rather than per-request-random — notably, Kelvran's current WRR (`modelState.next()` in
`wrr.go`) is a single shared, mutex-protected cyclic cursor with no per-key identity input, so it
cannot reproduce this stickiness today without a separate hash-bucketing layer.

Sources: launchdarkly.com/docs/sdk/concepts/flag-evaluation-rules; launchdarkly.com/docs/home/releases/percentage-rollouts

**build_now / not_yet:** not_yet — a real, named gap, but only worth building once sticky-per-tenant
canarying is an actual requirement, not just a theoretical one.

### Finding 3 — LaunchDarkly targeting-rule evaluation is order-sensitive; a canary schema must preserve explicit rule ordering
**Confidence: high**

LaunchDarkly targeting-rule evaluation is order-sensitive (first rule whose clauses all match wins,
walked in list order after individual-target checks), and a percentage/progressive/guarded rollout
is layered onto an individual targeting rule rather than being a flag-wide setting — meaning a
canary system built the same way must preserve explicit rule ordering (specific virtual keys before
a general X% rule) and treat staged rollout as an attribute of one rule, not the whole flag.
Directly informs how a Kelvran canary config schema should be shaped if it needs to support routing
specific virtual keys to the new deployment, then X% of everyone else — ordering and scope need to
be explicit, not implicit.

Sources: launchdarkly.com/docs/sdk/concepts/flag-evaluation-rules; launchdarkly.com/docs/fed-docs/home/flags/target-rules

**build_now / not_yet:** not_yet — a design constraint to carry into Finding 5's schema, not an
independent build item.

### Finding 4 — Config/flag changes propagate via push (SSE/CDN), not poll, as LaunchDarkly's default
**Confidence: high**

Config/flag changes propagate to already-running clients via push (CDN edge distribution +
Server-Sent-Events streaming), applied as an in-memory update with no restart or redeploy, rather
than each instance polling for changes — LaunchDarkly's default connection mode. Relevant as the
alternative to LiteLLM's poll-based model for how Kelvran's replicas could converge on a mutated
canary weight faster than a poll interval, if push infra is later justified.

Sources: launchdarkly.com/docs/tutorials/ld-arch-deep-dive; docs.launchdarkly.com/sdk/concepts/contributors-guide

**build_now / not_yet:** not_yet — poll-based (matching LiteLLM's own precedent) is the right v1;
push is a future optimization once multi-instance canary convergence latency is actually measured
as a problem.

### Finding 5 — Envoy, Kubernetes Gateway API, and Kong converge on the same weighted-backend pattern Kelvran's `Deployment.Weight` should adopt live-mutation of
**Confidence: high**

Envoy, the Kubernetes Gateway API, and Kong Ingress Controller converge on the same vendor-neutral
weighted-backend pattern: a weight field per backend/cluster that is a proportional ratio (not
required to sum to 100, and if only one backend is specified it implicitly gets 100% regardless of
weight), computed as weight divided by sum of all weights in that rule, and dynamically adjustable
at runtime (Envoy via a named `runtime_key_prefix` variable) without reloading the static route
config — explicitly designed for both gradual version-upgrade canaries and simultaneous A/B/
multivariate testing.

Sources: envoyproxy.io/docs/envoy/latest/configuration/http/http_conn_man/traffic_splitting;
gateway-api.sigs.k8s.io/guides/user-guides/traffic-splitting; developer.konghq.com/kubernetes-ingress-controller/routing/weights

**build_now / not_yet:** build_now — the single most directly portable, highest-leverage finding in
this report. Kelvran already has `Deployment.Weight` and the WRR mechanism that consumes it; the
gap is purely that it's frozen at startup. Extending the Admin API with a weight-mutation route
(mirroring the already-shipped live virtual-key mutation pattern) is small, additive, and needs no
new infrastructure.

### Finding 6 — LiteLLM proves hot-reload is retrofittable onto a config-file-based gateway via simple polling, but hasn't itself solved staged/rollback canarying
**Confidence: medium-high**

LiteLLM is the closest existing LLM-gateway precedent and shows hot-reload is retrofittable onto a
config-file-based gateway via simple per-pod DB polling (no push infra required) — but LiteLLM's
own router canary capability is just a static weight ratio with no staging or rollback, meaning
even the most comparable competitor hasn't solved the progressive half of progressive rollout
either. This is a genuine market gap, not just a Kelvran gap.

**build_now / not_yet:** not_yet for the staged/rollback layer specifically (depends on Finding 5
shipping first); the polling-based hot-reload mechanism itself is covered by the separate
`config-hot-reload-2026-09-15.md` research already in this directory — this report intentionally
does not re-litigate that mechanism, only the canary-weighting layer on top of it.

## Recommendation

Ship the weight-mutation primitive first (Finding 5) — extend the Admin API with a route to update
`Deployment.Weight` live, reusing the exact live-mutation pattern already proven for virtual keys.
Everything else in this report (sticky bucketing, staged/guarded rollout, rule ordering, push
propagation) is real, well-precedented follow-on work that depends on that primitive existing, not
independent build items today.
