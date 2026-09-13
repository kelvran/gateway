# Realtime / Bidirectional Streaming and Multimodal Support — Research Report

**Date:** 2026-09-13
**Scope:** `gateway/internal/streaming/`, `gateway/internal/adapter/*/`, `gateway/internal/gateway/dataplane/`
**Method:** 3-vote adversarial verification against primary vendor sources (OpenAI, Google, LiteLLM, Kong, AWS); 20 of 25 candidate claims survived verification.

---

## Executive Summary

Realtime, bidirectional, voice-native LLM APIs are real, current (2026), and shipping from all three frontier labs' gateway-adjacent surfaces: OpenAI's Realtime API / GPT-Live-1 (WebSocket or WebRTC, full-duplex audio), and Google's Gemini Live API (stateful WebSocket, concurrent text/audio/video in, audio/text/tool-call out). This is **not** uniformly unsupported in the gateway-vendor landscape — LiteLLM proxy, Kong AI Gateway, and (per AWS's own WebSocket API Gateway primitive) the infrastructure layer all support WebSocket-based realtime/voice proxying today, with LiteLLM in particular shipping protocol-translating implementations for OpenAI, Azure (GA and beta schema variants), xAI, Gemini Live/Vertex AI, and Bedrock. Architecturally, Kelvran's current pipeline — one HTTP request → one adapter call → one response/stream, with cache/guardrail/budget checks keyed to that single request-response shape — is fundamentally incompatible with these session models, which require a persistent bidirectional connection, per-turn (not per-request) guardrail checks, and session-resumption state management across multiple underlying WebSocket connections per logical session. However, peer gateways demonstrate this is an **incremental, additive path** (a new transport layer bolted alongside the existing HTTP pipeline), not a full rewrite — LiteLLM's realtime endpoint is a separate `/v1/realtime` WebSocket route with its own guardrail hook model, not a retrofit of the chat-completions request/response path.

---

## Findings

### Finding 1 — Realtime/voice APIs are WebSocket-based, stateful, and bidirectional by design (not request/response)
**Confidence: high** (5 unanimous/near-unanimous primary-source claims, both OpenAI and Google)

- OpenAI's Realtime API (WebSocket transport for server-to-server) carries audio and JSON events in **both directions** over a single persistent connection: audio in via `session.input_audio.append`, audio out via `session.output_audio.delta` (claim 0).
- OpenAI's Realtime API also supports WebRTC as the recommended browser transport, with WebSocket reserved for server-side use — it is not exclusively WebSocket as commonly assumed (claim 16).
- Realtime API sessions are long-lived and stateful: a single session persists across multiple audio turns, tool calls, interruptions, and handoffs — not a discrete one-shot exchange (claim 17).
- Gemini Live API is explicitly documented as "a stateful API that uses WebSockets" — one connection establishes one persistent bidirectional session, not an HTTP request/response model (claims 1, 3).
- Within that one session, a client streams text/audio/video concurrently and receives audio/text/function-call-requests back — genuine bidirectional multimodal streaming (claim 2), a categorical difference from Kelvran's confirmed unidirectional SSE (text/tool-call/reasoning deltas only).
- OpenAI's newest generation, GPT-Live-1 (API access opened 2026-09-10, three days before this report), is explicitly full-duplex — it can listen and speak simultaneously, and architecturally delegates deeper reasoning/tool-calling to a separate backend text model (e.g. GPT-6 Astra) rather than being one monolithic call (claims 18, 19). This further confirms the voice/session layer and the reasoning layer are decoupled in the vendor's own architecture — relevant if Kelvran ever proxies this, since the reasoning-delegation hop could in principle still route through an HTTP-shaped adapter even though the voice layer cannot.
- The Realtime API's core differentiator vs. chained Chat-Completions-style pipelines is that **one model** processes and generates audio directly, rather than chaining separate STT → LLM → TTS calls (claim 15) — meaning a "realtime" feature cannot be built by simply wiring Kelvran's existing text-completion adapters to a speech front-end; the vendor-side model itself is architected differently.

**build_now / not_yet:** `not_yet`. Trigger: a customer/product requirement for voice or video agent proxying through Kelvran. Until then, this is upstream-vendor capability with no inbound demand signal in Kelvran's current corpus.

---

### Finding 2 — Live sessions have hard duration caps and forced reconnects that make naive pass-through unworkable
**Confidence: high** (unanimous, single strong primary source, internally cross-corroborated)

