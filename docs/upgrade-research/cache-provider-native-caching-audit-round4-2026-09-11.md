# Cross-Provider Native-Caching Audit — Round 4 (2026-09-11)

Scope: a focused live-API-verification lens on every upstream provider's OWN native caching
mechanisms (write-side opt-in marker + read-side cost-reporting signal), motivated by the two
real bugs found by hand in this exact area on 2026-09-11 (Bedrock SigV4 signing, OpenAI's
unparsed `cached_tokens` field). Grounded against
`docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md` and
`docs/rfcs/2026-09-10-gateway-cache-token-cost-accounting.md` (incl. its 2026-09-11
addendum); externally verified against Gemini's own API docs + 3 vendor-maintained Firebase
SDKs + `googleapis/python-genai` GitHub issues, vLLM/Ollama/llama.cpp/TGI source code, and
Anthropic's/AWS Bedrock's own release notes/pricing pages, each claim adversarially
3-vote-verified before inclusion here.

## Executive Summary

This round resolves the single most valuable open question from the 2026-09-11 addendum:
**Gemini's automatic implicit caching DOES populate `usageMetadata.cachedContentTokenCount`**
— confirmed by three independent vendor-maintained SDKs (Firebase JS/Android/Unity) plus real
production repro data in `googleapis/python-genai` GitHub issues, even though the raw REST API
reference text is deliberately generic and doesn't name the mechanism. This is buildable now
as a pure read-side extraction with zero write-side change, since `gemini.go` never builds
today. For openaicompat, the runtime-by-runtime split is real and uneven: vLLM, Ollama, and
llama.cpp all genuinely wire a real prefix/KV-cache-hit count into the OpenAI-shaped
`prompt_tokens_details.cached_tokens` field (vLLM's is gated behind a default-off server
flag); TGI has zero cache-token support anywhere in its response types. On the already-shipped
Anthropic/Bedrock adapters, three concrete drifts surfaced since 2026-09-10/11: Bedrock added
a GA 1-hour cache TTL tier (new `ttl` field, differential pricing, new `cacheDetails` response
array) that Kelvran's `CachePoint` marker doesn't yet expose; Bedrock's total-input-token
accounting formula requires summing three fields, worth an explicit verification pass against
Kelvran's cost-accounting code; and Anthropic shipped both an automatic-caching mode (Feb
2026) and a new, cheaper cache-read pricing tier specific to Fable 5.1/Mythos 5.1 (0.025x vs
the standard 0.1x multiplier) that a flat per-vendor cache-read rate would misprice. No
live-verified evidence was found on caching mechanisms for xAI/Grok, Mistral, DeepSeek, or
Cohere — that remains a genuine, unresolved gap.

