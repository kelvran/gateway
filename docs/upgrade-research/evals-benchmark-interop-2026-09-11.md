# Evals Benchmark-Suite Interop — Deep Research (2026-09-11)

## Question

For `evals`' NEXT-VERSION (v2): should Kelvran adopt standard benchmark-suite
ingestion/interop (HumanEval, SWE-bench, MMLU-style formats), or stay a purely
custom-corpus regression-testing tool? Kelvran's evals is a 137+-case
hand-authored regression corpus (cache_adversarial/cost_abuse/routing_chaos/
dogfood/guardrail/judge_accuracy) purpose-built to probe the gateway/cache's
OWN correctness properties — not general LLM-capability benchmarking — with
real deterministic + LLM-judge scoring, a Wilson-lower-bound CI gate in CI,
and corpus-governance tooling. Research questions: (1) do real gateway/infra
products (LiteLLM, Portkey) offer standard-benchmark interop, and is it a
genuine feature or a checkbox nobody uses; (2) is there real demand for
running standard capability benchmarks *through* a gateway-testing harness
like Kelvran's, vs. using the benchmark's own native runner directly; (3) if
there IS a real use case ("verify my gateway doesn't degrade model quality
vs. calling the provider directly"), what's the minimum-viable interop shape,
and does it reuse Kelvran's EvalCase/Run/Score model cleanly or need a new
one; (4) is this a genuine v2 priority or a distraction from evals' actual,
validated value proposition.

## Executive Summary

No real gateway/infrastructure product — LiteLLM or Portkey — offers standard
capability-benchmark (HumanEval/SWE-bench/MMLU) interop as a customer-facing
feature; every occurrence of "benchmark" in their READMEs, docs, and product
pages refers exclusively to infra latency/throughput, and their nearest
adjacent code (a LiteLLM internal script that reuses HumanEval/SWE-bench-Lite
problems to validate a prompt-compression feature) is unshipped dev tooling,
not a product surface. The one genuine "does my gateway degrade model
quality vs. the provider directly" product in the wild — Artificial
Analysis's Endpoint Accuracy Index — is built and operated by a specialized
third party, not bolted onto a gateway vendor's own eval harness, and it
exists specifically to catch provider-serving drift (quantization, sampling
defaults, context handling), not to rank models. Where genuine eval
frameworks (Inspect AI, promptfoo) do ingest foreign benchmark data, they do
it through a generic adapter (a single required field plus a
field-mapping/custom-function layer), never through native
HumanEval/SWE-bench/MMLU-specific loaders — reinforcing that "benchmark
interop" in practice means "one more generic dataset import path," not a new
scoring paradigm. No evidence of customer demand for running standard
capability benchmarks *through* a gateway-testing harness was found either
way — this remains a genuine gap, not a refutation. Meanwhile, Kelvran's
existing gateway/cache-correctness threat model is independently validated by
three real, current (Sept 2026), unresolved LiteLLM production bugs — a
pass-through streaming-parse failure, silently mispriced non-streaming
pass-through spend, and a guardrail that is loaded/attached but never
actually inspects the relayed response — none of which any capability
benchmark could ever detect, since HumanEval/SWE-bench/MMLU never touch a
gateway's routing/caching/pass-through/guardrail layer at all.

## Findings

### Finding 1 — No real gateway product treats standard capability benchmarks as a feature; "benchmark" in gateway marketing means infra, not capability

**Verdict: not_yet — and not on the roadmap as stated.** There is no evidence
this is ever adopted as a genuine feature by comparable products, so there is
nothing to catch up to.

Across every primary surface checked for both major open-source gateways —
LiteLLM's GitHub README, its dedicated `/ai-gateway` product page, its
official Benchmarks doc page, and Portkey's GitHub README, its AI Gateway doc
page, and its Guardrails doc page — the word "benchmark" (where present at
all) refers exclusively to gateway-overhead metrics (P95 latency, RPS,
throughput, memory), and the strings "HumanEval," "SWE-bench," and "MMLU"
appear on **none** of them. LiteLLM's own "AI Gateway Bench" is explicitly an
"open standard for benchmarking and comparing AI gateways" on latency
overhead, not model capability. Portkey's only capability-adjacent feature,
Guardrails, is a boolean pass/fail enforcement layer (regex, JSON schema,
PII, prompt-injection checks returning `true`/`false`) — not a
scoring/benchmarking system, and its docs never mention any standard
benchmark format. Neither product's marketing surface even uses the word
"testing" as a model-quality concept — LiteLLM's homepage's few eval-adjacent
mentions are a mocked demo API-key name and a guardrail pass-rate table, not
a real feature.

*Confidence: high — 8 primary sources (GitHub READMEs, official product/doc
pages), unanimous 3-0 votes on all 8 underlying claims, independently
re-fetched live rather than relying on cached summaries.*

### Finding 2 — The one place benchmark-shaped code exists in a real gateway codebase is internal self-test tooling for a gateway feature, not benchmark interop

**Verdict: not_yet, but conceptually already covered by Kelvran's `dogfood`
category.** No new interop surface needed — this pattern (capability-shaped
payloads used to regression-test a gateway's own transformation) is exactly
what Kelvran's `dogfood` regression cases already do for its own features.

LiteLLM's only HumanEval-adjacent code (`scripts/eval_compression.py`, plus a
sibling `tests/eval_swe_bench.py` using SWE-bench Lite) is not a
benchmark-ingestion feature at all: it hand-writes a handful of
HumanEval-style function-completion problems and runs each one twice — once
raw, once through `litellm.compress()` — to check whether the gateway's own
prompt-compression feature preserves "the signal the model needs to solve
the task while reducing token usage." It lives in `scripts/`/`tests/`
internal dev tooling with no docs-site page presenting it as a customer
feature, and it hand-writes problems rather than ingesting the real
HumanEval/SWE-bench datasets or formats. Structurally this is a
gateway-correctness check (does *our* transformation preserve task-solving
signal) wearing a capability benchmark's clothing — which is precisely the
category Kelvran's corpus already occupies, just for cache/cost/guardrail
correctness instead of compression correctness.

*Confidence: high on the code's existence and mechanics (verified against
raw file content via two independent fetch methods); medium on the
"gateway-correctness-check" framing specifically (2-1 vote), though the
framing is well-supported by the surrounding code and commit history.*

