# Data Residency & Regional Routing — Upgrade Research (2026-09-15)

Recovered by hand from the deep-research workflow's own returned JSON — its file-write step failed
silently, the same recurring class per `AGENTS.md`'s Gotchas section. Reconstructed from the
workflow's full structured result, not re-run. The two most decision-relevant findings below were
independently re-verified directly against the live `gateway/` source in this same recovery pass
(via `grep`/file reads), not merely trusted from the workflow's own claim-verification pass.

## Current state, confirmed by direct code trace

**`Region` is purely a Bedrock-endpoint/SigV4-signing detail with zero tenant-facing routing
effect.** `DeploymentConfig.Region` (`gateway/internal/gateway/controlplane/config.go:72`),
`dataplane.Deployment.Region` (`dataplane.go:166`, consumed at line 3078's `signer.SignHTTP` call),
and a separate `Region` field on the Bedrock guardrail detector (`bedrockguard.go:51`, used to build
the endpoint URL and sign) are the ONLY places `Region` appears in the codebase outside tests. A
repo-wide `grep -n Region` against `internal/router` (`deployment.go`, `wrr.go`, `health.go`,
`samplewindow.go`, `router.go`) returns zero matches.

## Finding — a real correctness/compliance gap, not just a missing feature

**`attemptFallbackChain` (`gateway/internal/gateway/dataplane/fallback.go`, lines 372-434) has no
region gate.** Its candidate filter checks exactly four things: `IsHealthy`, `rateLimitOK`,
`deploymentCapacityOK`, `capabilityOK` — never `Region`. This means a same-model WRR fallback can
silently move a request from an EU-pinned Bedrock deployment to a non-EU deployment sharing the same
canonical model name today, with no override available. This is not theoretical — it's the exact
failure mode the industry already names and mitigates:

- OpenRouter explicitly frames its own fallback mechanism as a residency risk unless
  `allow_fallbacks=false` is set.
- Microsoft's official Azure OpenAI gateway architecture guidance states flatly that
  performance/health-based routing must be scrutinized for sovereignty compliance, and that
  "clients must be blocked from crossing geopolitical gateway boundaries" — the same general class
  of routing as Kelvran's WRR/health fallback.
- Even AWS's own Bedrock "Global" cross-region inference profile can route anywhere commercially for
  performance, with no compliance awareness at all.

**Verdict: build_now.** Add a region gate to `attemptFallbackChain`'s candidate filter, alongside
the existing four gates.

## Finding — the fix has an exact structural precedent already live in Kelvran

`identity.VirtualKey.AllowedModels` (`identity.go:77`, `map[string]struct{}`, empty/nil = every
model allowed) is enforced via `isModelAllowed` (`dataplane.go:1444-1449`) called right after auth
at `dataplane.go:1502`. An `AllowedRegions []string` or `RequiredRegion string` field is the same
shape — with one real wiring difference: `AllowedModels` is checked once against `req.Model`, but
`Region` lives on `Deployment`, so an `isRegionAllowed(vk, dep.Region)` check needs to run
**per-candidate-deployment**, inside both the router's selection path AND `attemptFallbackChain`'s
gate list — not just once at the top of the pipeline.

**Verdict: build_now**, with that wiring caveat.

## External precedent — this is a real, shipped 2026 feature in this exact product class

- **OpenRouter's `allowed_data_regions` guardrail** (global/europe/us) is settable at
  workspace-default, member, or API-key level, resolved by intersection (narrowing-only), and
  hard-rejects with HTTP 403 before inference — functionally the same shape as Kelvran's own
  `AllowedModels`. Confirms per-tenant region pinning is real and shipped, not hypothetical. (Note:
  OpenRouter's US in-region routing only reached GA 2026-09-09 — six days before this report — so
  treat this specific precedent as bleeding-edge and re-verify if acted on much later.)
- **AWS Bedrock's own CRIS** ships two distinct inference-profile types precisely to separate
  residency-constrained from unconstrained routing: "Geographic" profiles (bounded to a geography —
  US/EU/Australia/Japan — via a static profile-ID prefix like `eu.<model>`) for compliance, versus
  "Global" profiles (route anywhere commercially, ~10% cheaper). Design implication: **Kelvran's
  field should model geography buckets, not literal single-region strings** — even AWS's own
  "Geographic" bucket spans multiple literal regions (an EU request can land in Frankfurt, Ireland,
  or Paris interchangeably), so a literal single-region field would be false precision the provider
  itself doesn't offer.
- **LiteLLM (the closest OSS comparable) has no per-tenant region-pinning field in its core routing
  logic at all** — its "Global Control Plane" (Enterprise) achieves isolation via fully separate
  per-region infrastructure, not an in-router field. This means building `AllowedRegions` would be
  genuinely differentiating for Kelvran, not table-stakes catch-up.

## What this does NOT fix — disclosed, not silently implied as solved

Even AWS's own Bedrock CRIS only ever guarantees data **storage** stays in the source region — never
processing/transit — and puts the compliance-evaluation burden on the customer. An `AllowedRegions`
field constrains **which Kelvran-configured deployment is chosen**; it cannot strengthen the
upstream provider's own storage-vs-processing residency guarantee. This is exactly consistent with
`docs/operations/PROVIDERS.md`'s own existing disclosure ("Region-pinned by the customer's own
Bedrock configuration... Not independently verified by this repo") — that disclosure posture is the
correct response here, not something this finding should override.

Microsoft's guidance is the primary counterweight against treating an `AllowedRegions` field as a
**complete** solution: full geopolitical compliance separation wants fully independent per-region
gateway infrastructure, not just a routing field. **Verdict: not_yet** for that heavier
infrastructure-separation step — it needs an actual multi-region deployment topology Kelvran doesn't
have yet (a real trigger: Kelvran itself deployed across more than one physical region). The
fallback-chain region gate above is the narrower, actionable piece Kelvran can build today without a
topology change.

## Refuted (excluded from the findings above)

- Portkey's "data residency" is vague managed-infrastructure marketing language with zero documented
  technical mechanism in its own current docs (no dedicated residency page found).
- A claimed "Hadrian Gateway" `sovereignty_requirements` object (`allowed_inference_countries`,
  `required_certifications`, `blocked_hq_countries`) was investigated and explicitly refuted (0-3) —
  do not cite it as real, shipped precedent.
- A claim that OpenRouter's in-region routing fails closed (errors rather than silently falling back
  out-of-region) was checked and refuted (0-3) — don't assume OpenRouter's own fallback is
  automatically safe; it needs `allow_fallbacks=false` explicitly set, per the finding above.