- Gemini Live sessions cap at 15 minutes (audio-only), 2 minutes (audio-video), and the underlying WebSocket connection itself is capped at ~10 minutes — all without additional configuration (claim 4).
- To survive server-initiated periodic resets, the client must persist a server-issued **resumption token/handle** (valid 2 hours after last session termination) and use it to open a **new** WebSocket connection that continues the same logical session (claim 5).
- Net effect: any component proxying this — including a gateway — must manage state across **multiple physical connections per one logical session**, which is categorically different from Kelvran's one-request-in/one-response-out cycle.

**build_now / not_yet:** `not_yet`. Trigger: if/when Finding 1's trigger fires, this finding defines the *specific* engineering requirement (a session-resumption-token store, keyed independently of any single HTTP request) that must be designed before any Gemini-Live proxying is attempted.

---

### Finding 3 — At least one mainstream peer gateway (LiteLLM) already proxies realtime/voice sessions today, across multiple vendors, with real protocol translation
**Confidence: high** (unanimous across 5 claims, corroborated by both docs and source code)

This directly refutes a "uniformly unsupported across the gateway-vendor landscape" hypothesis:

- LiteLLM proxy exposes `ws://<host>/v1/realtime?model=<model>` and load-balances realtime voice/audio sessions across OpenAI, Azure, xAI, Google AI Studio (Gemini), Vertex AI, and Bedrock (claim 6). This is corroborated at the source-code level: dedicated `litellm/llms/<provider>/realtime/` handler modules per vendor, e2e test suites, and even a Rust reimplementation in progress — this is a maintained, non-trivial feature, not a stub.
- Azure support requires genuine **live protocol translation** between two incompatible Azure realtime schemas (GA vs. beta — differing `session.type`/`output_modalities` vs. `modalities`/`voice` field shapes), selected by client header, deployment config, or env var, with field remapping applied *during* the bidirectional forwarding loop, not just at connection setup (claim 7). This is concrete evidence that realtime proxying requires materially more protocol-aware logic than a thin pass-through.
- LiteLLM also proxies Google Vertex AI's Gemini Live (`BidiGenerateContent`) by translating it into OpenAI's Realtime wire format at the proxy layer, with documented PCM16 sample-rate handling (16kHz mic in / 24kHz speaker out) and full duplex voice+text+server-VAD support (claims 8, 9, 10).
- Realtime proxying forced LiteLLM to build a **fundamentally different guardrail model**: instead of one check per HTTP request/response, it runs a check on every conversational turn inside one long-lived connection, hooked to the `conversation.item.input_audio_transcription.completed` event rather than to request completion (claim 11). This is the single most directly relevant finding for Kelvran's own architecture, since Kelvran's guardrail/cache/budget checks are today keyed to the request-response shape exactly as LiteLLM's used to be.
- Kong AI Gateway (v2.0+) also supports OpenAI's Realtime API as a first-class capability, proxying `/realtime` to upstream `/v1/realtime` over WebSocket, explicitly marketed as bidirectional streaming for realtime applications (claims 12, 13) — a second independent peer-gateway confirmation.
- At the infrastructure layer below any of these proxies, AWS API Gateway's WebSocket API primitive is explicitly stateful and supports two-way communication (backend-initiated push via a separate `@connections` management API), contrasted directly against the request-response REST API model (claim 14) — the low-level building block any Go-based gateway (including Kelvran) would need if building this itself rather than reverse-proxying to a vendor's WebSocket endpoint.

**build_now / not_yet:** `not_yet`, but with a concrete precedent to copy if the trigger fires. Trigger: same as Finding 1 (customer demand for voice/realtime proxying) — at that point, LiteLLM's architecture (separate `/v1/realtime` route, per-turn guardrail hook, resumption-token state) is the reference design to adapt for Kelvran rather than researching from scratch.

---

### Finding 4 — Kelvran's current pipeline shape is incompatible with these session models, but the incompatibility is additive/orthogonal, not a full-pipeline rewrite
**Confidence: medium** (this is the report's own architectural synthesis, grounded in the verified findings above plus direct inspection of Kelvran's code — not an externally-sourced claim)

