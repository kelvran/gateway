- **Status**: accepted
- **Date**: 2026-09-03
- **Author(s)**: project founder + Claude Code

## Summary

Build Kelvran's Guardrails subsystem v1: pre-call and post-call content middleware, exactly where `gateway/ARCHITECTURE.md`'s Request Lifecycle already documents it (lines 101/106), implementing exactly what that same document already commits to at v1 (line 136) — **narrowed by this RFC's own grounding research the same way Cache L3-lite narrowed "real embeddings" to MinHash**: a pure-Go, stdlib-only regex/checksum detector suite for structured PII/secrets plus a prompt-injection heuristic, **never** free-text NER and **never** an ML/third-party moderation model in this pass. This closes a real, currently-live documentation-vs-code integrity gap: `THREAT_MODEL.md` and `SECURITY.md` already assert this mitigation exists (for Information Disclosure, Elevation of Privilege, and OWASP LLM01/LLM02) when zero guardrail code exists anywhere in the repo today.

## Motivation

Confirmed directly, not assumed: `grep -rni guardrail gateway/ --include="*.go"` returns exactly two hits, both comments (`dataplane.go`'s own package doc, `cmd/gateway/main.go`'s header) — no type, function, or package. `gateway/ARCHITECTURE.md:134-136` already reads: *"Ships with a basic PII/NER + regex classifier at v1; pluggable to call out to a third-party moderation model later. Fail-closed for regulated content categories, fail-open (with logging) for low-stakes ones."* `THREAT_MODEL.md:14,16` names guardrails as the stated mitigation for two real STRIDE threats (Information Disclosure, Elevation of Privilege); the OWASP crosswalk (`THREAT_MODEL.md:51-52`) claims LLM01/LLM02 coverage the same way. None of this is true in the live codebase today.

