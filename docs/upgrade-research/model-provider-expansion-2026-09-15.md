# Model Provider Expansion — Upgrade Research (2026-09-15)

Recovered by hand from the deep-research workflow's own returned JSON — its file-write step failed
silently, the same recurring class per `AGENTS.md`'s Gotchas section. Reconstructed from the
workflow's full structured result, not re-run.

## Scope

Compared Kelvran's 5 provider adapters (`openai`, `anthropic`, `gemini`, `bedrock`, `openaicompat`
— the last covering vLLM/TGI/Ollama/llama.cpp-style OpenAI-wire endpoints) and its canonical schema
+ per-feature capability checks (`gateway/internal/adapter/capabilities.go:41`,
`adapter.SupportsStructuredOutput`, already generalized to per-model logic for Bedrock at
`capabilities.go:56`) against LiteLLM's, Portkey's, and OpenRouter's real 2026 provider coverage.

## Findings

### 1. xAI/Grok — needs a genuine new adapter, not just an `openaicompat` config entry

xAI's own docs mark its Chat Completions endpoint as legacy, with a dedicated "Migrating to
Responses API" guide — new features land in the Responses API first, a distinct wire shape.
Separately, xAI reports reasoning tokens in `usage.completion_tokens_details.reasoning_tokens` as
**additive** to `completion_tokens`, unlike OpenAI's folded accounting — confirmed via xAI's own
pricing table, Google Cloud's Grok partner docs (worked arithmetic: 663+50+124=837), and a live
captured API response from a Vercel AI SDK GitHub issue (279+6+89=374). LiteLLM corroborates with a
dedicated `xai/` model prefix and xAI-specific response fields distinct from its generic
`openai_compatible` bucket. Two claims that xAI is a plain OpenAI-wire drop-in were explicitly
refuted (0-3, 1-2).

**Verdict: not_yet**, unless a real customer/use-case asks for it. If triggered: model the adapter
closest on `openai` (not `openaicompat`), since it needs custom usage/reasoning-token normalization
(additive, not subset) plus eventual Responses-API support — flag the Responses-API migration as a
2026-in-progress moving target, not a stable one to build against today.

### 2. Cohere — needs a genuine adapter or a hardened `openaicompat` profile

Cohere's own compatibility docs state `reasoning_effort` supports only `"none"`/`"high"` — not
`"medium"`/`"low"` — mapping to a binary native-thinking toggle, not OpenAI's graduated scale. The
same docs list 10 standard OpenAI chat params (`store`, `metadata`, `logit_bias`, `top_logprobs`,
`n`, `modalities`, `prediction`, `audio`, `service_tier`, `parallel_tool_calls`) as unsupported. A
claim that Cohere is a clean base-URL-only drop-in was refuted (1-2).

**Verdict: not_yet** by default, build_now only if a concrete customer needs Cohere. If triggered:
`openaicompat` as a starting skeleton, but requires an explicit denylist/translation layer for the
10 unsupported params plus special-casing `reasoning_effort`. **Open question, undocumented**:
whether sending an unsupported param silently drops or hard-errors — test empirically before
shipping.

### 3. DeepSeek — multiple distinct wire formats, not a single openaicompat-dialect provider

DeepSeek's own changelog (cross-corroborated via two independent DeepSeek pages) shows V4-Pro/V4-Flash
reachable via both the OpenAI Chat Completions interface AND a distinct native Anthropic-compatible
interface at the same `base_url` (since 2026-04-24), plus native OpenAI Responses API support for
Codex-style agentic clients (since 2026-08-13/2026-07-31). Both LiteLLM and Portkey list DeepSeek as
its own dedicated provider entry rather than a generic OpenAI-compatible bucket.

**Verdict: not_yet** as a full new adapter. Pragmatic near-term move if triggered: point Kelvran's
existing `anthropic` adapter at DeepSeek's Anthropic-compatible `base_url` for that dialect, and/or
the existing `openai`/`openaicompat` adapter for its Chat Completions dialect. Genuinely new adapter
code would only be needed for DeepSeek's newer Responses-API surface.

### 4. Mistral — likely already covered by `openaicompat`, but unverified at the field level

Mistral's own migration guide states its Chat Completions API follows the same request structure as
OpenAI's, with most migrations needing only a client-import/base-URL/model-name change (2-1 vote). A
stronger "any OpenAI client works with zero translation" claim was explicitly refuted (0-3).