### Finding 3 — The genuine "does my gateway degrade model quality vs. calling the provider directly" use case exists, but as a specialized third-party product, not a gateway-vendor feature

**Verdict: not_yet for v2, but real enough to flag as a distinct, roadmap-
worthy (not urgent) idea** — see Finding 4 for scoping.

Artificial Analysis's Endpoint Accuracy Index re-runs a fixed suite of
capability-benchmark subsets (BFCL v4-500 tool/function-calling, HLE-250 hard
reasoning, AA-LCR-25 long-context recall) against each provider's hosted
endpoint for a given model, and reports how closely each endpoint reproduces
that same model's accuracy versus a self-hosted reference baseline. Its
explicitly stated rationale is server-side implementation drift — the same
model can behave differently across providers due to quantization, sampling
defaults, context handling, token limits, and prompt parsing — i.e.,
infrastructure/serving-correctness testing that happens to reuse
capability-benchmark subsets as its probe, not raw model-capability ranking.
This is real precedent for the exact use case named in RQ3, but it is run by
a dedicated benchmarking company as a standalone product, not shipped inside
a gateway's own eval tooling — nobody has built this *into* a gateway-testing
harness like Kelvran's.

*Confidence: medium — single primary source (methodology page), 2-1 vote;
independent secondary corroboration was attempted but blocked by search-tool
rate limits, so this rests on direct primary-source verification alone.*

### Finding 4 — Where real eval frameworks do ingest foreign benchmark data, the shape is always a generic adapter layer, never a native benchmark-specific loader

**Verdict: not_yet for v2 — if ever built, this is the correct minimal
shape to copy, not a new Run/Score paradigm.**

Both Inspect AI and Promptfoo — genuine, widely used eval frameworks (unlike
the gateways above) — converge on the same interop pattern. Inspect AI's
`Sample` object has exactly one required field (`input`) plus optional
`choices`/`target`/`id`/`metadata`/`sandbox`/`files`/`setup`; every foreign
dataset format must be transformed into this single canonical shape before
any evaluation runs, via either `FieldSpec` (declarative field-renaming) for
simple cases or a user-defined `record_to_sample` function for anything
requiring real custom processing — i.e., foreign schemas bend to Inspect's
model, never the reverse. Promptfoo's test cases use an unrelated generic
schema (`description`, `vars`, `assert`, `providers`) with a cartesian-product
multi-provider-vs-multi-test execution model, and its documentation's only
"standard format" support is generic dataset-import plumbing (CSV/JSON/JSONL,
HuggingFace Datasets, Google Sheets/SharePoint/Azure Blob) — a
site-wide check found **zero** pages anywhere on promptfoo.dev for HumanEval
or SWE-bench, and the one MMLU-adjacent guide just reuses the same generic
HuggingFace-dataset import syntax plus hand-written templating, not a
dedicated MMLU loader.