This is genuinely ready to build now, unlike every other open candidate at the time this was picked: the evals skeptic-panel is explicitly `v2`-scoped in `PRD.md`; a real embedding-based semantic L3 and Cache L2's deferred whitespace-collapse/case-folding normalizations are both explicitly gated on real production telemetry that doesn't exist for a zero-traffic project (`DECISIONS.md`'s most recent entries). Guardrails is named in neither bucket — `PRD.md` is simply silent on it by name (confirmed via `grep -i guardrail PRD.md` → zero matches), and `PRD.md`'s own header states it is a one-time dated artifact, deferring to `ARCHITECTURE.md` for current state. `DECISIONS.md`'s only guardrail mention (line 18) groups it with streaming, distributed rate limiting, and Decimal cost accounting as "explicitly deferred, not silently dropped" during initial scaffolding — all three of those have since shipped as real v1 work. Guardrails is the one remaining item from that original bucket, sitting unbuilt with a fully-specified v1 design already committed to prose, no infrastructure dependency, no founder-pick gate beyond being chosen from the remaining pool (which this pass does), and no telemetry gate.

## Detailed Design

### Grounding

Grounded via a dynamic-workflow research pass (5 parallel angles — pure-Go PII/content-classification feasibility, fail-open/fail-closed policy precedent, pipeline integration including the streaming tension, a full line-numbered read of the current codebase, and a scope-readiness check against `PRD.md`/`THREAT_MODEL.md`/`DECISIONS.md` — plus a synthesis). Every load-bearing claim the synthesis relied on was independently re-verified directly against the live tree before being written into this RFC: the "zero guardrail code" claim, the exact `ARCHITECTURE.md`/`THREAT_MODEL.md`/`SECURITY.md`/`DECISIONS.md` wording, and — critically — the exact current line numbers and code shapes in `dataplane.go`/`streaming.go`, which had shifted since the research ran (this session's own prior `gatewayevents-decision-enrichment` pass edited both files minutes before this research was grounded). One real design correction was found during that re-verification and is reflected below, not inherited uncritically from the research (see "Cache-hit safety" below).

### v1 scope: two detector families, zero new `go.mod` entries

**Family 1 — structured PII/secrets (regex, checksum where the entity supports one).** Patterns lifted from gitleaks' MIT-licensed rule set and Microsoft Presidio's own published per-entity detection-method documentation (the two reference implementations for this exact problem), not reinvented:

| Detector | Method | Category |
|---|---|---|
| Email address | RFC-5322-shaped regex | `contact_info` |
| Phone number | regional regex set (NANP + generic E.164) | `contact_info` |
| US SSN | pattern regex | `government_id` |
| IBAN | pattern regex + ISO 7064 mod-97 checksum | `financial_id` |
| Credit card | pattern regex + Luhn checksum (~15 lines, no library) | `financial_id` |
| IPv4/IPv6 | pattern regex | `network_id` |
| API key / secret prefix | `sk-`, `sk-ant-`, `sk-proj-`, `ghp_/gho_/ghu_/ghs_/ghr_`, `AKIA`/`ASIA`, `sk_live_`/`pk_live_`/`sk_test_`, plus a generic `(api[_-]?key|secret|token)[\s:=]+['"]?[A-Za-z0-9_\-/+=]{20,}` gated by Shannon entropy ≥ 3.5 | `credential` |

**Family 2 — prompt-injection heuristic** (`THREAT_MODEL.md:51`'s LLM01 mapping): keyword/phrase fuzzy matching (Kong/LiteLLM-style verb×preposition combinatorics — "ignore/disregard/skip/forget" × "prior/previous/preceding instructions") plus hidden-Unicode detection (zero-width chars `U+200B`–`U+200D`/`U+FEFF`, bidi-control chars `U+202A`–`U+202E`, Unicode tag chars `U+E0020`–`U+E007F` — lifted directly from Kong's real AI Prompt Guard plugin, a genuinely cheap and genuinely effective real attack class). Category: `prompt_injection`.

**Explicitly NOT in v1 — narrowed the same way L3-lite narrowed "real embeddings" to MinHash, for the same class of reason:**

- **Free-text NER** (`PERSON`, street-level `LOCATION`, `ORGANIZATION`, `NRP`) — no mature pure-Go, no-cgo, no-model-file NER library exists today. `jdkato/prose` was archived; its 2025 fork `tsawler/prose` is weeks old, 7 stars, a pre-transformer statistical tagger. `hoophq/alcatraz` is architecturally the right split (regex core + optional ONNX module) but is 0-star, single-contributor, weeks old, and explicitly "Experimental... breaking changes before v1.0.0" — and even its "pure Go" NER path still requires downloading and running a real ONNX/GGUF model file in-process via FFI, the exact same new-model-file-artifact ops burden the Cache L3-lite RFC already rejected for real embeddings. Unlike L3-lite (which had MinHash as a legitimate full lexical substitute), there is no substitute here — NER is cleanly out of scope, not replaced by an equivalent-power alternative.
- **ML-grade toxicity/hate/sexual/violence/self-harm classification** — every real gateway surveyed (LiteLLM, Portkey, Kong) implements this as an optional call-out to a separately-deployed Presidio/vendor/model-provider moderation endpoint, never an in-process model. That call-out is a real upstream call with real cost (Azure Content Safety: metered per 1K chars), real rate limits (OpenAI moderation free tier: 250 RPM/5K RPD), and a new hard runtime dependency on a third party's uptime for every gated request on both the pre- and post-call legs — a genuine architectural decision for a later RFC, gated on the same kind of real-traffic-driven prioritization this project already applies elsewhere, not a default to reach for now.
- **CSAM detection** — not deferred, **not applicable**: v1's gateway is text-only. If/when multimodal input ships, CSAM hash-matching must be added as a separate, mandatory, always-on path per 18 U.S.C. § 2258A — it must never be folded into the general fail-open/closed classifier knob this RFC builds (Anthropic runs this outside its general classifier stack for exactly this reason).
- **CBRN/weapons-uplift classification** — same ML-classifier gap as toxicity; out of scope.

**Doc correction bundled with this RFC**: `gateway/ARCHITECTURE.md:136`'s current text — *"Ships with a basic PII/NER + regex classifier at v1"* — overclaims even after this RFC ships, since v1 ships no NER. Corrected to name what's actually real (see Implementation).

### Fail-open / fail-closed policy — category-tiered on both axes

Never a single global default, and **never inherited from the rate limiter's blanket fail-open policy** — the rate limiter's fail-open is safety-netted by `budget.Tracker` as an independent second control (`docs/rfcs/2026-09-03-distributed-rate-limiting.md`); guardrails has no equivalent second control, so it doesn't get the same default. This is OWASP's own "Fail Securely" guidance, applied deliberately rather than by habit: *"Any security mechanism should be designed... so that when it fails, it fails closed."*

| Category | Detectors | On detection | On detector error |
|---|---|---|---|
| `credential` | API-key/secret prefix + entropy | **Block** | **Block** |
| `financial_id` | Credit card (Luhn), IBAN (mod-97) | **Block** | **Block** |
| `government_id` | US SSN | **Block** | **Block** |
| `contact_info` | Email, phone | **Warn** (log + redact-in-audit-record, forward) | **Warn** |
| `network_id` | IP address | **Warn** | **Warn** |
| `prompt_injection` | Keyword + hidden-Unicode heuristic | **Warn** | **Warn** |

Block tier = categories with real legal/liability teeth (PCI DSS PAN handling, credential exposure) *and* high-precision detectors (checksum-validated, so false positives are rare — matches AWS Bedrock's own published guidance: *"Configure MASK for most sensitive data types... BLOCK for the most sensitive categories such as credentials... where masking isn't enough"*). Warn tier = real but lower-stakes exposure with lower-precision detectors (bare regex, no checksum, or a fuzzy heuristic with a foreseeable false-positive rate on ordinary conversation) — hard-blocking here would trade a marginal security benefit for a real UX cost, matching Azure Content Safety's own default (low-severity hits annotate, not block).

**A subtlety stated plainly, not glossed over**: every real "classifier error" precedent surveyed (LiteLLM, NeMo, Portkey, Azure) assumes a detector that makes a network call or invokes a model — something that can time out or 5xx. v1's detectors are pure `regexp`/arithmetic with no I/O; Go's RE2 does not error at match time. So the "detector error" column above is close to unreachable in practice for v1 specifically — it exists in the `Detector` interface and `Policy.ErrorActions` map now *because* `ARCHITECTURE.md:136` already commits to a future pluggable third-party-moderation detector that genuinely can error, and building the error-handling contract on the interface now means it doesn't need retrofitting later.

### Pipeline integration

**Pre-call: after cache lookup (L1→L2→L3 all miss), before the router.** Matches `gateway/ARCHITECTURE.md:96-102`'s already-documented lifecycle exactly — no diagram change needed.

- `dataplane.go`, `HandleChatCompletion`: insert between the end of the L3 lexical-cache-check block and `var found bool` / `p.nextDeployment(...)`.
- `streaming.go`, `HandleChatCompletionStream`: symmetric insertion, after the L3 check block, before `var found bool` / `p.nextDeployment(...)`. Verified directly: `streaming.NewWriter(w)` (constructed earlier) sets no headers and writes no bytes on its own — a pre-call rejection here can still return a clean, non-SSE JSON error exactly like the existing auth/rate-limit/budget rejections, provided the rejection path never calls any `sw.Write*` method.
- Rejection uses a new sentinel, `ErrGuardrailBlocked`, following the existing `ErrModelNotAllowed`/`ErrRateLimited`/`ErrBudgetExceeded` convention.

**Cache-hit safety — corrected from the grounding research's own proposal.** The research proposed storing a `guardrail_policy_version` "alongside the existing `ModelID` field" for all three cache layers. Checked directly against the real `cache.Cache` interface (`internal/cache/port.go`) before accepting that: **L1/L2 have no metadata envelope at all** — `Get`/`Put` operate on raw `[]byte`, and tenant/model isolation is baked into the cache *key* itself (`cache.Key`/`cache.NormalizedKey`'s SHA-256 hash inputs), never a stored+checked field. Only L3 (`LexicalCache`), because it does a similarity search rather than an exact key lookup, has an explicit provenance struct (`LexicalCandidate.ModelID`) to check against. This RFC therefore uses two different, layer-appropriate mechanisms instead of one uniform (and, for L1/L2, non-existent) mechanism:

- **L1/L2**: `cache.Key`/`cache.NormalizedKey` gain a new `guardrailPolicyVersion string` parameter, hashed in as one more `\x00`-tagged field alongside the existing `tenant`/`model`/`messages`/`temperature`/`max_tokens` fields. A policy-version bump automatically and implicitly invalidates every existing L1/L2 entry — the existing key-equality check *is* the version check, with zero new stored fields, zero new interface methods, and zero new "is this hit still valid" code. This is more consistent with how L1/L2 already achieve tenant/model isolation than inventing a new mechanism would be.
- **L3**: `LexicalCandidate` and `LexicalCache.Put` gain a `GuardrailPolicyVersion string` field/parameter, checked in `checkLexicalCache` as a new, separate inline check (`if c.GuardrailPolicyVersion != currentPolicyVersion { continue }`) — **not** folded into `freshnessRiskModel`, which stays scoped to Cache L3-lite's own documented checklist (staleness/similarity/model-match); this mirrors how `fingerprintsEqual` is already a separate check called alongside `freshnessRiskModel`, not merged into it.

`guardrailPolicyVersion` itself is a plain Go constant in v1 (e.g. `const guardrailPolicyVersion = "v1"`), bumped by hand whenever a detector or policy changes and a new binary is released — no operator-facing versioning UI is needed or built in this pass.

**Post-call: buffered = enforcement-capable; streaming = audit-only. Decisive, not an open menu.**

- **Buffered** (`dataplane.go`, `HandleChatCompletion`): insert between the unified upstream-error check closing and cache write-back start. `resp` is guaranteed fully populated here (primary or fallback succeeded), and nothing downstream has fired — a `Block` verdict can still refuse to write cache and return an error before any bytes reach the client.
- **Streaming** (`streaming.go`, `streamDeployment`): insert between `resp := acc.build(usage)` and `sw.WriteDone()`, running the check against the fully-reassembled response — **audit-only**. Verified directly in code: every chunk is written and flushed to the client (`sw.WriteChunk(c)`, calling `flusher.Flush()` synchronously) *inside* the decode loop, strictly before the full response exists. There is no point in the current, real `streamDeployment` where a post-call check can run before content reaches the client without restructuring the zero-buffer write loop the streaming RFC was explicitly built to keep non-buffering — that restructuring (NeMo's `stream_first: false` / LiteLLM's buffering-iterator-hook pattern) is real, named, and **out of scope for this RFC**, recorded in `DECISIONS.md` as deferred, not silently dropped.

**Accepted, named residual risk** (stated in this RFC's own scope, not smoothed over, matching how the streaming RFC named its own mid-stream-fallback tradeoff): on the streaming path, model-generated output that itself matches a `Block`-tier pattern cannot be withheld before delivery in v1 — it is caught only after the fact, logged at elevated severity with a distinct, alertable log field, matching LiteLLM's "critical"-level fail-open logging convention. Pre-call already fully screens the input side regardless of streaming/buffered (the complete request text is known before any upstream call, so there's no half-formed-content problem there). Regulated/`Block`-tier categories therefore have real enforcement on: (a) input, always; (b) output, buffered only.

### Go-level design

New package `gateway/internal/guardrail` (matches `ARCHITECTURE.md:65`'s already-documented path):

```go
package guardrail

type Category string
const (
    CategoryCredential      Category = "credential"       // Block
    CategoryFinancialID     Category = "financial_id"      // Block
    CategoryGovernmentID    Category = "government_id"     // Block
    CategoryContactInfo     Category = "contact_info"      // Warn
    CategoryNetworkID       Category = "network_id"        // Warn
    CategoryPromptInjection Category = "prompt_injection"   // Warn
)

type Action int
const ( ActionWarn Action = iota; ActionBlock )

// Policy maps each Category to its Action on detection AND on Detector error.
type Policy struct {
    Actions      map[Category]Action
    ErrorActions map[Category]Action
}

type Finding struct { Category Category; Detector string; Start, End int }

// Detector is intentionally I/O-agnostic: v1 implementations are pure
// regexp/stdlib, but the interface must not assume Detect() can't fail —
// ARCHITECTURE.md:136 already commits to a future third-party-moderation
// Detector that genuinely can error over the network.
type Detector interface {
    Name() string
    Category() Category
    Detect(ctx context.Context, text string) ([]Finding, error)
}

type Verdict struct {
    Blocked       bool
    Findings      []Finding
    DetectorError error // non-nil only when a Detector itself failed
}

type Engine struct { /* detectors []Detector; policy Policy; version string; logger *slog.Logger */ }

func NewEngine(detectors []Detector, policy Policy, version string, logger *slog.Logger) *Engine
func (e *Engine) Version() string
// Check is the single entry point — called identically for pre-call (on
// serializeMessages(req.Messages)) and post-call (on the response's
// serialized text). Engine has no concept of "pre/post" or
// "streaming/buffered" — that distinction, and how a Verdict gets
// enforced, lives in dataplane, not here.
func (e *Engine) Check(ctx context.Context, text string) Verdict
```

Files (respecting the "many small files" style rule): `engine.go`, `policy.go`, `types.go`, plus one file per detector — `email.go`, `phone.go`, `ssn.go`, `iban.go`, `creditcard.go`, `ipaddress.go`, `secretkey.go`, `promptinjection.go`.

**Dependency direction**: `guardrail` must never import `adapter`, `cache`, or any provider-specific package — text in, `Verdict` out. Add this to `ARCHITECTURE.md`'s Dependency direction rules block, which currently doesn't mention `guardrail` at all — a real gap this RFC closes.

**`dataplane.Config`**: mirror the `CacheL2`/`CacheL3`/`Limiter`/`Budget` pattern — **hard-required**, deliberately *not* the `UpstreamStream` pattern (deliberately optional, whose own doc comment says its optionality exists purely so pre-existing non-streaming tests don't need updating — a backward-compat concern that doesn't apply to a brand-new subsystem). `THREAT_MODEL.md`/`SECURITY.md` already classify guardrail bypass as a named severity item (P4); OWASP's fail-securely consensus treats security controls as always-on, not rollout-optional. A config-level "disable" is expressed as *zero detectors registered*, never `Config.Guardrails == nil` — this preserves the hard-dependency invariant while still letting an operator turn off enforcement via YAML if genuinely needed (e.g. staging).

```go
// Config additions:
Guardrails *guardrail.Engine // Required, like every other dependency here.
// NewPipeline validation switch, new case:
case cfg.Guardrails == nil:
    return nil, fmt.Errorf("dataplane: Config.Guardrails is required")
// Pipeline struct: guardrails *guardrail.Engine
// &Pipeline{...} literal: guardrails: cfg.Guardrails,
```

**`controlplane/config.go`** — the exact three-part optional-nested-section pattern already used for `cache`/`budget`/`rate_limit`:

```go
type GuardrailsConfig struct {
    PolicyVersion     string            // stamped into every cache write; bumped on any detector/policy change
    CategoryOverrides map[string]string // Category -> "block"/"warn", operator override of this RFC's default policy
}
// on Config: Guardrails GuardrailsConfig // Optional — zero value runs all v1 default detectors under this RFC's default policy.
// in Load(), alongside the existing budget/rate_limit/cache blocks:
if guardrailsRaw, ok := getMap(root, "guardrails"); ok {
    cfg.Guardrails.PolicyVersion, _ = getString(guardrailsRaw, "policy_version")
    if overridesRaw, ok := getMap(guardrailsRaw, "category_overrides"); ok { /* per-key getString */ }
}
```

**`gatewayevents_v1` consistency** — a gap neither the research nor its synthesis named, caught independently by cross-checking against the already-established `outcomeFor`/`Outcome` enum convention: a new `ErrGuardrailBlocked` sentinel, if left unclassified, would fall into `outcomeFor`'s `default: OUTCOME_UPSTREAM_ERROR` case — misclassifying a guardrail rejection as an upstream failure. This RFC adds a new, additive `OUTCOME_GUARDRAIL_BLOCKED` value to `GatewayDecisionEvent.Outcome` (per `docs/rfcs/2026-09-03-api-gatewayevents-contract.md`'s existing enum, and `docs/rfcs/2026-09-03-gatewayevents-decision-enrichment.md`'s precedent for extending it) and wires `outcomeFor` to classify `ErrGuardrailBlocked` correctly via `errors.Is`.

## Drawbacks

- No real-time streaming enforcement — a `Block`-tier pattern in model output is caught only after full delivery on the streaming path (named residual risk above).
- No free-text NER, no ML-grade content moderation — v1 catches structured PII/secrets and a coarse prompt-injection heuristic only; genuine name/address/toxicity detection needs either a later RFC's ONNX/GGML in-process model or an upstream moderation API call, both real architectural decisions deferred, not solved here.
- The detector-error fail-open/closed policy is built on an interface that v1's own pure-regex detectors can't practically exercise — genuinely untested in production until a real erroring detector (the future pluggable model) exists. Stated plainly as an acknowledged gap, not hidden.
- `cache.Key`/`cache.NormalizedKey`'s signature change (new `guardrailPolicyVersion` parameter) touches every existing call site — small blast radius (confirmed: exactly 2 non-test call sites), but a real signature change nonetheless.

## Alternatives Considered

1. **Full NER/ML classification in v1** — rejected; no mature pure-Go, no-cgo, no-model-file NER library exists today, and even the nearest candidate (`hoophq/alcatraz`) still needs a real ONNX/GGUF model file in-process, the same ops burden already rejected for Cache L3-lite's real embeddings.
2. **A call-out to a third-party moderation API (OpenAI Moderation, Azure Content Safety) for v1** — rejected: real cost, real rate limits, a new hard runtime dependency on a third party's uptime for every gated request on both pre- and post-call legs, and it would be an upstream call inside the pre-call stage specifically, which sits *before* the pipeline's own real upstream call — a materially different, larger architectural decision than this RFC's scope.
3. **Store a version field alongside every cache write for all three layers uniformly** (the research's own initial proposal) — rejected after checking the real `cache.Cache` interface: L1/L2 have no metadata envelope to attach such a field to at all. Corrected to bake the policy version into the L1/L2 cache key hash instead, and use an explicit field only for L3, where the existing `ModelID`/`LexicalCandidate` mechanism already does exactly this for a different provenance dimension.
4. **Fold the guardrail-policy-version check into `freshnessRiskModel`** — rejected: that function is explicitly scoped to Cache L3-lite's own RFC checklist (staleness/similarity/model-match); mixing an unrelated RFC's concern into it breaks single-responsibility, and the existing `fingerprintsEqual`-as-a-separate-check precedent already shows the right shape for adding a new, independent gate.
5. **Real-time mid-stream enforcement (buffering/windowing before delivery)** — rejected for this pass: requires restructuring the exact zero-buffer write loop `streamDeployment` was built to keep non-buffering; a genuine future RFC, not a default reach for v1.
6. **A single global fail-open/fail-closed default with per-category override** (mirroring the rate limiter's own default) — rejected: guardrails has no independent second control the way `budget.Tracker` backstops the rate limiter's fail-open default; category-tiering on both the detection and error axes is the correct default, not an override of one.

## Unresolved Questions

- Real false-positive/false-negative rates for every detector in this RFC — no production traffic exists yet to measure them, same caveat this project has applied to every heuristic shipped so far (L3-lite's similarity floor, L2's normalization allowlist).
- Whether/when a real ML-grade moderation tier (in-process ONNX/GGML or an upstream API call-out) is worth building — deliberately not decided here, the same "gated on real data, not a fixed timeline" posture already applied to a real embedding-based L3.
- Whether real-time mid-stream enforcement is ever worth the write-loop restructuring it requires — deferred, named in `DECISIONS.md`, not decided against permanently.
- Whether `PRD.md`'s silence on guardrails (neither in- nor out-of-scope by name) is worth reconciling explicitly in that document — a real, minor gap noted by this RFC's own research; does not gate this RFC, since `ARCHITECTURE.md` is the documented authoritative current-state source, but worth a founder's attention in passing.

## Research Trail

Grounded via a dynamic-workflow research pass (5 parallel angles: pure-Go PII/content-classification feasibility — cited against Microsoft Presidio's and AWS Comprehend's own per-entity detection-method documentation, gitleaks'/trufflehog's real regex rule sets, and a realistic check of the only two candidate pure-Go NER libraries; fail-open/fail-closed policy precedent — cited against OWASP's "Fail Securely" guidance, AWS Bedrock/Azure Content Safety/OpenAI Moderation's own published category-severity defaults, and LiteLLM/NeMo/Portkey/Azure OpenAI's real, documented classifier-error handling; pipeline integration including the streaming tension — cited against OpenAI's own streaming-moderation documentation and LiteLLM/Portkey/NeMo's real streaming-guardrail implementations; a full line-numbered read of the current `dataplane.go`/`streaming.go`/`controlplane/config.go`; and a scope-readiness check against `PRD.md`/`THREAT_MODEL.md`/`DECISIONS.md` — plus a synthesis). Every load-bearing claim was independently re-verified directly against the live repo before being trusted, including re-confirming the "zero guardrail code" claim, the exact doc wording in four separate files, and — because this session's own prior work had just edited the two files this RFC touches most — re-reading their current, post-edit line numbers and structure rather than trusting the research's now-stale citations. One real design correction was found and applied during that re-verification: the research's proposed uniform "store a version field on every cache write" mechanism doesn't fit L1/L2's real interface (no metadata envelope exists there at all), corrected to a cache-key-hash-based mechanism for those two layers specifically. A second gap — that a new sentinel error needs a corresponding `Outcome` enum value or it silently misclassifies — was caught independently by cross-referencing this RFC's design against the established convention from the two prior `gatewayevents` RFCs, not named by the research itself.