## Open questions (not resolved this round)

- Does Kelvran have (or plan) any actual multi-region deployment topology today, or is every current
  `DeploymentConfig.Region` value effectively the same single region in practice — i.e. is the
  `attemptFallbackChain` cross-region gap merely latent (no real cross-region deployments configured
  yet) or already live in a production config with EU + non-EU Bedrock deployments of the same
  canonical model?
- Should a region field store literal AWS region strings or geography buckets (mirroring AWS's own
  Geographic CRIS boundary)?
- Should the fallback-chain region gate be opt-in per virtual key (like OpenRouter's
  `allow_fallbacks=false`) or a hard, always-on invariant once any region constraint exists on a
  key — i.e. fail-open or fail-closed by default when no compliant fallback target exists?
- Does `PROVIDERS.md` need a corresponding update once/if `AllowedRegions` ships, to make clear the
  field only constrains which Kelvran-configured deployment is chosen?

## Sources

Primary: `openrouter.ai/docs` (in-region routing, sovereign-ai, guardrails), AWS Bedrock CRIS docs
and ML blog posts (Geographic vs. Global profiles, residency guarantees), Microsoft's
`azure-openai-gateway-multi-backend.md` architecture-center doc, `docs.litellm.ai` (control
plane/data plane, global control plane). 22 sources fetched across 5 search angles; 105 claims
extracted, 25 adversarially verified (16 confirmed, 9 refuted), synthesized to 11 findings above,
plus 2 claims independently re-verified against Kelvran's own live source in this recovery pass.