Ground truth re-confirmed in this codebase during this research pass:
- `gateway/internal/streaming/types.go` implements only `SSEEvent`, `ChatCompletionChunk`, `ToolCallDelta`, `ReasoningDelta`, and a `StreamDecoder` interface for **provider-native SSE** framing (OpenAI/Anthropic-style; Bedrock's `application/vnd.amazon.eventstream` is handled by a separate decoder implementing the same one-way `SSEEvent`-shaped contract). There is no WebSocket type, no bidirectional session type, anywhere in this package.
- `gateway/internal/adapter/openai/openai.go`'s `contentToNative` explicitly handles only `"image"` (mapped to `image_url`) and rejects `"document"` with a typed error (`"openai: document content parts are not supported by the Chat Completions API"`). There is no `"audio"` or `"video"` case at all.
- `gateway/internal/gateway/dataplane/` contains ~30+ files each modeling one request-scoped concern (budget, cache, guardrail, fallback, health-probe, rate-limit) — every one of them is written against a single inbound request and its (possibly streamed, but still terminating) response.

Given Finding 3's evidence that LiteLLM and Kong solved this by adding a **separate WebSocket route with its own guardrail/session model**, rather than retrofitting their existing chat-completions pipeline, the realistic incremental path for Kelvran would be:
1. A new transport entry point (WebSocket upgrade handler) alongside, not inside, the existing HTTP handler — this is additive, low-risk to the existing pipeline.
2. A new session-scoped state store for connection lifecycle (open/reconnect-with-resumption-token/close), decoupled from the existing per-request budget/cache/guardrail checks.
3. A new per-turn guardrail hook analogous to LiteLLM's `conversation.item.input_audio_transcription.completed`, since Kelvran's existing guardrail checks are wired to full-request completion.
4. Cache and budget semantics would need genuinely new design (not reuse): Kelvran's cache is keyed on a complete, hashable request; a multi-turn voice session has no single hashable "request," and budget/cost accounting would need to move from a per-call debit to a per-turn or time-based debit (paralleling OpenAI GPT-Live-1's $/minute pricing model referenced in claim 18's corroborating sources).

This is a real, executable incremental path — not a "fundamentally different transport layer requiring a full rewrite" in the sense of touching the existing HTTP dataplane files. But it is also not a small feature: items 2–4 are each non-trivial new subsystems with no existing Kelvran analog to extend.

**build_now / not_yet:** `not_yet`. Trigger: (a) explicit customer/product requirement for voice or realtime multimodal proxying, AND (b) a decision on cost-accounting model for non-per-call billing (dependency for item 4 above) — without that pricing-model decision, building the transport layer alone would ship half a feature.

---

## Caveats

- **Source skew toward vendor docs.** Nearly every confirmed claim is sourced from the vendor's own documentation (OpenAI, Google, LiteLLM, Kong, AWS) rather than independent/adversarial third-party analysis. This is appropriate for descriptive/architectural claims (how does X work) but means performance, reliability, and cost claims from these vendors were not independently stress-tested in this research pass.
- **Time-sensitivity is high.** GPT-Live-1 opened to developers only 3 days before this report (2026-09-10); the Gemini Live API is still labeled "Preview"; and LiteLLM's realtime feature set is under active development (a Rust rewrite is in progress, and an open PR was found fixing a duplicate-response bug in the same subsystem). Figures like session duration caps, pricing, and even which providers are supported are likely to shift within months.
- **Portkey claim was refuted.** A candidate claim that Portkey (the vendor project some gateways fork from) supports Realtime API proxying via an integrated WebSocket server did **not** survive verification (1-2 vote) — do not assume Portkey parity with LiteLLM/Kong on this specific capability without separately re-verifying.
- **Finding 4 is this report's own synthesis**, not an externally-sourced claim — it is grounded in direct inspection of Kelvran's current code plus the verified peer-gateway evidence, but the specific implementation shape (new WebSocket route, new session store, new per-turn guardrail hook) is a design recommendation, not a fact independently verified by a third party.
- **Two refuted-but-relevant claims are worth remembering as negative evidence**, not just discarding: (1) OpenAI's Realtime WebSocket protocol claim about explicit `session.start`/`session.close` handshake events was refuted (1-2) — session lifecycle framing may differ from that specific characterization; (2) the claim that Vertex AI's `BidiGenerateContent` only accepts one setup message per connection (so LiteLLM silently drops `session.update`/`response.create`) was refuted (1-2) — treat LiteLLM's Vertex-Realtime limitations as less certain than the core proxying capability itself.

## Open Questions

1. What is Kelvran's actual product/customer demand signal for voice or realtime multimodal support — is this purely speculative research, or is there a concrete roadmap item driving it? (This gates every `build_now` vs `not_yet` call above.)
2. If built, should Kelvran build its own WebSocket-to-vendor-WebSocket proxy (LiteLLM/Kong's approach) or instead front vendor WebRTC/WebSocket endpoints directly from clients and only handle auth/budget/guardrail out-of-band (a lighter-weight "broker" model)? The research didn't evaluate this design tradeoff.
3. How would Kelvran's cost/budget accounting model (currently per-call, tokens-based) need to change for time-based ($/minute) realtime billing, and does that require schema changes to existing budget/ledger tables independent of the streaming-transport question?
4. Does Kelvran's guardrail engine have an extensibility point today that could support a "per-turn" hook analogous to LiteLLM's transcription-completed hook, or would that require a parallel guardrail pipeline entirely separate from the existing request-scoped one?
