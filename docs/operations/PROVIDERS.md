# Provider & Data-Flow Inventory

Maintained as a manifest kept current with Gateway's routing config, not as hand-authored prose left to go stale — treat any provider entry below as needing a matching entry in `gateway/internal/adapter/` before it's real. Linked from `SECURITY.md`'s Provider & Data-Flow Inventory section for the same reason a threat model needs to know exactly what leaves the system and where.

| Provider | Data classes sent | Auth mechanism | Data-residency notes |
|---|---|---|---|
| OpenAI | Full prompt/completion content, tool-call arguments, embeddings (if used for L3 semantic cache) | API key (BYO or Gateway-issued virtual key mapped to a pooled/dedicated upstream key) | US-hosted by default; no residency guarantee unless the customer's own OpenAI org has one |
| Anthropic | Full prompt/completion content, tool-call arguments | API key (BYO or virtual key) | US-hosted by default |
| Google Gemini / Vertex | Full prompt/completion content, tool-call arguments | API key or GCP service-account credentials | Vertex AI supports regional deployment; plain Gemini API does not guarantee residency |
| AWS Bedrock | Full prompt/completion content via `Converse`/`ConverseStream`, tool-call arguments | AWS IAM credentials / role assumption | Region-pinned by the customer's own Bedrock configuration |
| Self-hosted (vLLM / TGI / Ollama, OpenAI-compatible) | Full prompt/completion content | Operator-defined (often none, or a shared bearer token) | Fully under the operator's control — no third-party data flow at all |
| Anthropic (Evals judge/panel) | Judge prompts (candidate `output`/`reference` text, embedded in the prompt), judge verdict/rationale text | `ANTHROPIC_API_KEY` (`evals run --llm-judge`'s non-Bedrock leg) | US-hosted by default |
| OpenAI (Evals judge, reserved) | Same shape as the Anthropic judge row | `OPENAI_API_KEY` — `make_openai_call_model()` exists but has no real caller today (the panel moved to Bedrock); reserved for a future caller | US-hosted by default |
| AWS Bedrock (Evals judge/panel) | Same shape as the Anthropic judge row, via `Converse` | AWS IAM credentials (`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_REGION`) — the live default `--llm-judge`/`--llm-judge-panel` path | Region-pinned by the customer's own Bedrock configuration |

## Notes

- **Embeddings for semantic caching** are a distinct data flow from the completion call itself — whichever embedding provider/model is used for L3 cache matching must be listed here once chosen (currently unresolved, per `PRD.md`'s open-questions note on the semantic-cache risk model). The offline embedding-gate validation harness (`evals/scripts/validate_embedding_gate.py`, a one-off research script, never production code) does call AWS Bedrock Titan embeddings directly — not listed as its own row since it's not a shipped capability.
- **No provider receives data it wasn't the target of.** Cache never forwards a cached response's origin-provider content to a different provider — cross-provider cache reuse is explicitly out of scope (see `THREAT_MODEL.md`'s Cache STRIDE table on why this is a correctness risk, not just a caching nicety).
- **Corrected 2026-09-11**: the "zero entries for Evals by design" claim immediately below this note (in every version of this file until now) was false since `evals/evals/judge/providers.py` first shipped a real, direct provider call (2026-09-04) — this file's own header claims it is "maintained as a manifest kept current," but `git log` shows it was never touched since the initial scaffolding commit. Evals genuinely does call Anthropic/OpenAI/Bedrock directly for LLM-judge scoring, independent of Gateway's own request path entirely — the 3 new rows above are that real data flow, added for the first time.
