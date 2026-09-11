# Gateway Realtime/Bidirectional Streaming Research (2026-09-11)

Scope: whether Kelvran's v2 gateway upgrade needs bidirectional/realtime (WebSocket-style)
streaming support, versus its current unidirectional SSE-only design in
`gateway/internal/streaming` (real SSE across providers, mid-stream runaway-completion guard,
mid-stream budget/TPM reservation top-up, streaming timeout enforcement, and a per-request
circuit breaker composed with streaming fallback that deliberately never retries after a
chunk has reached the client). Externally verified against LiteLLM, Portkey Gateway, Kong,
OpenAI's Realtime/voice-agent docs, Anthropic's Messages streaming docs, and the MCP
Streamable HTTP + MRTR specifications (2026-07-28 revision). 13 claims survived 3-vote
adversarial verification; several stronger derivative claims were explicitly rejected and are
reported below for transparency, since they change how confidently some findings can be
stated.

## Executive Summary

Bidirectional/WebSocket LLM support is real and actively maintained in production gateways
(LiteLLM, Portkey) as of 2026, but it is purpose-built specifically for OpenAI's audio/voice
Realtime API — not a generalized bidirectional text-completion feature — and Kong's own
gateway shows no public evidence of it at all. Kelvran's implicit reference provider,
Anthropic, offers no bidirectional/WebSocket Messages API, and even its stream-interruption
recovery mechanism is built entirely on issuing a brand-new SSE-based HTTP request, reinforcing
a per-request/connection-scoped design at every layer including error handling. MCP — the
protocol closest in spirit to Kelvran's "server needs to interact mid-turn" problem — moved
further away from persistent bidirectional/stateful connections in its most recent
(2026-07-28) revision, explicitly designing its server-to-client mid-turn pattern to avoid
needing shared storage or sticky load balancing. Taken together, the evidence supports
scoping voice/multimodal realtime explicitly OUT of Kelvran v2 (cheap to declare now, avoids
half-building later) while treating general bidirectional support for text-completion traffic
as NOT YET — there is no upstream text-completion-provider protocol driving it, no confirmed
Kelvran customer-demand signal, and the two nearest analogous protocols (Anthropic Messages,
MCP) are both doubling down on the per-request/SSE model Kelvran already ships.

## Findings

### Finding 1 — Bidirectional/WebSocket realtime IS real, shipped, actively-maintained infrastructure in production gateways — but purpose-built for OpenAI's voice Realtime API, not general text-completion streaming
**Confidence: high**

Two of the three major gateway products checked (LiteLLM, Portkey) have genuine,
non-trivial, actively-maintained WebSocket realtime implementations — this is not a
checkbox/vaporware feature at the code level. But every implementation found targets OpenAI's
audio/voice `/v1/realtime` surface specifically; none show a generalized "bidirectional
text-completion" pattern independent of that.

**Verdict: NOT YET** for Kelvran to build a generalized bidirectional text-completion path.
**Trigger:** a concrete customer/use-case requesting OpenAI-Realtime-API (voice) proxying
through Kelvran specifically — absent that, this is competitor-parity pressure, not demand.