**Verdict: not_yet** — no code work needed in the near term; treat as likely already coverable by
`openaicompat`, but verify tool-calling/structured-output field parity empirically before assuming
zero-code coverage.

### 5. Together AI, Groq, Fireworks AI — listed separately by comparators, but genuinely unresearched at the wire-format level

LiteLLM and Portkey both give these dedicated docs pages, structurally separate from LiteLLM's
generic "OpenAI-Compatible Endpoints" entry — real precedent that a comparable gateway treats them
as first-class. But no claim in this round verified their actual request/response shape differs
from OpenAI's; the dedicated-listing status may reflect discoverability/marketing/rate-limit-tier
conventions rather than a proven need for new adapter code.

**Verdict: not_yet**, and genuinely unresearched — don't treat "LiteLLM lists them separately" as
proof they need new code. A follow-up pass would need to independently verify each one's wire
format the way this round did for xAI/Cohere/DeepSeek.

### 6. Capability negotiation — no better pattern exists to adopt; Kelvran's current approach is already ahead

Kelvran already does fine-grained per-provider-per-model capability checks today
(`capabilities.go:41`/`:56`/`:87`). The closest external precedent for something more structured —
LiteLLM PR #30032's `ProviderSupports`/`ProviderStrengths` — was verified via `gh pr view`/`gh api`
to be unmerged, stale, authored by a first-time contributor, reviewed only by a bot flagging
convention violations, with zero runtime integration and 0% patch coverage. The other real
precedent, `inja-online/llm-gateway`'s `compatibility-matrix.md`, gates capabilities at a coarser
`kind`-level default table (openai/openai_compat/anthropic/google) — **coarser** than Kelvran's
existing per-model checks, not finer.

**Verdict: not_yet / no change needed** — there is no established better pattern to adopt; Kelvran's
current granularity already exceeds both precedents found.

### 7. Canonical-schema design direction — validated by OpenRouter's own precedent (lower confidence)

OpenRouter's docs state it "standardizes the tool calling interface across models and providers,"
using an identical `tools`/`tool_calls`/`role:tool` shape regardless of each provider's native
format — consistent with Kelvran's own canonical `adapter.ChatRequest`/`ChatResponse`/`Message`
design. Confidence: medium (2-1 vote); a related, more specific claim about OpenRouter's exact
negotiation mechanics (a static filterable attribute vs. a runtime handshake) was refuted (1-2).

## Open questions (not resolved this round)

- Do Together AI, Groq, and Fireworks AI actually deviate from OpenAI's wire format, or are they
  genuinely full `openaicompat`-only cases that LiteLLM/Portkey merely list separately for
  discoverability?
- Is there an actual Kelvran customer/use-case currently asking for xAI, Cohere, or DeepSeek
  specifically? This research had no visibility into Kelvran's customer pipeline.
- Given DeepSeek's and xAI's Responses-API migrations are both fast-moving 2026 developments, does
  Kelvran's canonical schema already have a place to represent Responses-API-style
  state/conversation-thread semantics, or would supporting either provider's Responses API require
  a canonical-schema change beyond just a new adapter file?
- For Cohere: does an unsupported OpenAI param silently drop or hard-error through the compatibility
  endpoint? Undocumented, and materially changes how risky an `openaicompat`-shortcut would be.

## Caveats

Several supporting claims carried split 2-1 votes (xAI comparator-gateway framing, Mistral parity
claim, OpenRouter schema-unification claim) — directionally right but individually less certain than
the 3-0 claims they're paired with. The DeepSeek-Responses-API and Cohere-parameter-gap findings are
the most time-sensitive (2026 changelog entries/current API docs) and should be re-verified if acted
on more than ~2-3 months later. This synthesis is external-precedent-only for the comparator-gateway
claims — it did not re-audit `gateway/ARCHITECTURE.md`'s full Canonical Schema section text beyond a
directory listing and one grep of `capabilities.go`.

## Sources

Primary: `docs.x.ai`, `docs.litellm.ai/docs/providers`, `portkey-ai-gateway.mintlify.app`,
`docs.mistral.ai`, `docs.cohere.com`, `api-docs.deepseek.com`, `github.com/BerriAI/litellm/pull/30032`,
`github.com/inja-online/llm-gateway`, `openrouter.ai/docs`. 26 sources fetched across 6 search
angles; 116 claims extracted, 25 adversarially verified (14 confirmed, 11 refuted), synthesized to 7
findings above.