Synthesis on RQ3's second half (reuse of Kelvran's EvalCase/Run/Score model):
based on this pattern, a hypothetical minimum-viable interop would look like
a `record_to_sample`-style adapter function mapping an arbitrary foreign
dataset row into Kelvran's existing `EvalCase` shape — that part reuses
cleanly. But the "gateway vs. direct-provider" comparison itself does not fit
Kelvran's current `Score` model as-is: today's `Score` is a verdict against a
single expected answer (deterministic or judge-based); a quality-preservation
check needs a *paired* comparison between two `Run`s of the same `EvalCase`
(one via gateway, one direct-to-provider) with a diff/similarity-based
verdict — a new comparison-mode `Score`, not a drop-in reuse of the existing
one. *(This paragraph is inferential synthesis about Kelvran's own codebase,
not a sourced web claim — flagged accordingly; no claim in this research
round examined Kelvran's actual `EvalCase`/`Run`/`Score` implementation.)*

*Confidence: high on the Inspect AI/Promptfoo interop-shape claims (3-0 votes,
primary docs, directly and independently verified, including a site-wide
cross-check on promptfoo.dev); the EvalCase/Run/Score reuse conclusion itself
is a synthesis, not independently sourced — treat as informed opinion.*

### Finding 5 — Kelvran's own threat model is independently validated by real, current, unresolved production bugs in a comparable gateway — bugs no capability benchmark could ever surface

**Verdict: build_now is not applicable here (this isn't a benchmark-interop
finding) — but it's the strongest argument that v2 effort belongs in
deepening the existing corpus, not in benchmark interop.** Roadmap: continue
investing in Kelvran's own custom-corpus regression testing; this is
validated, not speculative.

Three separate, currently open (as of Sept 2026) GitHub issues against
LiteLLM's pass-through mode map directly onto Kelvran's existing corpus
categories: (a) a streaming SSE-parse failure on Anthropic-compatible
pass-through responses (filed 2026-09-07, unfixed PR open) — a
`cache_adversarial`/`dogfood`-shaped bug; (b) silent discarding of the
upstream provider's self-reported cost/token totals on non-streaming
pass-through calls while honoring them correctly on streaming calls to the
same endpoint, mispricing every non-streaming call with no warning — a
`cost_abuse`-shaped bug; and (c) a `tool_permission` guardrail with
`default_action: deny` that is loaded, attached, and "considered" for a
pass-through request (confirmed via debug logs) but never actually inspects
the upstream response body, letting a denied tool call (e.g. `Bash rm -rf /`)
relay back to the client unchanged with HTTP 200 — a `guardrail`-shaped bug.
None of these would ever be caught by HumanEval, SWE-bench, or MMLU, because
none of those benchmarks touch a gateway's pass-through/cache/cost/guardrail
layer at all — they test whether a model can solve a coding or reasoning
problem, not whether the plumbing around the model preserves cost accounting
or enforces a deny rule. This is direct, current-day corroboration that
Kelvran's category of testing (infrastructure-correctness, not
model-capability) catches real bugs that the standard-benchmark category
structurally cannot.

*Confidence: high — verified directly against live GitHub issues/PRs via
`gh api`/`gh issue view` (not the claims' own weak citation, a generic issue-
search URL), all three issues confirmed open and unfixed as of research date;
two of the three underlying claims were 2-1 votes, but the verification
evidence for both (full reproduction steps, matching debug logs, root-cause
PRs) is strong regardless of vote margin.*

## Answering the Roadmap Question (RQ4)

