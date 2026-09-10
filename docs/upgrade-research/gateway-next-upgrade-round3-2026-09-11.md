# Gateway Next-Upgrade Scan — Deep Research, Round 3 (2026-09-11)

*(This file was reconstructed from the completed workflow's structured result after the underlying run did not itself write the file to disk — the content below is the verified research output verbatim, not re-derived.)*

## Question

Now that `gateway/v0.4.0` is tagged and every finding from both prior rounds has shipped — what is the single feature that would most change a prospective operator's decision to adopt Kelvran, and are there genuinely new, well-precedented BIG value-add opportunities (not incremental bug fixes) for the gateway, comparing against 2026 production practice in LiteLLM, Kong AI Gateway, Envoy AI Gateway, Portkey, Helicone, TensorZero, and Bifrost?

## Context (Kelvran's own real, current state)

Adapters for OpenAI/Anthropic/Gemini/Bedrock/openaicompat; weighted routing; multi-hop fallback chains; active health probing; per-key/per-deployment concurrency + RPM/TPM limits; atomic budget Reserve/Reconcile; a runaway-completion guard; Guardrails v1; a 3-layer embedded cache with a `SharedAcrossTenants` flag, TTL jitter, and cache-token-inclusive cost accounting; a 2-tier Admin API; real OTel tracing/metrics. Both prior rounds' findings are fully shipped.

## Summary

The single highest-value, ready-to-build gap is **cross-provider structured-output/JSON-schema normalization**: as of 2026 all three major first-party providers (OpenAI, Anthropic, Gemini) ship native schema-enforced structured output via three different API shapes, at least two competing gateways (LiteLLM, Requesty) already normalize this behind one request shape (with a client-side jsonschema-validation fallback for providers lacking native support), and Kelvran currently has zero equivalent — this is now baseline-expected, well-precedented, and slots cleanly into Kelvran's existing per-provider adapter translation layer and multi-hop fallback chains (Finding 1). A second, slightly less mature but real finding: server-side prompt/template management (`prompt_id` + `prompt_version` + `prompt_variables`, resolved at the gateway) is now shipped by two independent competitors (LiteLLM beta, Helicone GA/13+ months live) with a converging API shape, making it a reasonable build-now, scope-managed v1 addition on top of Kelvran's existing Admin API (Finding 2). Two other candidate areas were re-checked per the research brief and both reaffirm prior "not yet" verdicts rather than triggering: MCP/A2A brokering saw genuinely new competitive activity in the last few months (Kong GA'd a third Agent Gateway mode with A2A policy/guardrail enforcement; LiteLLM shipped a major MCP-gateway security upgrade 9 days before this research) but neither the required user-demand signal nor the missing non-token cost-accounting unit has appeared, so the trigger has not fired (Finding 3); multi-provider cost-optimization routing has no genuinely new 2026 pattern — Kong's route-by-model-in-body feature is real but is client-declared alias routing, not autonomous quality-bar-aware routing, so it doesn't add anything beyond Kelvran's existing weighted routing (Finding 4).

## Findings

### Finding 1 — BUILD NOW: cross-provider structured-output/JSON-schema-enforcement normalization (confidence: high)

All three major first-party providers now natively support schema-enforced structured output via different API shapes: OpenAI's `response_format`/`text.format` with `json_schema`; Anthropic's `output_config.format` using grammar-constrained decoding (GA since Jan 29, 2026), plus an independently-composable `strict: true` tool-input-validation surface sharing the same grammar mechanism; Gemini's `response_schema`+`response_mime_type`. LiteLLM normalizes this behind a single `response_format` param plus a client-side jsonschema validation fallback for models/providers lacking native support, and Requesty markets one OpenAI-compatible schema request translated per-provider — including across fallback/model-switch routing — as a core differentiator. This maps directly onto Kelvran's existing per-provider adapter translation layer and multi-hop fallback chains. Aggregates 7 independently 2-1 or 3-0 verified claims against primary vendor docs (Anthropic, LiteLLM, Requesty), cross-corroborated by AWS Bedrock docs confirming Anthropic's shared grammar mechanism for JSON-output and strict-tool-use.

**Caveat**: Requesty's own benchmark data shows real-world structured-output pass rates are NOT uniform across providers/endpoints (native-endpoint 80-93%, SDK-level as low as 50-75%, outright failures on some native endpoints for DeepSeek and Novita) — a normalization layer reduces but does not eliminate cross-provider variance; don't oversell it as "always works."

### Finding 2 — BUILD NOW (scope-managed v1): server-side prompt/template management (confidence: medium)

`prompt_id` plus optional `prompt_version` and `prompt_variables`, resolved at the gateway rather than requiring inline prompt text in application code, with automatic version history (v1, v2, v3 on every edit) and pinning to a specific historical version. Two independent competitors ship nearly identical API shapes: LiteLLM (labeled Beta) and Helicone (GA, live 13+ months, framed by its own docs as gateway-side compilation rather than SDK/observability tooling). **Recommendation**: scope v1 to `prompt_id`/`version`/`variables` plus Admin-API CRUD on Kelvran's existing 2-tier Admin API, deferring prompt experimentation/A-B routing which even the category leaders haven't shipped. LiteLLM's own Beta label means this is real but less mature than Finding 1.

### Finding 3 — NOT YET (reaffirmed, new signal logged): MCP/A2A outbound brokering (confidence: medium)

New evidence since the prior two rounds: LiteLLM v1.100.0 (Sept 6, 2026) shipped RFC 7662 token introspection, RS256-signed session tokens, bulk import of Anthropic MCP connectors, and per-team/org/user toolset enforcement. Kong AI Gateway reached GA (v3.14, April 2026) on a third gateway mode, Agent Gateway, applying the same auth/rate-limit/observability policies to A2A traffic as to LLM traffic plus real-time inspection for policy violations, prompt-injection attempts, and anomalous behavior, backed by a multi-release-maintained plugin. **Neither named trigger for un-deferring (a real user asking for it; a cost-accounting unit for non-token tool calls) has appeared, so the verdict does not flip** — treat as rising competitive pressure worth a shorter re-check interval than the other deferred items.

### Finding 4 — NOT YET (reaffirmed): multi-provider automatic cost-optimization routing beyond static weights (confidence: medium)

Kong's April 2026 route-by-model-in-body feature is real and shipped — it inspects the request body's `model` field and routes to a config-defined alias (e.g. `cheap` maps to self-hosted Llama, `powerful` maps to GPT-4o) — but this is client-declared alias routing, not autonomous quality-bar-aware cost optimization. No genuinely new 2026 pattern for automatic cheapest-provider-that-meets-a-quality-bar routing was found beyond what prior rounds already ruled out; this is a more ergonomic addressing convention over the same static-weighted-deployment pattern Kelvran already has. The verifier explicitly flagged that calling this equivalent to automatic quality-bar routing would be an overreach.

## Caveats

- Several primary sources are vendor blogs/marketing pages (Kong, Requesty, Helicone) rather than neutral third parties — verified against changelogs, independent primary docs, or cross-vendor corroboration where possible, but self-reported effectiveness claims should be discounted.
- Both the Kong Agent Gateway GA and the LiteLLM MCP-security release are very recent (within ~5 months; the LiteLLM one only 9 days before this research date) — re-verify maturity and field adoption in a future round rather than assuming durability.
- Several adjacent single-source claims were explicitly REFUTED during verification and must not be carried into any spec derived from this report: LiteLLM's universal-`response_format`-across-all-listed-providers claim (0-3); Helicone's gateway-compilation-preferred-specifically-for-latency framing (1-2); environment-param-replaces-prompt-versioning (0-3); a third-party blog's characterization of Kong's Agent Gateway scope (0-3); OpenAI-made-strict-mode-default-plus-Responses-API-field-name-split (0-3); Gemini-silently-drops-unsupported-schema-keywords (0-3); specific uniform pass-rate percentages (0-3); Google-added-Pydantic/Zod-support-Nov-2025 (0-3); OpenAI-token-level-decoding-vs-legacy-JSON-mode-as-deprecated (1-2, split vote).

## Recommendation for Kelvran

1. **Build now**: cross-provider structured-output/JSON-schema normalization (Finding 1) — the single highest-value, best-precedented addition this round.
2. **Build now, scope-managed v1**: server-side prompt/template management (Finding 2) — CRUD on the existing Admin API, deferring experimentation/A-B routing.
3. **Not yet, reaffirmed but watch more closely**: MCP/A2A brokering (Finding 3) — competitive pressure is rising, but neither named trigger has fired.
4. **Not yet, reaffirmed**: autonomous cost-optimization routing (Finding 4) — nothing genuinely new found.

## Open Questions

- Does Kelvran's adapter layer already have a per-provider request-shape translation hook that a structured-output normalization layer could reuse, or does this require new plumbing across all five provider adapters?
- For prompt management, should prompt resolution happen before or after cache-key computation — should a `prompt_id`/`version` change bust the cache while variable substitution with identical resolved inputs still hits it?
- Is there any concrete operator or user demand signal yet for MCP/A2A brokering, or is the remaining blocker still purely that no one has asked — this is the one fact that would flip the reaffirmed NOT YET verdict?
- Should Kelvran's structured-output layer also cover the strict-tool-use surface (schema-validated tool-call arguments), given Kelvran's existing guardrails already inspect tool-call arguments for PII and injection — is there a natural single integration point for both concerns?
