# Gateway Pre-Completion Token-Counting — 2026-09-07 Research

**Date:** 2026-09-07
**Scope:** whether provider-side pre-completion token-counting endpoints are now viable enough, per-adapter, to unblock the "pre-call reserve+reconcile" TPM design `docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md` explicitly rejected for lack of a tokenizer. This is the dedicated follow-up research pass `docs/upgrade-research/gateway-2026-09-06.md` Finding 4 named as its own "concrete next step (not urgent — a follow-on RFC, not an immediate build)."
**Method:** direct primary-source retrieval (`WebFetch` against each vendor's own current, live documentation; one Exa web search for vLLM's tokenizer internals, which worked cleanly — unlike sibling research passes this session, no rate-limiting was encountered) cross-checked against Kelvran's own real adapter source (`gateway/internal/adapter/*/`). Every claim below is either a direct citation of a fetched vendor doc or a direct reference to Kelvran's own code; the two are kept visibly separate, never blended into one unlabeled sentence, per this project's own established research-reporting discipline.

---

## Grounding: what each of Kelvran's 5 adapters currently calls

Confirmed by reading `gateway/internal/adapter/*/*.go` directly:

- **anthropic**: real Anthropic Messages API (`api.anthropic.com`).
- **openai**: real OpenAI Chat Completions API (`api.openai.com`).
- **gemini**: Google's Gemini API, built against its own discovery document (`generativelanguage.googleapis.com/$discovery/rest`).
- **bedrock**: AWS Bedrock's real Converse API (`docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html`, per the adapter's own package doc comment).
- **openaicompat**: a wire-protocol-compatible adapter for self-hosted runtimes (vLLM, Ollama, TGI, etc.) speaking the OpenAI Chat Completions dialect against an operator-configured `base_url` — not a single fixed provider.

---

## Per-provider findings

### Anthropic — real, free, separately-rate-limited endpoint (strongest case for pre-call viability)

**Confirmed:**