**Not a v2 priority; not a distraction to actively avoid, but also nothing to
build now.** The evidence converges on a clear category mismatch: Kelvran's
evals system tests whether the *gateway* behaves correctly (cache poisoning,
TOCTOU races, cost mass-assignment, guardrail bypass) — a property that has
no relationship to whether a *model* can solve a coding or reasoning problem.
No comparable product treats standard-benchmark interop as a real feature
(Finding 1), the one adjacent example is bespoke internal tooling Kelvran's
`dogfood` category already conceptually covers (Finding 2), the one genuine
version of the "verify gateway doesn't degrade quality" use case is a
specialized third-party product built from scratch rather than a
gateway-vendor add-on (Finding 3), and even in eval frameworks that do
ingest foreign data, it's via a generic adapter, not a benchmark-specific
integration (Finding 4) — while Kelvran's actual, already-shipped value
proposition keeps finding real, current bugs that standard benchmarks
structurally cannot see (Finding 5). If a concrete customer ever asks for
"verify my gateway doesn't degrade model quality vs. the provider directly,"
the right response is a small, generic dataset-import adapter (à la
Inspect AI's `record_to_sample` or Promptfoo's field-mapping) feeding a new
paired-comparison `Score` mode — not a HumanEval/SWE-bench/MMLU-native
integration. Until that concrete ask exists, this belongs in the backlog as
a "someday/maybe," not in v2 scope.

## Caveats

- **Vote margins**: 14 of the 20 underlying claims passed 3-0 (unanimous);
  6 passed only 2-1 (claims behind Finding 2's "gateway-correctness-check"
  framing, Finding 3's Artificial Analysis rationale, Finding 4's Promptfoo
  cartesian-product characterization, and two of Finding 5's LiteLLM bug
  claims). None were refuted outright, but findings resting more heavily on
  2-1 claims (Findings 2, 3) carry medium rather than high confidence.
- **Citation hygiene on the LiteLLM bug claims**: two of the sourced claims
  in Finding 5 cited a generic GitHub issue-search URL rather than a direct
  permalink — sloppy citation practice flagged by verifiers — but the
  underlying issues (#37105, #32201, #40117) were independently confirmed as
  real, specific, and current via direct `gh api`/`gh issue view` calls, so
  the substance holds despite the weak citation.
- **Time-sensitivity**: Finding 5's three bugs are described as OPEN/unfixed
  as of the research date (2026-09-11 / early September 2026 verification
  passes). Open-source issues can be merged or closed quickly; re-verify
  "still open" status before citing these as ongoing evidence in any future
  document.
- **Tooling gaps**: several verification passes (notably around Finding 3's
  Artificial Analysis claim and one LiteLLM claim) could not get independent
  secondary corroboration because Exa/Tavily/Bing/DuckDuckGo were
  rate-limited or bot-blocked during verification — those findings rest on
  direct primary-source fetches alone, which is adequate for narrow
  content-presence/absence claims but weaker for claims about a product's
  underlying motivation or market position.
- **No demand-signal research was found either way** (RQ2) — this is a
  genuine evidentiary gap, not a finding that demand doesn't exist. General
  web research is a poor instrument for detecting "did any Kelvran user ever
  ask for this"; that question can only really be answered by checking
  Kelvran's own support/feedback channels directly.
- **Finding 4's EvalCase/Run/Score reuse conclusion is inferential**, drawn
  from how comparable frameworks structure similar comparisons, not from any
  claim that examined Kelvran's actual code. Treat it as a reasoned
  hypothesis to validate against the real `evals/` codebase before acting on
  it, not as a verified fact.

## Open Questions

1. Has any Kelvran user or prospective customer ever actually asked for
   "run HumanEval/MMLU/SWE-bench through the gateway and compare to direct"?
   This research found no evidence either way — check internal
   support/feedback channels directly rather than general web research.
2. If a paired gateway-vs-direct comparison feature were ever built, what
   diffing method should the new comparison-mode `Score` use — exact match,
   embedding-similarity threshold, or LLM-judge — and how would it
   distinguish genuine quality degradation from ordinary model
   non-determinism (temperature/sampling variance) at the same provider?
3. A gateway-vs-direct comparison would need to bypass Kelvran's own
   response cache to get a fair apples-to-apples measurement (otherwise a
   cache hit would trivially "match" and mask any real degradation) — does
   this conflict or overlap with the existing `cache_adversarial` corpus's
   cache-bypass mechanics, or would it need its own isolated code path?
4. Does Artificial Analysis's statistical framing (confidence-interval bands
   classifying a provider endpoint as "within range" vs. "significantly
   outside range" of a reference baseline) offer a directly reusable
   statistical pattern for a future gateway-quality-preservation score,
   given Kelvran already has Wilson-lower-bound CI machinery in its
   regression-corpus gate?
