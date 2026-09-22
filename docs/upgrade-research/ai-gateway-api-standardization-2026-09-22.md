# Kelvran Gateway — Is a Standardized "AI Gateway API" Emerging? (2026)

**Date:** 2026-09-22
**Scope:** `gateway/` — whether an emerging, vendor-neutral "AI Gateway API" specification exists or is converging in 2026, analogous to how the Kubernetes Gateway API standardized ingress/routing config across implementations. Specifically: the Gateway API project's own inference-extension work, any CNCF/Linux Foundation AI-gateway standardization effort, and whether Kong, Envoy AI Gateway, and LiteLLM are converging on a shared config schema or API surface. This report answers a narrower, standards-landscape question than the sibling `gateway-2026-09-06.md` competitor-feature report — it does not re-litigate that report's findings.
**Method:** Primary-source research only — official project READMEs/docs/charters fetched directly (`gh api` for GitHub repos, direct doc-site fetches), not blog summaries. Every claim below states its source and how many independent primary sources it was checked against.

---

## Executive summary

No vendor-neutral "AI Gateway API" analogous to the Kubernetes Gateway API exists yet at the layer Kelvran actually competes in, and none is close. What does exist is a **GA'd standard for a different, narrower problem** — routing to self-hosted model-server Pods inside a single Kubernetes cluster, based on live serving metrics like KV-cache utilization — and a **six-month-old, proposal-stage effort** at the layer that would matter to Kelvran, with no shipping implementation from any named competitor. The research converges on one central finding: the Kubernetes ecosystem has split "AI Gateway" into two genuinely different standardization efforts, and Kelvran sits entirely outside the one that has actually shipped. The **Gateway API Inference Extension** (GIE, `kubernetes-sigs/gateway-api-inference-extension`) reached GA in September 2025 and its `InferencePool` CRD graduated to a stable v1 API — but GIE's own README explicitly draws the boundary that excludes Kelvran: it optimizes "self-hosting Generative Models on Kubernetes" and positions itself as the layer *underneath* "a higher level AI Gateway like LiteLLM, Solo AI Gateway, or Apigee" [github.com/kubernetes-sigs/gateway-api-inference-extension — confirmed, direct README read]. Kelvran never hosts inference itself, so it has no Pods with KV-cache/queue-depth telemetry to route across — this independently confirms, from a completely different primary source, the same structural boundary Kelvran's own prior cache research (`cache-next-upgrade-round3-2026-09-11.md`) already drew.