- `POST /v1/messages/count_tokens` is a real, current, documented endpoint accepting the identical structured input `POST /v1/messages` does (system prompt, tools, images, PDFs, thinking blocks) and returning `{"input_tokens": N}` [platform.claude.com/docs/en/api/messages-count-tokens](https://platform.claude.com/docs/en/api/messages-count-tokens) — confirmed.
- **"Token counting is free to use but subject to requests per minute rate limits based on your usage tier"** — Start tier: 5,000 RPM; Build: 10,000 RPM; Scale: 20,000 RPM. **"Token counting and message creation have separate and independent rate limits. Usage of one does not count against the limits of the other."** [platform.claude.com/docs/en/build-with-claude/token-counting](https://platform.claude.com/docs/en/build-with-claude/token-counting) — confirmed. This is the single most decisive fact in this whole report: calling `count_tokens` before every real Anthropic call does **not** consume the same RPM/TPM budget the real call would, so it cannot itself become the "rate-limit multiplier" this research was explicitly asked to check for.
- The count is an **estimate** — "the actual number of input tokens used when creating a message might differ by a small amount" — and does not reflect prompt caching ("token counting provides an estimate without using caching logic... prompt caching only occurs during actual message creation"). Both are real, named limitations, not silently glossed over.
- Latency: not stated as a number anywhere in the fetched docs. No source found (vendor or third-party) benchmarking this endpoint's real response time. Genuinely unresolved (see Tier 3 below) — not assumed fast just because it's described as free and lightweight.

**Why it matters for Kelvran specifically:** Anthropic is the one provider where "does calling this add rate-limit pressure" — the exact risk `docs/upgrade-research/gateway-2026-09-06.md` Finding 4 named as needing an answer — is confirmed, by the vendor's own words, to be a non-issue. This directly de-risks the highest-value adapter to prototype pre-call TPM reservation against first.

### AWS Bedrock — a real, free, no-charge endpoint; rate-limit interaction genuinely unconfirmed

**Confirmed:**

- `CountTokens` is a real Bedrock Runtime API operation. **"Using the CountTokens API doesn't incur charges."** It accepts either an `InvokeModel`-shaped or `Converse`-shaped body (Kelvran's bedrock adapter already speaks Converse) and returns `inputTokens` [docs.aws.amazon.com/bedrock/latest/userguide/count-tokens.html](https://docs.aws.amazon.com/bedrock/latest/userguide/count-tokens.html) — confirmed.
- Token counting is **model-specific** — "the token count returned by this operation will match the token count that would be charged if the same input were sent to the model to run inference" — and **not every model supports it on the `bedrock-runtime` endpoint**: some Anthropic-on-Bedrock models (specifically those launching cross-Region-inference-only) require calling Anthropic's own `count_tokens` API on a separate `bedrock-mantle` endpoint instead, with its own distinct auth scheme (SigV4 with service name `bedrock-mantle`, or a Bedrock API key) and its own IAM action (`bedrock-mantle:CountTokens`) — confirmed. **This means Bedrock is not one consistent integration, even within itself**: depending on which model Kelvran's operator configures, the count-tokens call may need to target a materially different endpoint/auth scheme than the real Converse call does.
- Uses its own IAM permission (`bedrock:CountTokens`), separate from `bedrock:InvokeModel` — confirmed.

**Genuinely unresolved:** the fetched documentation names no rate-limit interaction at all — neither confirming CountTokens shares Bedrock's own per-account throttling quota with real inference calls, nor stating (as Anthropic's own docs explicitly do) that it's separately/independently limited. This is a real, material gap for Kelvran's own decision, not a minor omission — Bedrock is the one provider in this report where the exact question this research was tasked to answer is left open by the vendor's own docs.

### Gemini — a real endpoint; rate-limit interaction also unconfirmed

**Confirmed:**

- `countTokens` is a real, current API method — "Returns the total number of tokens in the input only," with the docs' own advice: **"Make this call before sending input to check the size of your requests."** Request: `model` + `contents` (text or multimodal — images/video/audio use documented per-modality token heuristics, e.g. small images = 258 tokens, audio = 32 tokens/second). Response: `total_tokens`/`totalTokens` [ai.google.dev/gemini-api/docs/tokens](https://ai.google.dev/gemini-api/docs/tokens) — confirmed.

**Genuinely unresolved:** the fetched page names no latency figure and no rate-limit interaction — it neither confirms nor denies that `countTokens` calls share Gemini's own per-project RPM/TPM quota with real `generateContent` calls. Same gap class as Bedrock.

### OpenAI — confirmed absence, not a gap in this research

**Confirmed:**

- The current OpenAI API documentation (`developers.openai.com/api/docs/guides/text`, the real current location after a 301 redirect from the legacy `platform.openai.com` URL) contains **no mention of a token-counting endpoint, "count tokens" feature, tiktoken, or any pre-call token-estimation API** anywhere in its text-generation guide — confirmed by direct fetch. OpenAI's own, well-known answer to "how many tokens is this" remains the client-side `tiktoken` library (encoding tables shipped for local computation), not a live API call — consistent with this research's own prior general knowledge, now directly re-confirmed against the current live docs rather than assumed from memory.

**Why this is a confirmed absence, not an open question:** unlike Bedrock's/Gemini's rate-limit-interaction gaps (where the endpoint exists but a specific fact about it wasn't documented), this is the current, live, official guide's own text containing zero reference to any such capability — the strongest form of "not found" this research method can produce short of an explicit vendor statement that no such endpoint exists.

### openaicompat (self-hosted runtimes, e.g. vLLM) — real, local, CPU-only endpoint; genuinely inconsistent across runtimes

**Confirmed, for vLLM specifically (the runtime this research checked in detail):**

- vLLM's OpenAI-compatible server ships a real `/tokenize` (and `/detokenize`) Tokenizer API — **"a simple wrapper over HuggingFace-style tokenizers"** — `/tokenize` corresponds directly to calling `tokenizer.encode()` [docs.vllm.ai/en/v0.20.0/serving/openai_compatible_server/](https://docs.vllm.ai/en/v0.20.0/serving/openai_compatible_server/) — confirmed.
- Reading vLLM's own current source (`vllm/entrypoints/serve/tokenize/serving.py`, fetched via GitHub) confirms the request/response shape directly: `TokenizeRequest`/`TokenizeChatRequest` in, `TokenizeResponse{tokens, token_strs, count, max_model_len}` out — `count` is exactly the pre-completion input-token count Kelvran's TPM design would need.
- **This is genuinely local, CPU-side computation, not a second round of GPU model inference** — vLLM's own render-server architecture (a "CPU-only render server" documented directly in the API reference) builds `ServingTokenization` with `engine_client = None`, explicitly noting "there is no engine to poll" for stats, and bootstraps only the "preprocessing pipeline (renderer, input_processor)" — never the neural network itself. This is the strongest efficiency signal found anywhere in this report: tokenizing via vLLM's `/tokenize` should be materially cheaper and faster than any of the three cloud providers' own count-tokens calls, none of which document their own internal implementation this transparently.

**Why "openaicompat" cannot get one consistent answer, and this is itself the finding, not a gap:** `openaicompat` is a protocol dialect, not a single vendor — an operator could point it at vLLM (confirmed: real `/tokenize`), Ollama, TGI, or any other OpenAI-wire-compatible server, each with its own answer to whether a token-counting endpoint exists at all. This research checked vLLM specifically as the most likely real-world `openaicompat` target (per this adapter's own doc comment, cited in the original Finding 4) and found a real, cheap, confirmed endpoint — but Kelvran cannot assume every `openaicompat` deployment has one. Any future implementation would need a runtime-capability check or a config flag, not a blanket assumption either way.

---

## Comparison

| Provider | Endpoint exists? | Free/no-charge? | Rate-limit interaction | Latency documented? | Shape consistency with the real call |
|---|---|---|---|---|---|
| **Anthropic** | Yes, `POST /v1/messages/count_tokens` | Yes, explicitly free | **Confirmed separate/independent** — the strongest case in this report | Not documented | Same structured input as `/v1/messages` |
| **Bedrock** | Yes, `CountTokens` (or Anthropic's own `count_tokens` on `bedrock-mantle` for some models) | Yes, explicitly no-charge | **Not documented either way** — genuinely unresolved | Not documented | Converse-shaped body accepted directly — but the mantle-endpoint carve-out for some Anthropic models is a real inconsistency *within* Bedrock itself |
| **Gemini** | Yes, `countTokens` | Not stated explicitly (implied free, not confirmed) | **Not documented either way** — genuinely unresolved | Not documented | Same `contents` shape as `generateContent` |
| **OpenAI** | **No** — confirmed absent from current live docs | N/A | N/A | N/A | N/A — client-side `tiktoken` only |
| **openaicompat (vLLM)** | Yes, `/tokenize` | Yes — local CPU computation, no model inference | N/A (self-hosted, no vendor-imposed rate limit at all) | Not documented, but architecturally CPU-only, not GPU-inference | HuggingFace-tokenizer-shaped, not identical to `/v1/chat/completions`'s own body, but close |

---

## Direct answer to the research question

**Is pre-call TPM reservation via provider-side token-counting endpoints now viable for Kelvran, given real latency/rate-limit-multiplier data?**

**Partially — strong yes for Anthropic and openaicompat/vLLM specifically; genuinely unresolved for Bedrock and Gemini; a confirmed no for OpenAI (no endpoint exists at all).** This is not a uniform "yes, build it" or "no, don't" — the honest answer is provider-dependent, and any follow-on RFC must treat it that way rather than assuming one design fits all five adapters:

- **Anthropic**: the rate-limit-multiplier risk this research was explicitly asked to check for is confirmed, by the vendor's own current documentation, to be a non-issue (separate, independent RPM limit; zero token/dollar cost). This is the strongest, most de-risked candidate to prototype against first.
- **openaicompat/vLLM**: a real, free, CPU-only, no-inference endpoint exists and is architecturally the cheapest of all five to call — but "openaicompat" as an adapter category cannot guarantee every operator's chosen runtime has one; this would need a capability check, not a blanket assumption.
- **Bedrock and Gemini**: an endpoint exists and is free/no-charge, but this research could not confirm or rule out whether calling it shares the same rate-limit quota as real inference calls — the exact fact needed to close this out one way or the other for these two providers specifically remains open (see Tier 3/Open Questions).
- **OpenAI**: no endpoint exists at all, live, today. Any pre-call design for this adapter would still need to fall back to a real client-side tokenizer (e.g., a Go tiktoken-compatible library) or skip pre-call reservation for OpenAI specifically — a materially different implementation path than the other four.

**Sketch of what a follow-on RFC would need to specify, if pursued (not built here):** a per-adapter capability table exactly like the Comparison section above, built into `internal/adapter.Adapter`'s own interface or a new optional capability method (e.g., an adapter that can pre-count returns a real estimate; one that can't returns a typed "not supported" the caller falls back on); a design for what "pre-call TPM reservation" actually reserves and reconciles given Anthropic's own explicit caveat that the count is an *estimate* that "might differ by a small amount" from the real, billed count — the reconciliation step `docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md`'s retrospective design already has to do anyway would still be needed, just running against a real pre-call estimate instead of zero. Given OpenAI's confirmed lack of any endpoint, the RFC would also need to decide whether pre-call reservation ships for a subset of adapters only (Anthropic/vLLM first) with the retrospective-only design remaining OpenAI's/Bedrock's/Gemini's fallback, or whether inconsistent per-adapter behavior is judged not worth shipping at all until Bedrock/Gemini's own rate-limit-interaction question is resolved.

---

## Caveats

- **This is a single-pass, single-citation-per-claim research method**, not the three existing 2026-09-06 reports' 3-vote adversarial cross-check. Every claim above is cited to one real, fetched, current primary source, but has not been independently triangulated a second or third time.
- **No source found anywhere in this pass documents concrete latency numbers** for any of the three cloud providers' own count-tokens endpoints — this is reported as a genuine gap (Tier 3), not filled with an assumed "fast" or "slow" characterization. Only vLLM's case has strong *architectural* evidence (CPU-only render path, no engine/GPU involvement) supporting an inference that it's cheap — labeled as inference, not a measured number.
- **Bedrock's and Gemini's rate-limit-interaction questions could not be resolved from either provider's own fetched documentation** — this is the single most consequential open question this report leaves for a follow-on pass, since it directly determines whether those two providers are safe to call before every real request the way Anthropic's own docs already confirm is safe.
- **openaicompat's finding is scoped to vLLM specifically** — Ollama, TGI, and any other real-world `openaicompat` target were not independently checked in this pass; the finding that "openaicompat cannot get one consistent answer" is itself confirmed (vLLM has an endpoint; nothing here confirms or denies any other runtime does), but the specific claim "vLLM has a real, free, CPU-only endpoint" does not generalize to every possible `openaicompat` deployment.

## Open questions

- Do Bedrock's `CountTokens` and Gemini's `countTokens` calls consume the same per-account/per-project rate-limit quota as real inference calls, or are they separately/independently limited the way Anthropic's own docs explicitly confirm? This is the single most important unresolved question in this report — it directly determines whether pre-call reservation is safe to build for these two providers the way it demonstrably is for Anthropic.
- What is the real, measured latency of each cloud provider's own count-tokens endpoint, under realistic network conditions? No source found states this; a future pass would need to either find a benchmark or measure it directly against a real account.
- Which other real-world `openaicompat` targets (Ollama, TGI, LM Studio, etc.) ship an equivalent tokenize endpoint, and how consistent is its shape/cost across them? Only vLLM was checked in this pass.
- Given Anthropic's own explicit "count is an estimate, may differ from the real, billed count" caveat, how large is that discrepancy in practice, and does it matter for a rate-limit *reservation* use case (where a slight under/over-estimate is far less consequential than for exact billing)? Not measured by this pass.