**BUILD NOW:** (1) Gemini read-side cache extraction — wire `usageMetadata.cachedContentTokenCount`
into the gemini adapter, zero write-side change needed. (2) Tighten openaicompat disclosure
to name vLLM/Ollama/llama.cpp as genuinely supported (with vLLM's opt-in-flag caveat) and TGI
as not supported. (3) Anthropic per-model cache-read pricing override for Fable 5.1/Mythos
5.1 (0.025x, not the standard 0.1x).

**NOT YET / NEEDS DECISION:** (4) Bedrock 1-hour TTL tier — real, GA, differentially priced,
needs write-side `ttl` field plus tier-aware pricing; not urgent since default 5-minute
behavior is unaffected. (5) Bedrock `cacheDetails` response field — verify it isn't silently
dropped. (6) Bedrock total-input-tokens formula — verify `costaccounting.go` sums all three
fields rather than using `inputTokens` alone (a verification task, not a confirmed bug). (7)
Anthropic automatic-caching mode — could simplify write-side breakpoint logic; optimization,
not a fix.

**BLOCKED / UNRESOLVED:** (8) New/emerging provider caching (xAI/Grok, Mistral, DeepSeek,
Cohere) — no evidence surfaced this round; needs its own dedicated research pass.

## Findings

### Finding 1 — Gemini implicit caching DOES populate `cachedContentTokenCount`
**Confidence: high**

Gemini's automatic, zero-opt-in implicit caching DOES populate
`usageMetadata.cachedContentTokenCount` on real cache hits. Implicit caching is on-by-default
for Gemini 2.5+. The `generateContent` REST reference documents
`cachedContentTokenCount`/`cacheTokensDetails[]` as real fields but is deliberately generic
about which caching mode populates them. Decisive evidence is at the SDK layer: Firebase JS
SDK's own doc comment states the field "equals zero when implicit caching is not active"
(logical converse confirms non-zero on implicit hits), corroborated independently by Firebase
Android and Unity SDKs explicitly adding "implicit caching" support for this exact field,
plus real production repro data in `googleapis/python-genai` GitHub issues (#2064, #1880,
#1896) showing the field alternating 0/nonzero on plain `generateContent` calls with zero
explicit `CachedContent` API usage. Caveat: Google's newer, separate Interactions API surface
uses a differently-named field (`usage.total_cached_tokens`) — a distinct surface, not a
contradiction, but Kelvran must confirm which API surface its adapter targets (classic
`generateContent`, which this finding covers).

**Verdict: BUILD NOW** — a pure read-side extraction, zero write-side change (`gemini.go`
never builds today).

### Finding 2 — openaicompat runtimes: vLLM/Ollama/llama.cpp genuinely support it, TGI does not
**Confidence: high**

Self-hosted OpenAI-compatible runtimes are unevenly verified: vLLM, Ollama, and llama.cpp
genuinely populate `prompt_tokens_details.cached_tokens` from real engine-level prefix/KV-cache-hit
counts; TGI has zero cache-token support anywhere in its response types. vLLM: unbroken
source chain from the scheduler's real KV-cache-manager prefix-match lookup to
`PromptTokenUsageInfo.cached_tokens`, but gated behind a default-off server flag
(`enable_prompt_tokens_details`). Ollama: `CachedTokens` populated from
`r.Metrics.PromptEvalCachedCount`, sourced from llama.cpp server's native `cache_n` field,
wired identically into both `/v1/chat/completions` and `/v1/completions`; caveat — only via
the legacy llama.cpp-backed runner and experimental MLX runner, not Ollama's newer pure-Go
engine path. llama.cpp: `n_prompt_tokens_cache` (from real KV-cache-restore token counts)
mapped directly into `prompt_tokens_details.cached_tokens` for both completion endpoints.
TGI: `Usage` struct has exactly three fields (`prompt_tokens`, `completion_tokens`,
`total_tokens`) with zero occurrences of `cached_tokens`, `prompt_tokens_details`, or even the
word "cache" anywhere in the router source.

**Verdict: BUILD NOW** — tighten the "best-effort, unverified" disclosure to this exact
breakdown.

### Finding 3 — Bedrock added a GA 1-hour cache TTL tier since last verified
**Confidence: high**

Amazon Bedrock added a GA 1-hour cache TTL tier (Claude Sonnet 4.5, Haiku 4.5, Opus 4.5) via a
new `ttl` field on `cachePoint`/`cache_control` (values `5m`|`1h`), billed at a different rate
than the 5-minute default — a write-side and pricing gap Kelvran's flat `CachePoint` marker
and flat cache-rate cost model don't expose. Three independent AWS primary sources agree on
the `ttl` field, its value set, GA scope to three named models, and differential billing
(exact multiplier not disclosed). Cross-checked against Kelvran's actual code: `bedrock.go`
documents `CachePoint` as having no TTL concept (`{type:default}` only), and
`costaccounting.go` uses a single flat `CacheReadPerToken`/`CacheCreationPerToken` rate per
model with no TTL/tier dimension.

**Verdict: NOT YET** — real, not hypothetical, but not urgent since default 5-minute behavior
is unaffected.

### Finding 4 — Bedrock's `cacheDetails` response field and total-input-tokens formula need a verification pass
**Confidence: high**

Bedrock's Converse API response now includes a third caching field, `cacheDetails` (array of
`{inputTokens, ttl}`), alongside `cacheReadInputTokens`/`cacheWriteInputTokens`. Separately,
Bedrock's Converse API requires `total input tokens = inputTokens + cacheReadInputTokens +
cacheWriteInputTokens` (`inputTokens` alone represents only the non-cached portion when
caching is enabled) — AWS's documented formula, verbatim, character-for-character match to
AWS's own "Important" callout in the current User Guide. Whether Kelvran's actual
`costaccounting.go` implements either of these correctly was NOT verified in this synthesis
and is recommended as an immediate follow-up, not a confirmed bug.

**Verdict: NOT YET / NEEDS VERIFICATION** — advisory; aggregate cost totals don't strictly
require `cacheDetails`, but TTL-tier attribution is lost without it.

### Finding 5 — Anthropic: automatic caching mode + a cheaper cache-read tier for Fable 5.1/Mythos 5.1
**Confidence: high**

Anthropic launched automatic prompt caching on the Messages API (Feb 19, 2026): a single
`cache_control` field now auto-caches the last cacheable block and advances the breakpoint as
the conversation grows, with no manual breakpoint management required (Claude API and
Microsoft Foundry preview; not available on legacy Bedrock Opus 4.6-and-earlier integration).
Separately, as of Sept 1, 2026, Anthropic introduced a cheaper prompt-cache-read pricing tier
specific to Claude Fable 5.1 and Claude Mythos 5.1: cache reads at 0.025x base input cost
versus the standard 0.1x multiplier on other models; cache-write pricing is unchanged. A flat
per-vendor cache-read multiplier in Kelvran's cost-accounting would misprice these two
specific models (overcharging cache reads by 4x). Both confirmed verbatim across two
independent, current, official Anthropic documentation pages.

**Verdict: BUILD NOW** for the Fable 5.1/Mythos 5.1 pricing override (a real cost-accuracy
fix). **NOT YET / optimization** for the automatic-caching write-side simplification —
not a fix for a defect.

### Finding 6 — New/emerging providers (xAI/Grok, Mistral, DeepSeek, Cohere): unresolved
**Confidence: low**

No live-verified evidence was found this round on caching mechanisms for xAI/Grok, Mistral,
DeepSeek, or Cohere — genuinely unresolved and blocked pending a dedicated research pass
before any adapter work targeting these vendors. This is an absence-of-evidence research gap,
not a positive finding that these vendors lack caching.

## Caveats

Verification in this round leaned heavily on vendor-maintained SDK source code and official
documentation/changelog pages rather than live-credentialed API calls — Kelvran has real AWS
Bedrock credentials in `evals/.env` but no Gemini credentials, so the Gemini implicit-caching
finding, while very well corroborated by independent SDKs and real GitHub-issue repro data,
is still not a first-party live test against Kelvran's own Gemini calls. The vLLM finding
carries an operational caveat (cache-token reporting is off by default, requiring
`enable_prompt_tokens_details=true` server-side). The Ollama finding has a coverage gap: the
field is only populated by the legacy llama.cpp-backed runner and experimental MLX runner, not
Ollama's newer pure-Go engine path. Two claims were explicitly refuted during verification and
must not be carried forward: that the documented cache-hit field name is
`usage.total_cached_tokens` for the classic `generateContent` API (that name belongs to the
separate, newer Interactions API surface), and that `promptTokenCount`'s field description
ties cache accounting specifically to the explicit `cachedContent` parameter (it doesn't make
that distinction either way).

## Open Questions

- Do xAI/Grok, Mistral, DeepSeek, or Cohere have caching mechanisms worth knowing about before
  Kelvran builds adapters for them?
- Does Kelvran's actual `gateway/internal/costaccounting/costaccounting.go` currently use
  `inputTokens` alone or the correct three-field sum for Bedrock total-input-token billing?
- What is the exact dollar multiplier AWS charges for the new Bedrock 1-hour cache TTL tier
  relative to the 5-minute tier? Sources confirm a different rate exists but none disclosed
  the specific number.
- Which Gemini API surface does Kelvran's (currently unbuilt) `gemini.go` adapter target —
  classic `generateContent` (uses `cachedContentTokenCount`) or the newer Interactions API
  (uses `usage.total_cached_tokens`)?