The layer that *would* matter to Kelvran — standardizing egress routing to hosted, multi-provider model APIs (OpenAI, Anthropic, Bedrock, Gemini) with auth injection, failover, and payload inspection — is the subject of a real but nascent effort: the Kubernetes **AI Gateway Working Group** (`wg-ai-gateway`), announced 2026-03-09 on the official Kubernetes community blog [kubernetes.dev — confirmed, primary]. Six-plus months in, its two active proposals (Egress Gateways, Payload Processing) are both still status `Proposed`, ship no reference implementation, and the repo's own README states flatly that "any code contained in this repo is a prototype only and NOT suitable for anything resembling production" [github.com/kubernetes-sigs/wg-ai-gateway — confirmed, direct source read]. Against that backdrop, the three named competitors show **zero observed convergence** on a shared schema at Kelvran's layer: Envoy AI Gateway had to invent its own proprietary `AIGatewayRoute` CRD (`aigateway.envoyproxy.io/v1alpha1`) to do cross-provider fallback and token-based rate limiting, after a multi-month, twice-restarted engineering effort documented in its own GitHub issue tracker [github.com/envoyproxy/ai-gateway#423 — confirmed, direct issue thread read]; Kong's own standardization story is a de facto wire-protocol convention (translating every provider into an OpenAI-compatible request/response shape), not a Kubernetes CRD or Gateway API extension [konghq.com/blog — confirmed, primary vendor source]; and LiteLLM has no native Gateway API or Kubernetes CRD story at all — the only Kubernetes "operator" found for it is a third-party project (PalenaAI, not BerriAI, LiteLLM's own maintainer) with its own bespoke, incompatible CRD group [litellm-operator.palena.ai, github.com/PalenaAI/litellm-operator — confirmed, direct doc/source read; explicitly flagged below as third-party, not upstream]. No distinct CNCF or Linux Foundation standards body for "AI Gateway" was found; the actual locus of standardization work is internal Kubernetes governance (SIG Network, successor to the now-disbanded WG Serving), not a CNCF-branded specification process — CNCF's own blog covers the WG Serving conclusion as news, but does not itself own or charter the work [cncf.io/blog — confirmed, primary].

**Bottom line for Kelvran:** premature to track this as an adoption candidate, and clearly premature to build against it. The one GA'd standard doesn't apply to Kelvran's architecture; the one effort that would apply is pre-alpha and unimplemented by any peer; and the peers Kelvran is actually benchmarked against in this repo's own convention (Kong, Envoy AI Gateway, LiteLLM) have each gone their own proprietary way at the layer that matters. This is a "monitor lightly, revisit in 6-12 months" verdict, not a "no forever" one — see Top 3 do next.

---

## Findings, ranked

### 1. Two genuinely different "AI Gateway" standardization efforts exist inside Kubernetes governance, and only the one Kelvran does NOT need has reached GA (High confidence)

**What:** The Kubernetes community runs two distinct efforts under the "AI Gateway" banner, and they solve different problems for different audiences:
- **Gateway API Inference Extension (GIE)** — sponsored by SIG Network, previously incubated under the now-concluded WG Serving — standardizes routing *within a cluster* to self-hosted model-server Pods (vLLM, etc.), using an `InferencePool` backend resource and an Endpoint Picker (EPP) extension that makes routing decisions from live serving telemetry (KV-cache utilization, queue depth, active LoRA adapters) [gateway-api-inference-extension.sigs.k8s.io — confirmed, direct docs-site read].
- **AI Gateway Working Group (`wg-ai-gateway`)** — a separate, newer working group formed specifically because GIE's scope explicitly excludes it — standardizes the *outward-facing* AI-gateway concerns: egress routing to external hosted APIs, payload inspection/guardrails, and token-based rate limiting at the `Gateway`/`HTTPRoute` level [kubernetes.dev/blog/2026/03/09/announcing-ai-gateway-wg — confirmed, primary].

The WG's own charter makes this split explicit and deliberate, not accidental: "there is a subtle distinction... When the use case includes local model serving on the cluster, and routing and load-balancing features *rely on information from the inference workloads*, this kind of routing falls under the scope of WG Serving [i.e., GIE]... this WG means to operate more at the `Gateway` and `HTTPRoute` level" [github.com/kubernetes/community/blob/master/wg-ai-gateway/charter.md — confirmed, direct source read].

**Why it matters for Kelvran specifically:** this is the load-bearing distinction for everything else in this report. GIE is GA'd, multi-vendor, and stable — but it standardizes routing across a pool of *self-hosted* model-server Pods that Kelvran does not run and structurally cannot have (Kelvran calls hosted provider APIs — Anthropic, OpenAI, Bedrock, Gemini — that never expose Pod-level KV-cache telemetry). The effort at Kelvran's actual layer — multi-provider egress routing — is the newer, unfinished one.

**Consistent with settled decisions?** Reinforces, from an independent primary source, the same conclusion `cache-next-upgrade-round3-2026-09-11.md` already reached when evaluating GIE-style KV-cache-aware *routing* for caching purposes: "this requires direct Kubernetes/Gateway control over self-hosted GPU workers publishing their own KV-cache-state events — a mechanism with no third-party-hosted-API equivalent." This report reaches the same boundary independently, for the standardization question rather than the caching question, which raises confidence in both findings.

**2026 best practice grounding:** GIE's own README states plainly it exists to "integrate your self-hosted models alongside model-as-a-service providers in a higher level AI Gateway like LiteLLM, Solo AI Gateway, or Apigee" [github.com/kubernetes-sigs/gateway-api-inference-extension — confirmed]. `InferencePool` graduated to a stable v1 API as of GIE v1.0.0 (2025-09-09) [gateway-api-inference-extension.sigs.k8s.io/api-types/inferencepool — confirmed, direct docs read].

**Concrete next step:** none. Confirms Kelvran's existing "not applicable" posture toward GIE/InferencePool is correct — no action needed, just don't reopen this question expecting a different answer next time GIE ships a release.

**Effort to track:** None — structurally inapplicable, not merely deprioritized.

---

### 2. The AI Gateway Working Group — the one effort that would matter to Kelvran — is six months old, proposal-stage, and explicitly not production code (High confidence)

**What:** `wg-ai-gateway` was announced 2026-03-09 [kubernetes.dev/blog/2026/03/09/announcing-ai-gateway-wg — confirmed, primary, official Kubernetes community blog]. As of this research date (2026-09-22), its two substantive proposals are:
- **Egress Gateways** (proposal 10, status `Proposed`) — defines a `Backend` resource plus `Gateway`/`HTTPRoute` routing modes for reaching external hosted AI APIs (OpenAI, Vertex AI, Bedrock), with auth-token injection and failover — the closest thing to a future standard for exactly what Kelvran's router does today [github.com/kubernetes-sigs/wg-ai-gateway/blob/main/proposals/10-egress-gateways.md — confirmed, direct source read].
- **Payload Processing** (proposal 7, status `Proposed`) — defines a declarative, ordered pipeline of request/response processors (PII redaction, prompt-injection detection, guardrails) attached to `HTTPRoute` rules — a future standard for what Kelvran's guardrail layer does today [github.com/kubernetes-sigs/wg-ai-gateway/blob/main/proposals/7-payload-processing.md — confirmed, direct source read].

Both proposals list open design questions still unresolved (e.g., how Payload Processors interact with existing `HTTPRoute` filters, whether processing loops are possible, protocol shape TBD). The WG's own repository states: "As a Kubernetes working group, we do not *directly* own projects or code, our purpose is to make proposals... Any code contained in this repo is a prototype only and NOT suitable for anything resembling production" [github.com/kubernetes-sigs/wg-ai-gateway — confirmed, direct README read].

**Why it matters for Kelvran specifically:** if a standard ever emerges that Kelvran should track, this is where it will come from — the Egress Gateways proposal is a near-exact conceptual match for Kelvran's own router/adapter/budget stack. But "proposal-stage, no reference implementation, explicitly PoC-only" is about as early as a standardization effort can be while still existing at all. There is nothing shippable to adopt today, and the API shapes shown in the proposal docs are illustrative, not final.

**Consistent with settled decisions?** New information — no prior Kelvran research doc names `wg-ai-gateway` specifically (confirmed via grep of this directory).

**2026 best practice grounding:** as cited above. Notably, this WG is the direct successor to **WG Serving**, which formally concluded 2026-02-26 after "successfully" seeding GIE and folding remaining work into SIG Network and other groups, per CNCF's own blog: "Multiple groups have worked to standardize AI gateway functionality, and early inference gateway participants went on to seed agent networking work in SIG Network" [cncf.io/blog/2026/02/26/kubernetes-wg-serving-concludes — confirmed, primary]. This is a healthy governance signal (a WG that actually exits when its scope is done) but also confirms the *current* WG AI Gateway is genuinely new, not a rebrand of a mature effort.

**Concrete next step:** none to build. Worth a calendar reminder (6-12 months out) to re-check whether either proposal has graduated past `Proposed` or produced a reference implementation in Envoy Gateway, kgateway, or GKE Gateway — the three implementations GIE itself names as its production integration points.

**Effort to track:** Small — a periodic (quarterly) check of proposal status, not an active watch.

---

### 3. Envoy AI Gateway — the most standards-aligned of the three named competitors — still had to build its own proprietary CRD for Kelvran's actual feature set, after a messy, multi-month integration effort (Medium-high confidence)

**What:** Envoy AI Gateway ships two distinct integration paths with GIE's `InferencePool`:
1. **`HTTPRoute` + `InferencePool`** — the standard Gateway API path, for "basic load balancing and endpoint selection" [aigateway.envoyproxy.io/docs/0.4/capabilities/inference/httproute-inferencepool — confirmed, direct docs read].
2. **`AIGatewayRoute` + `InferencePool`** — Envoy's own proprietary CRD (`apiVersion: aigateway.envoyproxy.io/v1alpha1`), required for "advanced AI-specific features like model-based routing, token rate limiting, and advanced observability" [aigateway.envoyproxy.io/docs/0.3/capabilities/inference/aigatewayroute-inferencepool — confirmed, direct docs read].

Cross-provider fallback, token-based rate limiting, and model-name-based routing across multiple providers — precisely the feature set this repo's own `gateway-2026-09-06.md` and `gateway-tpm-permodel-fallback-2026-09-09.md` benchmark Kelvran against — live only in path 2, the non-standard CRD. The path to get there was not smooth: `envoyproxy/ai-gateway` issue #423 (opened 2025-02-25) documents an initial implementation landing, then being fully reverted ("we removed the entire initial implementation... The reason was that we had to do the massive refactoring in v0.2... to implement... cross-provider fallbacks"), a period where "Inference Extension support" was "temporarily disable[d]... on the main branch," and an explicit open decision — as of the most recent comment in the thread — about "which feature of the current AIGW's feature we want to support in the GAIE support," because backend security policy and transformers "are not needed" for the standards path but "metrics as well as the dynamic metadata extraction for rate limit should be useful" [github.com/envoyproxy/ai-gateway#423 — confirmed, direct issue thread read, spanning Feb 2025 through mid-2025].

**Why it matters for Kelvran specifically:** Envoy AI Gateway is the vendor with the strongest structural incentive to standardize (it's built directly on Envoy Gateway, one of the most mature Gateway API implementations) and the closest working relationship with GIE's maintainers of any of the three named competitors. If convergence toward a shared multi-provider-routing schema were happening anywhere, evidence would show up here first. It hasn't: even Envoy's own team treats `InferencePool` as solving only the self-hosted-backend-selection sub-problem and had to design a separate, non-standardized surface for the provider-fallback/rate-limiting problem Kelvran actually solves.

**Consistent with settled decisions?** Corroborates, rather than revisits, `wasm-plugin-extensibility-2026-09-22.md`'s independent finding (via a completely different technical question — WASM plugin extensibility, not Gateway API) that this space moves fast and reverses course often — that report found Kong fully removed WASM support after two years; this one finds Envoy AI Gateway fully reverted and rebuilt its inference-extension integration mid-stream. Two independent findings, same "this ecosystem is still finding its footing" texture.

**2026 best practice grounding:** as cited above, both from Envoy's own current documentation and its own GitHub issue tracker — not third-party commentary.

**Concrete next step:** none. If Kelvran ever evaluates "should we expose a Gateway-API-compatible ingress surface," Envoy AI Gateway's `AIGatewayRoute` shape (not `InferencePool`) is the closer analogue to study — but that's a future, not current, evaluation trigger.

**Effort to track:** None currently — informational only.

---

### 4. Kong's and LiteLLM's own "standardization" stories are unrelated to Gateway API, and to each other (Medium-high confidence)

**What:** Two more data points against convergence, from the two other competitors named in the research brief:
- **Kong AI Gateway 2.0** (GA'd 2026-09-01) standardizes at the wire-protocol level, not the Kubernetes-API level: its SageMaker-support announcement explicitly frames the approach as "rather than inventing a SageMaker-specific format, we leaned on the architecture that already works: the plugin standardizes on the broadly supported OpenAI format and converts internally" [konghq.com/blog/product-releases/kong-ai-gateway-2-0-ga — confirmed, primary vendor source]. No mention of Gateway API, `InferencePool`, or `wg-ai-gateway` anywhere in this announcement. Kong does run on Kubernetes via its own Ingress Controller [github.com/kong/kong — confirmed, direct README read], but that is general-purpose K8s ingress, not AI-gateway-specific standardization.
- **LiteLLM** has no native Kubernetes CRD or Gateway API story at all. GIE's own README names LiteLLM as a "higher level AI Gateway" peer — but only as an example of what sits *above* GIE, with no claim LiteLLM itself consumes GIE's APIs. The only Kubernetes "operator" found for LiteLLM is a third-party community project — `PalenaAI/litellm-operator`, **not published by BerriAI** (LiteLLM's own maintaining organization) — which wraps LiteLLM deployment in nine bespoke CRDs under its own `litellm.palena.ai/v1alpha1` API group (`LiteLLMInstance`, `LiteLLMModel`, `LiteLLMTeam`, etc.), entirely unrelated to `gateway.networking.k8s.io` or `inference.networking.k8s.io` [litellm-operator.palena.ai/reference/crds, github.com/PalenaAI/litellm-operator — confirmed, direct docs/source read; flagged explicitly as third-party evidence, not upstream LiteLLM policy].

**Why it matters for Kelvran specifically:** these are the same three vendors `gateway-2026-09-06.md` benchmarks Kelvran's own feature set against, and none of the three shows any pull toward a shared schema for the layer Kelvran competes in. If anything, the *direction* of movement is toward each vendor's own proprietary lingua franca (Kong: OpenAI-shaped wire format; Envoy: a proprietary CRD; LiteLLM: a Python-config-file model with no CRD story of its own at all).

**Consistent with settled decisions?** New information, does not revisit any prior finding — but it directly informs `gateway-competitor-gaps-2026-09-07.md`'s and `gateway-2026-09-06.md`'s own convention of benchmarking against these three vendors: this report adds that none of them is moving toward a common schema Kelvran might one day need to interoperate with.

**2026 best practice grounding:** as cited above, from Kong's own blog and both LiteLLM/GIE's own README and the (explicitly third-party) LiteLLM operator's own docs.

**Concrete next step:** none.

**Effort to track:** None currently.

---

### 5. No distinct CNCF or Linux Foundation "AI Gateway" standards body exists — the real governance locus is internal Kubernetes SIG/WG process, which is narrower and slower than a cross-industry spec process (Medium confidence)

**What:** The research brief asked specifically about a CNCF/Linux Foundation AI-gateway standardization effort, separate from the Kubernetes Gateway API project itself. None was found. What exists instead:
- The standardization work lives entirely inside Kubernetes' own community governance — `wg-ai-gateway`, sponsored informally by SIG Network, following the standard Kubernetes WG lifecycle (propose → build consensus → hand deliverables to SIGs → exit) [github.com/kubernetes/community/blob/master/wg-ai-gateway/charter.md — confirmed].
- CNCF's own blog *reports on* this ecosystem (e.g., covering WG Serving's conclusion) but does not itself charter or own an AI-gateway spec [cncf.io/blog/2026/02/26/kubernetes-wg-serving-concludes — confirmed, primary, but as reportage, not governance].
- The one CNCF-recognized project actually named "AI-native gateway" in this research (Higress, per this directory's own `wasm-plugin-extensibility-2026-09-22.md`) is a CNCF *Sandbox project* — i.e., one vendor's product accepted into CNCF's project portfolio — not a CNCF *specification* other vendors are converging on.

**Why it matters for Kelvran specifically:** this changes the shape of the "should we track this" question. A CNCF-chartered, cross-vendor spec process (the kind that produced OpenTelemetry's semantic conventions, which `gateway-2026-09-06.md` Finding 3 already tracks) carries different adoption risk than a single-community, Kubernetes-internal WG whose own charter says its job is done once it hands off proposals to SIGs. The latter is real but slower, narrower in reach (Kubernetes-only, not a cross-platform HTTP/REST spec), and has no committed timeline.

**Consistent with settled decisions?** Complements, rather than contradicts, `gateway-2026-09-06.md`'s own precedent of treating OTel GenAI semantic conventions (a genuine CNCF/OTel cross-vendor spec) as worth tracking even at "Development" maturity — the contrast sharpens why *that* effort is worth tracking and *this* one is not yet: OTel GenAI has a real spec repo with a maturity ladder; `wg-ai-gateway` has two `Proposed`-status documents and no reference implementation.

**2026 best practice grounding:** as cited above — absence of evidence here is treated as informative (a specific negative search result), not as "unresearched."

**Concrete next step:** none. If a genuine CNCF-branded, cross-vendor AI-gateway spec (comparable in ambition to OpenTelemetry) is announced later, that would be the trigger to revisit — not incremental progress inside `wg-ai-gateway` alone.

**Effort to track:** None currently.

---

### 6. Even GIE's GA'd, multi-vendor-adopted surface is still undergoing real API-ownership churn — a caution against over-trusting "GA" labels in this space generally (Medium confidence)

**What:** Despite GIE's README stating "This project is GA'd!," the same README (fetched live, 2026-09-22) discloses that several of the project's own APIs are actively being relocated out of the flagship repository: "The Endpoint Picker (EPP), InferenceObjective and InferenceModelRewrite APIs, and Body Based Router (BBR) packages have moved to new repositories... No new code will be accepted to these packages in this repository, and they will be archived soon" — moving instead to `llm-d/llm-d-router` and `llm-d/llm-d-inference-payload-processor`, both under the `llm-d` organization (a vLLM-ecosystem-adjacent community, not `kubernetes-sigs`) [github.com/kubernetes-sigs/gateway-api-inference-extension — confirmed, direct README read, current as of this research date]. The flagship `kubernetes-sigs` repo retains only the `InferencePool` CRD itself, a lightweight reference EPP for conformance testing, and the conformance test suite.

**Why it matters for Kelvran specifically:** this is a general caution, not a Kelvran-actionable item — but it directly informs the honesty of the "GA means stable" framing used elsewhere in this report. Even the one part of this ecosystem that has reached formal GA status is mid-reorganization on its own governance boundaries (what belongs in `kubernetes-sigs` proper versus a narrower, vendor-adjacent community project) just over a year after that GA milestone.

**Consistent with settled decisions?** New information; reinforces this directory's own recurring "this is a fast-moving space, re-verify before citing" caveat pattern (explicitly named in `wasm-plugin-extensibility-2026-09-22.md`, `progressive-rollout-canary-config-2026-09-15.md`, and `gateway-2026-09-06.md`'s own Caveats section).

**2026 best practice grounding:** as cited above, direct primary-source README read at research time.

**Concrete next step:** none for Kelvran directly — flagged for completeness so a future reader of this report doesn't mistake "GA'd" for "governance-settled."

**Effort to track:** None currently.

---

## Top 3 do next

1. **Do nothing to Kelvran's code or architecture as a result of this report.** Every named standardization surface either doesn't apply to Kelvran's architecture (Finding 1) or is too immature to build against (Findings 2-4). This is the correct verdict, not a placeholder for "we ran out of time to find something."

2. **Add a short note to `gateway/ARCHITECTURE.md` (or wherever `PRD.md`'s scope-outs are tracked) recording this report's central finding**: GIE/`InferencePool` is confirmed structurally inapplicable (Finding 1), and no standard exists yet at Kelvran's own layer (Findings 2-4) — so a future contributor doesn't independently re-research this same question from scratch. Cheap, and directly reusable the next time someone asks "should we adopt Gateway API."

3. **Set a lightweight, calendar-based re-check (6-12 months out, not a build task) on `wg-ai-gateway`'s Egress Gateways proposal specifically** (Finding 2) — it is the one concrete future analogue to Kelvran's own router/adapter/auth-injection stack, and its status (`Proposed` today) is exactly the kind of thing that could graduate to a real GEP with a reference implementation in Envoy Gateway or kgateway within that window. Nothing to build today; just don't let it go stale for years the way `gateway-2026-09-06.md`'s own Envoy v1.1 findings did.

**Worth naming explicitly, not for action:** the fact that Kelvran is deliberately *not* Kubernetes-CRD-native today (per `AGENTS.md`'s and this directory's own settled framing — a directly-configured Go binary, not a K8s-Ingress-fronted control plane) means even a hypothetical future `wg-ai-gateway` GEP would only become relevant if Kelvran's own deployment model changes first. That is a much bigger, separate decision than "is there a standard to adopt," and this report does not make a case for making it.

---

## Caveats

- **Single-session primary-source research, not adversarially voted.** Unlike `gateway-2026-09-06.md`'s 3-vote-per-claim convention, this report was produced from direct primary-source fetches (official READMEs, docs sites, charters, GitHub issue threads) cross-checked against each other where multiple sources existed, but without a separate adversarial-verification pass. Confidence tags reflect source-directness (a project's own README/docs/charter, read live) rather than a multi-agent vote count.
- **Time-sensitivity is real and probably the single biggest risk to this report's shelf life.** `wg-ai-gateway` is six months old and both its live proposals are explicitly pre-implementation; GIE itself is mid-reorganization on its own repo boundaries (Finding 6) a year after GA. Anything in Findings 1-4 could look different within another two or three GIE/WG-AI-Gateway release cycles. Re-verify before citing this report in a design doc more than ~6 months old.
- **Vendor coverage matches the research brief's own named set (Kong, Envoy AI Gateway, LiteLLM) plus the Kubernetes-governance side (GIE, `wg-ai-gateway`) — it does not cover every AI gateway vendor.** Portkey, TensorZero, Bifrost, Helicone, Cloudflare AI Gateway, and Solo/Gloo/kgateway were not independently re-researched here; some were already found to have zero surviving claims in `gateway-2026-09-06.md`'s own Caveats section, and that gap is not closed by this report.
- **The LiteLLM-operator finding rests on a single third-party project (PalenaAI), not LiteLLM's own maintainers (BerriAI).** It is presented as evidence about *the ecosystem's* current lack of convergence, not as a claim about BerriAI's own roadmap or intentions — BerriAI could ship a native Gateway-API-aligned operator at any time without this report's knowledge.
- **"No CNCF/Linux Foundation AI-gateway standardization effort was found" is an absence-of-evidence finding, not a guarantee of absence** — per this directory's own established convention (`gateway-2026-09-06.md` Finding 6's identical framing for the prompt-caching question). A dedicated, narrower search specifically inside CNCF TAG-Network or LF AI & Data Foundation meeting notes (not just their public blog) was not performed and could turn up something this pass missed.

## Open questions

- If `wg-ai-gateway`'s Egress Gateways proposal graduates to a real GEP with a shipping reference implementation in the next 6-12 months, does Kelvran's own deployment model (a directly-configured Go binary, not K8s-CRD-native) change the calculus for adopting it, or does that remain a separate, bigger decision than this report addresses? Not resolved here — flagged for the calendar re-check in Top 3 do next.
- Does Solo.io's Gloo AI Gateway / the CNCF-adjacent `kgateway` project (named by GIE's own README as one of three production Inference Gateway implementations, alongside Envoy Gateway and GKE Gateway) show any different convergence story than Envoy AI Gateway's own proprietary-CRD path (Finding 3)? Not independently investigated in this pass — a natural follow-up if Kelvran ever evaluates Envoy AI Gateway more deeply.
- Does BerriAI (LiteLLM's own maintaining org) have any unpublished or roadmap-stage Gateway API / Kubernetes-operator plans that would supersede the third-party PalenaAI operator found here? Not discoverable from public docs alone.
- Is there a genuinely separate CNCF TAG-Network or LF AI & Data Foundation initiative on AI-gateway standardization that simply doesn't surface in public blog search — i.e., is Finding 5's "no CNCF-distinct effort found" a real absence or a search-depth limitation? Would need direct outreach to CNCF TAG-Network, not further public-web research, to fully close.