Evidence: LiteLLM ships a dedicated `realtime_api` module (`Abstraction / Routing logic for
OpenAI's /v1/realtime endpoints`) with per-provider handler classes for OpenAI, Azure OpenAI,
Bedrock, Vertex AI, and xAI, a `ProviderConfigManager` credential/URL-resolution layer,
guardrail-metadata plumbing, and ephemeral client-secret/transcription-session flows — with
commits as recent as 2026-09-05 and 11 open, real bug reports (Azure AD token handling on
realtime websockets, `credential_name` resolution, SSL on `ws://` health checks). A merged
bug-fix PR (#32077, 2026-07-04) confirms three HTTP realtime endpoints
(`/realtime/client_secrets`, `/realtime/transcription_sessions`, `/realtime/calls`) plus a
pre-existing WebSocket path (`_arealtime`) were already live, in-use features being patched
for enterprise/multi-tenant credential-resolution edge cases (wildcard model groups,
team-scoped models, named credentials) — the bug was that these endpoints bypassed the
router's full credential stack and silently fell through to an empty/unset API key when
lookup failed (`except Exception: pass`). Portkey Gateway's open-source README lists
`Realtime APIs` under its core `Reliable Routing` features (no enterprise-only asterisk),
backed by real shipped code (`src/handlers/realtimeHandler.ts`, `realtimeHandlerNode.ts`,
`websocketUtils.ts`, `services/realtimeLlmEventParser.ts`) wired into live routing
(`app.get('/v1/realtime', realTimeHandler)`), with commit history from 2024-11-22 onward and
Portkey's own `CLAUDE.md` independently confirming the handler-layer implementation.

*Rejected/refuted at this level:* a claim that LiteLLM's five-provider realtime routing
constitutes evidence of broad ecosystem-wide adoption was explicitly refuted (0-3 vote) —
proxying five providers' realtime endpoints demonstrates LiteLLM's own abstraction breadth,
not market-wide demand. A related claim about LiteLLM's Rust `ai-gateway` shipping a
bidirectional WebSocket route was also refuted (1-2), as was a specific connection-setup
latency benchmark claim (0-3) — treat those as unverified, not as supporting evidence.

### Finding 2 — Kong shows no public evidence of bidirectional/realtime support, but this is weak (absence) evidence, not a confirmed architectural stance
**Confidence: medium**

Kong's top-level repo README, despite extensively describing its AI Gateway / LLM-proxying
capabilities (multi-provider "Universal LLM API," ~60 AI features including observability,
semantic caching, semantic routing), contains zero mentions of OpenAI Realtime API, WebSocket
LLM passthrough, bidirectional streaming, or voice/real-time AI support.

**Verdict: NOT YET / inconclusive.** This is a single, absence-only data point (top-level
README only, not Kong's full docs site) and does not establish that Kong lacks the capability
elsewhere, nor that Kong has made any deliberate architectural choice to exclude it — a
stronger claim asserting Kong "positions itself as a unidirectional/request-response gateway"
by design was explicitly refuted (0-3 vote) as an overreach beyond what the evidence shows.
**Trigger for re-check:** none identified; treat as a data point that one of three major
gateways is silent on this feature at the top-level marketing/README layer.

### Finding 3 — Anthropic's Messages API remains exclusively unidirectional SSE, including at the error-recovery layer, with no evidence of any bidirectional offering
**Confidence: high**

Anthropic's Messages API streaming (current docs, referencing `claude-opus-5` and Claude 4.6)
is implemented exclusively via SSE over standard HTTP request/response — zero mentions of
WebSocket or any duplex transport across the entire streaming guide, the API overview, or the
newer stateful Sessions API (which streams via `GET /v1/sessions/{id}/events/stream`, itself
still SSE-style, not a socket). Critically, even Anthropic's own stream-interruption recovery
mechanism requires issuing a brand-new HTTP request that replays the partial response (as a
continuation assistant message for Claude 4.5-and-earlier, or a "please continue" user message
for Claude 4.6+) — there is no resumption over any persistent connection, anywhere in the
documented design.

**Verdict: NOT YET / not applicable** — there is no upstream Anthropic bidirectional protocol
for Kelvran to support today, on its apparent primary/reference provider. **Trigger:**
Anthropic shipping any bidirectional/WebSocket Messages surface (no evidence this exists or is
planned as of 2026-09-11; would warrant an immediate re-scope if it happens).

### Finding 4 — MCP's most recent revision moved further AWAY from persistent/stateful bidirectional connections, not toward them
**Confidence: high**

The current MCP Streamable HTTP transport (spec revision 2026-07-28) is fundamentally
unidirectional-per-request: a single HTTP POST endpoint where each client request gets either
a plain JSON response or an SSE stream scoped to that one request. This revision explicitly
*removed* the standalone GET-based SSE stream and protocol-level sessions (`Mcp-Session-Id`)
that revisions 2025-03-26 through 2025-11-25 had used for server-initiated push — i.e., MCP
had stateful/session machinery and deliberately walked it back. Separately, MCP's Multi
Round-Trip Requests (MRTR) pattern — its mechanism for server-to-client mid-turn interactions
(elicitation, sampling, roots) — is explicitly designed to avoid requiring a shared storage
layer across server instances or stateful/sticky load balancing, replacing the old
server-push approach with independent, stateless request/response round trips keyed by an
opaque client-echoed `requestState` token.

**Verdict: NOT YET** for any MCP-facing bidirectional transport work — the protocol itself
does not require, and is actively moving away from, persistent duplex channels. **Trigger:**
a future MCP spec revision reintroducing session/stream-based server push (no signal of this
as of 2026-09-11; the trend line points the opposite direction).

*Rejected/refuted at this level:* several stronger formulations were checked and did not
survive — that this revision reclassifies legacy HTTP+SSE as formally "Deprecated" (1-2), that
it replaces session state with explicit handles passed as tool arguments (0-3), that it
removes the `initialize` handshake entirely (0-3), and that MRTR is a "breaking change" fully
replacing the old mechanism with no fallback (1-2). The verified, load-bearing fact is narrower
than these: sessions and the GET stream were removed, and MRTR's stated design goal is
avoiding shared state/sticky routing — not the broader claims layered on top.

### Finding 5 — OpenAI's own guidance centers Realtime API on voice-specific interaction needs, but the strongest "explicit exclusion of text completion" framings did not survive verification
**Confidence: medium**

OpenAI's official Realtime API guide recommends the API specifically for "conversational and
immediate" interaction needing barge-in, low first-audio latency, natural turn-taking, and
realtime tool use — i.e., voice-agent framing, not general text-streaming replacement. This
survived verification unanimously. However, three more strongly-worded derivative claims — that
OpenAI "explicitly scopes" Realtime API away from general-purpose text completion, that its
bidirectional/full-duplex framing is defined specifically as audio (not a general
text-streaming concept), and that OpenAI explicitly confines realtime/bidirectional transport
changes to "the audio layer only" with business logic staying identical to text agents — were
all rejected (0-3 each) as overreaching beyond OpenAI's literal published text.

**Verdict: BUILD NOW** to formally, explicitly document voice/multimodal realtime as OUT OF
SCOPE for Kelvran v2 — this is a cheap, low-risk documentation decision, not a code change, and
it is directionally well-supported even though the strongest textual-exclusion claims didn't
hold up: both shipped competitor implementations found (Finding 1) target OpenAI's voice
Realtime API specifically, with no generalized bidirectional text-completion analog found
anywhere in this research pass. **Trigger already met** — the risk of *not* declaring the scope
boundary now is a future half-build under competitive pressure; declaring it costs nothing.

## RQ2 and RQ4 — Kelvran-specific architecture and infra-reuse questions: unresolved by this pass

No confirmed external claim directly analyzes Kelvran's own codebase (`gateway/internal/streaming`)
against bidirectional requirements — auth over a long-lived connection, guardrail scanning of a
stream with no traditional "completion" point, or budget/rate-limit accounting for an
open-ended session rather than a per-request model. These questions require an internal
codebase pass, not external landscape research, and are listed as open questions below. The
closest transferable signal from this research is indirect but consistent: MCP's own design
intent (Finding 4) is to avoid exactly this class of complexity (shared state, sticky routing,
session lifecycle) by staying per-request, and Anthropic's error-recovery design (Finding 3)
achieves resilience via new-request-replay rather than in-place session resumption — both
precedents argue for extending Kelvran's existing per-request SSE resilience machinery
(runaway guard, mid-stream reservation top-up, streaming timeout, circuit breaker) rather than
building a parallel duplex-session architecture, if and when a concrete trigger appears.

## Open Questions

- Does Kelvran have any actual customer/use-case demand for OpenAI's voice Realtime API
  specifically, or is this purely speculative competitor-parity pressure (LiteLLM/Portkey both
  have it)?
- If Kelvran did add a narrow, explicitly-scoped OpenAI-Realtime-API voice proxy path, would it
  reuse the existing per-request budget/guardrail/circuit-breaker machinery in
  `gateway/internal/streaming`, or does a long-lived duplex session require materially new
  abstractions? This needs an internal codebase pass, not covered by this research.
- Will Anthropic ever ship a bidirectional Messages API? No current evidence either way as of
  2026-09-11 — worth a time-boxed recheck at Anthropic's next major model/API release.
- Now that MCP 2026-07-28 removed protocol-level sessions and the standalone GET stream
  entirely, does any of Kelvran's MCP-transport-facing code (if any exists) need updating to
  match the new Streamable HTTP shape — independent of, and regardless of the outcome of, the
  realtime-streaming question?

## Caveats

Portkey and LiteLLM sources are vendor-controlled (GitHub READMEs/docs) though corroborated
directly against code and commit history, which meaningfully raises confidence beyond a bare
marketing claim. The Kong absence-of-mention finding is weak (top-level README only, not the
full docs site) and a stronger claim about Kong's deliberate architectural positioning was
explicitly rejected — do not over-read Kong's silence as a confirmed design stance. Several of
the strongest-sounding claims about OpenAI's and MCP's "explicit" scoping/deprecation language
did not survive adversarial re-verification against the literal source text; the surviving
claims are narrower than the rejected ones, and this report deliberately reports both so the
distinction isn't lost in a later summary pass. This is a text-completion-gateway research
question — voice/multimodal realtime is a genuinely different engineering problem (audio
codecs, turn-taking, barge-in) that this report does not attempt to design for, only to scope
in/out. Time-sensitivity: LiteLLM/Portkey realtime code is under active 2026 development
(commits as recent as 2026-07-04 through 2026-09-05); the MCP spec revision cited (2026-07-28)
is current as of this writing but MCP has changed its transport shape multiple times in the
prior ~18 months, so re-verify before treating any of this as stable long-term footing.
