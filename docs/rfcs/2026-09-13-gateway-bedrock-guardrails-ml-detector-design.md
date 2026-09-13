# RFC: AWS Bedrock Guardrails as an optional ML detector backend — design only, no code

## Status

Design-only. `gateway/ARCHITECTURE.md`'s Guardrails Subsystem section already states this exact
capability is "pluggable to call out to a third-party moderation model later, once that
architectural decision gets its own RFC." This is that RFC. No code accompanies it —
`internal/guardrail` remains, today, exactly what its own package doc says: "Pure-Go,
stdlib-only... never free-text NER and never an ML/third-party moderation model."

## Date

2026-09-13

## Summary

`gateway/internal/guardrail/promptinjection.go` is a keyword/hidden-Unicode heuristic — real,
shipped, zero marginal cost, but a regex-class detector with a regex-class accuracy ceiling.
AWS Bedrock Guardrails' `PROMPT_ATTACK` filter is a managed, model-agnostic ML classifier
reachable through an AWS integration Kelvran already operates in production (the live Bedrock
pilot, per `DECISIONS.md`'s `[2026-09-13]` entries) — the lowest-activation-cost ML option found
across a 2026 survey of Llama Guard 4, Prompt Guard 2, Anthropic's Constitutional Classifiers, and
Azure AI Content Safety (`docs/upgrade-research/gateway-ml-guardrails-2026-09-13.md`), all of which
either require self-hosting a GPU-bound model or aren't offered as an external product at all. This
RFC designs a new, optional `guardrail.Detector` implementation that calls Bedrock's Guardrails
service directly (not via the existing chat-completion `Converse` adapter), living in a new
sibling package specifically so `internal/guardrail`'s own core package keeps its explicit,
already-stated "never imports adapter, cache, or any provider-specific package" boundary intact.
Nothing here is implemented — this is a design sketch for a future implementation pass, following
this same session's own precedent for guardrail-adjacent changes (`docs/rfcs/2026-09-12-...
reasoning-content...md`'s guardrail-scanning updates) of scoping the design before touching this
security-sensitive package.

## Motivation

`docs/upgrade-research/gateway-ml-guardrails-2026-09-13.md` (this session's own research, verified
against live AWS/Meta/Anthropic/Azure/OWASP primary sources) found: Meta's Prompt Guard 2
demonstrates a real, measured accuracy ceiling above pure regex (Recall@1%FPR jumping from 21.2%
to 97.5%), but ships only as self-hosted open weights requiring GPU inference — a materially
bigger architectural commitment than wrapping an API Kelvran already calls. Bedrock Guardrails is
the one option that is genuinely low-activation-cost: managed, API-configurable, model-agnostic,
metered at $0.08/1,000 text units for the `PROMPT_ATTACK` filter specifically (cheaper than the
$0.15/1,000 general content-filter SKU), reachable via the exact AWS account/credentials the pilot
already uses. `THREAT_MODEL.md`'s Guardrails-adjacent rows and OWASP's own 2026 Top 10 agree: no ML
classifier is a complete, standalone defense — every vendor frames theirs as a layer, never a
replacement. This RFC's proposal is additive for exactly that reason: `promptinjection.go` stays
in place, unconditionally, as the free, zero-latency first line; Bedrock Guardrails would be an
optional second opinion, opt-in per deployment.

## Detailed Design

### Why this cannot live inside `internal/guardrail` itself

`internal/guardrail`'s own package doc (`types.go:1-9`) is explicit: "guardrail never imports
adapter, cache, or any provider-specific package — text in, Verdict out." A Bedrock Guardrails call
is unavoidably provider-specific (it is a live AWS API call, with its own AWS SDK client, its own
credentials, its own error modes). Importing that into `internal/guardrail` directly would violate
a boundary this package states about itself, not an incidental convention. The `Detector` interface
is already deliberately shaped to allow this without a boundary violation, per its own doc comment:
"the interface must not assume `Detect` can't fail" and "`ARCHITECTURE.md`'s Guardrails Subsystem
section already commits to a future third-party-moderation Detector that genuinely can error over
the network." The interface was built for exactly this; the implementation must not live in the
same package as the interface's pure-Go siblings.

**Proposed location:** a new sibling package, `gateway/internal/guardrail/bedrockguard`, containing
a `Detector` type implementing `guardrail.Detector` (`Name()`, `Category()`, `Detect(ctx, text)`).
This mirrors `internal/cache/grpcserver`/`grpcclient`'s own precedent — a provider/transport-
specific implementation living adjacent to, never inside, the core package whose interface it
satisfies. `internal/guardrail` itself needs zero changes to its own source — `DefaultDetectors()`
stays exactly as-is (the always-on, zero-config set); a new, separate constructor path (wiring code
in `dataplane` or `main.go`, not `guardrail` itself) would append `bedrockguard.Detector` to the
engine's detector slice only when a deployment opts in.

### Which Bedrock API, and why not the `Converse` call's own `guardrailConfiguration`

`gateway/internal/adapter/bedrock/bedrock.go`'s own doc comment confirms Kelvran's Bedrock adapter
targets the **Converse API** exclusively (never `InvokeModel`/`InvokeModelWithResponseStream`).
Converse has its own native `guardrailConfiguration` mechanism for attaching a guardrail to a
model-invocation call — but that mechanism is **Bedrock-Converse-specific**: it would do nothing
for a request routed to OpenAI, Anthropic, or Gemini, defeating the entire point of
`internal/guardrail` being a provider-agnostic layer that runs identically regardless of which
upstream adapter ultimately serves the request (`engine.go`'s own doc comment: "one instance
shared across every pre-call/post-call check... buffered and streaming alike"). The correct shape
is therefore to call Bedrock's **standalone Guardrails check** as its own, independent AWS API
call — decoupled from which model/provider will actually generate the completion — the same way
every other `Detector` in this engine runs unconditionally regardless of upstream routing.

This also resolves the research's own flagged open question about AWS's input-tagging requirement:
that gotcha is specifically documented for `InvokeModel`/`InvokeModelWithResponseStream` callers
who attach a guardrail at model-invocation time. Since this design never attaches Bedrock
Guardrails to a model-invocation call at all — it calls the guardrails check directly and
independently — the tagging requirement should not apply. **This inference is not independently
verified against AWS's live API reference for the standalone/decoupled call shape specifically**
(the research pass verified it for the `InvokeModel`-attached shape only) — a future implementer
must confirm the exact standalone operation name and request shape against AWS's current Bedrock
Runtime API reference before writing code, not trust this RFC's naming.

### Detector shape

```go
package bedrockguard

// Detector calls AWS Bedrock's standalone Guardrails check as an
// optional, ML-classifier second opinion behind
// promptinjection.go's own zero-latency regex heuristic. Never a
// replacement -- DefaultDetectors() is unconditionally unaffected;
// this is appended only when a deployment opts in.
type Detector struct {
    client       *bedrockruntimeClient // exact SDK type TBD -- see Unresolved Questions
    guardrailID  string
    guardrailVer string
    // FailOpen controls what Detect returns when the AWS call itself
    // fails (network error, throttling, credential error -- NOT a
    // successful call that found nothing). See "Fail-open vs.
    // fail-closed" below for why this defaults to true, matching
    // prompt_injection's already-established Warn-tier ErrorActions
    // convention rather than inventing a new default.
    FailOpen bool
}

func (d *Detector) Name() string { return "bedrock_guardrails_prompt_attack" }
func (d *Detector) Category() guardrail.Category { return guardrail.CategoryPromptInjection }
func (d *Detector) Detect(ctx context.Context, text string) ([]guardrail.Finding, error) {
    // Calls Bedrock's standalone guardrails-check operation (exact
    // name TBD) with d.guardrailID/d.guardrailVer, text as the
    // content to check. A BLOCKED verdict from Bedrock maps to one
    // guardrail.Finding{Category: CategoryPromptInjection, Detector:
    // d.Name()}; a clean verdict maps to nil findings. An AWS-call
    // failure returns a non-nil error -- Engine.Check's existing
    // ErrorActions[CategoryPromptInjection] policy then decides
    // Blocked, exactly like every other detector's error path
    // already works; this Detector does not need its own separate
    // fail-open logic beyond what Engine already provides, UNLESS
    // the desired policy needs to differ per-detector rather than
    // per-category -- see Unresolved Questions.
}
```

Reusing `Engine.Check`'s existing `ErrorActions` map (keyed by `Category`, not by individual
detector) means Bedrock Guardrails' own error behavior is governed by whatever policy
`prompt_injection` already has configured — currently Warn-tier (fail-open-with-logging) per
`ARCHITECTURE.md`'s Guardrails Subsystem section ("Category-tiered fail-closed... vs.
fail-open-with-logging (contact_info/network_id/prompt_injection)"). This is almost certainly the
right default to inherit rather than override: an AWS outage or throttling event should not itself
become a new denial-of-service vector against Kelvran's own gateway, and `promptinjection.go`'s
regex detector keeps running unconditionally in the same category regardless — a Bedrock-call
failure degrades Kelvran back to regex-only coverage for that one request, not to zero coverage.

### Config wiring

`gateway/internal/gateway/controlplane/config.go`'s existing `GuardrailsConfig` struct (already
real, already optional, already YAML-driven via the `guardrails:` root key) is the natural
extension point — mirroring its own existing `PolicyVersion`/`CategoryOverrides` fields' shape:

```go
type GuardrailsConfig struct {
    PolicyVersion     string
    CategoryOverrides map[string]string
    // BedrockGuardrails, when non-nil, opts this deployment into the
    // AWS Bedrock Guardrails PROMPT_ATTACK detector as an additional
    // (never a replacement for promptinjection.go) backend. Absent =
    // today's exact behavior, byte-for-byte -- matching this file's
    // own "optional, zero-valued struct reproduces v1 default"
    // convention for every other field here.
    BedrockGuardrails *BedrockGuardrailsConfig
}

type BedrockGuardrailsConfig struct {
    GuardrailID      string
    GuardrailVersion string
    // FailOpen, default true -- see Detector's own doc comment above
    // for why this should not need to differ from the existing
    // prompt_injection category's own ErrorActions policy in the
    // common case.
}
```

A deployment that never sets `guardrails.bedrock_guardrails` in its YAML gets exactly today's
regex-only behavior — the same "additive, opt-in, zero-value-reproduces-current-behavior"
convention this file already uses for every other field.

### AWS credentials and IAM — a real, disclosed gap this RFC does not resolve

The pilot's real AWS identity (`AAVA_Bedrock_Non_Prod`, confirmed this session via a real
`sts:GetCallerIdentity` call) is scoped to Bedrock model-invocation only — `s3:ListBuckets` and
several other unrelated actions were confirmed denied via real `AccessDenied` responses earlier
this session. Whether that same credential's policy already grants whatever IAM action the
standalone Guardrails-check operation requires (likely `bedrock:ApplyGuardrail` or similarly named
— unconfirmed, see Unresolved Questions) is **not verified by this RFC**. A future implementer must
check this before assuming the existing pilot credential "just works" — Guardrails is a distinct
IAM action from `bedrock:InvokeModel`/`bedrock:Converse`, and AWS IAM policies scope by action, not
just by service.

### Cost and latency — real, disclosed, not hidden in the design

Every Bedrock-Guardrails-enabled request pays: (1) a real network round-trip to AWS on top of the
existing upstream provider call (parallelizable with the pre-call guardrail's own regex pass and
with the actual chat-completion call itself, per Finding 1 of the research — "policy checks run in
parallel per configured policy specifically to minimize added latency" — but this is AWS's own
parallelism across its filters, not a guarantee about Kelvran's own call-ordering, which a future
implementer must design explicitly, e.g. `errgroup`-style concurrent dispatch alongside the
existing regex detectors rather than sequential); (2) a real, metered AWS cost ($0.08/1,000 text
units) on top of the completion's own token cost — small per-call, but a genuine new line item an
operator must be able to see, most naturally via this same session's own recently-shipped
cost-attribution machinery (`agent_run_id`/`cost_usd` on `GatewayDecisionEvent`) rather than a
wholly separate, untracked cost.

## Drawbacks

- **A new external dependency in the guardrail hot path.** Every other detector in
  `DefaultDetectors()` is pure-Go, zero-network, effectively free. This one is not — an AWS outage,
  throttling, or credential misconfiguration becomes a new failure mode for a request path that
  previously had zero external dependencies at all (beyond the upstream LLM call itself, which
  already tolerates failure via the existing fallback-chain machinery).
- **Real, ongoing metered cost**, however small per-call, on every enabled deployment — a cost this
  RFC does not propose absorbing invisibly; see the cost-attribution note above.
- **A second, distinct IAM permission surface** to manage on top of the existing Bedrock-invoke
  scoping — operationally, this means the credential rotation/scoping discipline already
  established for the pilot (`DECISIONS.md`'s AWS-key-rotation deferral) now has one more action to
  track correctly, not zero.
- **Package-boundary discipline must be actively maintained, not just declared once.** A future
  contributor unfamiliar with `types.go`'s own stated boundary could plausibly try to add this
  detector directly inside `internal/guardrail` "for convenience" — this RFC's own chosen shape
  (a sibling package) only holds if code review/`go-arch-lint` actively catches a violation, which
  is worth confirming is enforceable (see Unresolved Questions).

## Alternatives Considered

- **Azure AI Content Safety's Prompt Shields.** Rejected for the same reason the research report
  itself gives: it is a net-new cloud dependency (a second cloud provider account, credentials, and
  operational surface) Kelvran does not have today, unlike Bedrock. Directly analogous coverage to
  Bedrock's `PROMPT_ATTACK` filter, but at strictly higher activation cost for this specific
  gateway's current footprint.
- **Self-hosting Meta's Llama Guard 4 or Prompt Guard 2.** Rejected: both ship only as open weights
  requiring GPU-bound inference (confirmed via live model-card/HuggingFace sources in the
  research), a materially bigger commitment (new inference infrastructure, a model-serving
  lifecycle, GPU capacity planning) than wrapping an API call. Prompt Guard 2's real accuracy
  numbers are compelling, but self-hosting-only availability is the deciding factor, not accuracy.
- **Anthropic's Constitutional Classifiers.** Not evaluable as an alternative at all — confirmed
  internal-only, with no external API surface for Kelvran to call.
- **Bedrock's broader content-filter/denied-topics SKU** (the $0.15/1,000-unit tier, distinct from
  `PROMPT_ATTACK`). Explicitly out of scope for this RFC — `THREAT_MODEL.md` already states general
  content moderation is "a large, separately-scoped effort, not a natural extension" of this
  package's current PII/secrets/prompt-injection scope. This RFC's proposal stays narrowly scoped
  to the `PROMPT_ATTACK` filter specifically, mirroring what `promptinjection.go` already does.
- **Do nothing — keep `promptinjection.go`'s regex heuristic as the only defense.** Rejected: a
  real, externally-verified accuracy gap exists between regex-class detection and a modern ML
  classifier (Prompt Guard 2's Recall@1%FPR figure), and the lowest-activation-cost path to closing
  some of that gap is genuinely available today at low incremental cost — declining to even design
  it would leave a validated, cheap improvement permanently on the table for no stated reason.

## Unresolved Questions

- **Exact AWS API operation/request shape.** This RFC names the capability (Bedrock Guardrails'
  standalone check, decoupled from model invocation) but does not commit to a specific SDK method
  signature — the research's own citation of "`InvokeGuardrailChecks`" was not independently
  re-verified against AWS's current, live Bedrock Runtime API reference during this design pass.
  A future implementer must confirm the real operation name, request/response shape, and whether
  it lives in `bedrock-runtime` or a sibling Bedrock service client before writing any code.
- **Does the input-tagging requirement genuinely not apply to the standalone/decoupled call
  shape?** This RFC infers it does not (see Detailed Design), reasoning from the fact that tagging
  is documented specifically for `InvokeModel`-attached guardrails — but this inference was not
  independently confirmed against AWS's own docs for the standalone call path specifically.
- **Does the pilot's existing AWS credential already have whatever IAM action this requires**, or
  does this need a policy change on an account this project has already flagged (per
  `DECISIONS.md`) as sensitive to touch carelessly? Not resolved here.
- **Should `FailOpen` be configurable per-deployment, or should it always inherit
  `ErrorActions[CategoryPromptInjection]`** exactly as sketched above? The sketch above proposes
  the latter (simpler, no new failure-policy surface) but a future implementer should confirm no
  deployment genuinely needs Bedrock-Guardrails-specific error handling distinct from the category
  as a whole before committing to that simplification.
- **Call-ordering/concurrency**: should the Bedrock Guardrails call run concurrently with
  `Engine.Check`'s existing sequential detector loop (via `errgroup` or similar), or sequentially
  after the free regex detectors as today's loop already does? Sequential is simpler and matches
  today's `Engine.Check` structure exactly; concurrent adds real latency savings but a real
  implementation-complexity cost (partial-result handling, a detector that can outlive the others).
  Not decided here — flagged as the first real implementation-time design choice once this RFC is
  picked up.
- **`go-arch-lint` enforcement**: does the existing `.go-arch-lint.yml` config already forbid
  `internal/guardrail` from importing AWS SDK packages, or would this RFC's own proposed boundary
  need a new lint rule added at implementation time to make the "never imports provider-specific
  packages" claim enforced, not just documented?

## Verification

None — this RFC is design-only, per its own Status line. No code changes accompany it. A future
implementation pass would need, at minimum: the new `bedrockguard` package plus a real AWS-call
integration test (gated behind a real-credentials env var, mirroring this session's own
`RUN_LIVE_LLM_TESTS`-style convention used for `validate_embedding_gate.py`'s live Bedrock tests);
the `GuardrailsConfig`/`BedrockGuardrailsConfig` wiring plus a unit test proving the zero-value case
reproduces today's exact detector set; a fail-open sanity-check-by-breaking test (force an AWS-call
error, confirm the category's existing `ErrorActions` policy is honored, not silently overridden);
and its own RFC status update from "Design-only" to "Accepted, implemented" — none of which exist
today.
