# Provider & Data-Flow Inventory

Maintained as a manifest kept current with Gateway's routing config, not as hand-authored prose left to go stale — treat any provider entry below as needing a matching entry in `gateway/internal/adapter/` before it's real. Linked from `SECURITY.md`'s Provider & Data-Flow Inventory section for the same reason a threat model needs to know exactly what leaves the system and where.

| Provider | Data classes sent | Auth mechanism | Data-residency notes |
|---|---|---|---|
| OpenAI | Full prompt/completion content, tool-call arguments, embeddings (if used for L3 semantic cache) | API key (BYO or Gateway-issued virtual key mapped to a pooled/dedicated upstream key) | US-hosted by default; no residency guarantee unless the customer's own OpenAI org has one |
| Anthropic | Full prompt/completion content, tool-call arguments | API key (BYO or virtual key) | US-hosted by default |
| Google Gemini / Vertex | Full prompt/completion content, tool-call arguments | API key or GCP service-account credentials | Vertex AI supports regional deployment; plain Gemini API does not guarantee residency |
| AWS Bedrock | Full prompt/completion content via `Converse`/`ConverseStream`, tool-call arguments | AWS IAM credentials / role assumption | Region-pinned by the customer's own Bedrock configuration |
| Self-hosted (vLLM / TGI / Ollama, OpenAI-compatible) | Full prompt/completion content | Operator-defined (often none, or a shared bearer token) | Fully under the operator's control — no third-party data flow at all |

## Notes

- **Embeddings for semantic caching** are a distinct data flow from the completion call itself — whichever embedding provider/model is used for L3 cache matching must be listed here once chosen (currently unresolved, per `PRD.md`'s open-questions note on the semantic-cache risk model).
- **No provider receives data it wasn't the target of.** Cache never forwards a cached response's origin-provider content to a different provider — cross-provider cache reuse is explicitly out of scope (see `THREAT_MODEL.md`'s Cache STRIDE table on why this is a correctness risk, not just a caching nicety).
- **This table has zero entries for Evals** by design — Evals never calls a model provider directly in its v1 scope; it calls Gateway's own API, so the provider-facing data flow is entirely Gateway's, not duplicated here.
