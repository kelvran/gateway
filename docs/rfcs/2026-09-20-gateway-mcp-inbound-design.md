# RFC: MCP inbound server — design only, no code

## Status

Design-only, explicitly deferred pending its own named trigger. Mirrors the exact "designed in
full, deferred" precedent already established for the Redis-backed cache
(`docs/rfcs/2026-09-11-gateway-redis-backed-cache-design.md`) and for the outbound half of this
same subsystem (`docs/rfcs/2026-09-11-gateway-mcp-outbound-credential-design.md`, this RFC's direct
sibling): scope the "how" now, without committing to the "when" — MCP/A2A brokering itself remains
genuinely unbuilt (`gateway/internal/mcp` has zero code, confirmed via grep this session, and
reconfirmed by `docs/upgrade-research/mcp-a2a-brokering-tier1-2026-09-20.md`'s own fresh check),
and this RFC does not change that.

## Date

2026-09-20

## Author(s)

Session agent (Claude Code), per this round's Tier-2 backlog item "inbound-only MCP server."

## Summary

`gateway/ARCHITECTURE.md`'s MCP/A2A Subsystem section has, since 2026-09-07, named two directions
for a future MCP surface: **inbound** (expose Kelvran's own APIs as MCP tools, so an external MCP
client can call INTO Kelvran) and **outbound** (broker agent tool calls FROM a Kelvran-routed
request TO a downstream MCP server or model provider on the caller's behalf). The outbound half
already has a design-only RFC (the sibling document above), covering the credential-delegation
question that direction raises. This RFC covers the mirror-image inbound half: how Kelvran would
expose a minimal MCP server on top of its own existing gateway process, sharing — never
duplicating — the exact same `identity.Verifier` authentication and `costaccounting.Calculator`
pricing every ordinary HTTP request already goes through, per `ARCHITECTURE.md`'s own stated
intent ("Shares Gateway's own auth/budget/audit objects rather than being a second gateway with a
second config source").

## Motivation

`docs/upgrade-research/mcp-a2a-brokering-tier1-2026-09-20.md` — the third deep pass on this exact
question (following 2026-09-07 and 2026-09-11 predecessors) — reconfirms zero recorded demand
signal for MCP/A2A brokering of either direction, and its own top-line recommendation is explicit:
MCP/A2A brokering stays correctly out of scope for v1. This RFC does not overturn that. What it
does is capture that same research's own concrete, code-grounded scoping of *which slice to start
with, if a demand signal ever does justify starting*:

> "a minimal inbound MCP server exposing Kelvran's existing chat-completions capability as a
> single MCP tool, admitting callers through the existing `identity.Verifier` bearer-token check
> exactly as-is, and pricing every resulting completion through the existing
> `costaccounting.Calculator` exactly as-is... this needs zero new credential model... and zero new
> cost-accounting primitive."

That finding is itself the third independent research pass to land on "inbound is the tractable
direction" (the 2026-09-07 report first suggested it; 2026-09-11 treated it as a working
assumption; 2026-09-20 confirmed it directly against the real, current code rather than by
analogy). Writing this design down now — mirroring the outbound RFC's own "de-risk the how, not
the when" discipline — means a future implementer starts from a code-grounded plan rather than
re-deriving one from scratch, or reaching for the same "just reuse whatever's nearest" shortcut the
outbound RFC's own Motivation section warns against.

## Detailed Design

### Scope: one tool, zero new identity concepts, stateless transport

The 07-28 MCP spec revision (confirmed still current, per the 2026-09-20 research pass's Finding
1) uses a stateless, no-handshake request model — a materially smaller integration surface than
the pre-07-28 session-stateful design, and a good fit for Kelvran's dataplane, which is already a
stateless-per-request Go HTTP pipeline with no sticky-session infrastructure to bridge. This RFC
scopes the smallest useful inbound surface, not a general-purpose MCP-tool-catalog framework:

- **One tool**: `chat_completion` (naming TBD at implementation time), whose input schema mirrors
  `adapter.ChatRequest`'s already-canonical shape (model, messages, max_tokens, tools, etc.) and
  whose output is the resulting `adapter.ChatResponse`, serialized as the MCP tool result. No
  second tool, no resource catalog, no prompt-template exposure via MCP (Kelvran's own
  `internal/prompt` already has an HTTP Admin API for that; duplicating it as an MCP resource is
  explicitly out of scope for this first slice).
- **Transport**: HTTP, not stdio — Kelvran is already a long-running HTTP service with a real
  listener (`cmd/gateway/main.go`), and an inbound MCP client (an agent framework, not a human at a
  terminal) is the expected caller; stdio's process-per-session model has no natural fit here. The
  MCP spec's Streamable HTTP transport (the mechanism the 07-28 revision's stateless model is built
  around) is the target, mounted as its own route (e.g. `POST /mcp`) alongside the existing
  `/v1/chat/completions` and `/v1/embeddings` routes in `cmd/gateway/main.go`'s mux — a new route
  registration, not a new listener or a second config source.
- **Auth**: an MCP client authenticates with a bearer token exactly as any other gateway caller
  does today — the token is checked against `identity.Verifier` and resolves to the SAME
  `VirtualKey` a `/v1/chat/completions` caller would resolve to. There is no new "MCP client"
  identity concept, and — critically, per the research's own framing — no OAuth 2.1 dependency:
  MCP's own spec-recommended authorization model layers OAuth 2.1 on top of the transport, but that
  is a recommendation for MCP servers exposed to untrusted/third-party clients on the open internet,
  not a requirement, and it is the wrong fit here regardless: Kelvran's own bearer-token model
  already IS the access-control mechanism for this exact surface today, and OAuth 2.1 itself
  remains an unratified IETF Internet-Draft (still revision -16 as of this research pass) — a
  design dependency worth avoiding for this reason alone even if there were appetite to adopt it.
- **Budget/rate-limit/audit/cache**: every one of these already-real enforcement mechanisms is
  keyed off the resolved `VirtualKey` and the request's own model/token counts — none of them
  changes shape for an MCP-originated call. A tool invocation resolves to a real
  `dataplane.Pipeline.HandleChatCompletion` call under the hood (the same function
  `/v1/chat/completions`'s own handler calls), so budget enforcement, rate limiting, cache
  lookup/write, guardrail scanning, and the audit log all apply identically and automatically — an
  MCP tool call is a real request against real infrastructure, never a side door around any
  existing control. This is the direct, mechanical consequence of "share the identity/costaccounting
  objects" rather than a new design decision this RFC has to make.
- **Cost accounting**: unchanged. A chat completion invoked via the MCP tool still produces the
  exact same token-priced `adapter.Usage` Kelvran already bills through
  `costaccounting.Calculator.Calculate(model, usage)` today. There is no new "per-tool-call" cost
  unit to invent, because this slice deliberately does not touch any capability whose cost isn't
  already token-priced (see Alternatives Considered for why a broader admin-capability surface is
  deferred).

### New package: `gateway/internal/mcp` (inbound-only for this slice)

A new leaf package, mirroring `internal/admin`'s own shape (an HTTP-adjacent package that takes a
`*dataplane.Pipeline` and a `Credentials`-equivalent auth check, and registers routes into the
caller-supplied mux) rather than a new standalone service:

- `mcp.NewInboundServer(pipeline *dataplane.Pipeline, verifier *identity.Verifier) http.Handler` —
  returns a handler implementing the MCP Streamable HTTP transport for exactly the one
  `chat_completion` tool, translating an incoming MCP `tools/call` request into a call to
  `pipeline.HandleChatCompletion` and the resulting `adapter.ChatResponse` back into an MCP tool
  result. `identity.Verifier` is passed in, never re-implemented — this package must not grow its
  own bearer-token-checking logic.
- Wired in `cmd/gateway/main.go` alongside the existing route registrations, gated behind its own
  config flag (e.g. `mcp.enabled`, mirroring `EnablePprof`'s own opt-in-by-default-off convention
  for a surface most deployments won't want) — additive, zero behavior change for any deployment
  that doesn't enable it.
- Dependency direction: `internal/mcp` may import `internal/gateway/dataplane`, `internal/identity`,
  and `internal/adapter` (to build tool-call request/response translation) — it must NOT be
  imported by any of those packages, the same one-way dependency `internal/admin` already
  respects, enforceable by the same `go-arch-lint` component boundary already checking every other
  package.

### Trace propagation: use SEP-414, don't invent a mechanism

Per the 2026-09-20 research pass's Finding 7, the current MCP spec revision documents SEP-414 —
standardized OpenTelemetry trace-context propagation via `_meta` keys (`traceparent`, `tracestate`,
`baggage`). Kelvran already has real, shipped `agent_run_id`-scoped OTel tracing via W3C Baggage
(`docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md`). A future implementation should propagate
that existing baggage across the MCP tool-call boundary using SEP-414's own `_meta` convention
rather than inventing a bespoke mechanism — the spec already defines the exact shape Kelvran's
existing tracing needs here.

## Drawbacks

- **A new route, transport, and package is a real ongoing maintenance surface**, however small —
  one more thing to keep in sync with the MCP spec's own continued churn (the 2026-09-20 research
  pass's own Finding 2 shows the spec's deprecation machinery is still actively shedding legacy
  surface even at a frozen core version). Scoping to exactly one tool minimizes but does not
  eliminate this.
- **No real MCP client to validate against yet.** This design is built from the MCP spec's own
  documentation and Kelvran's real code, not from an integration test against a genuine MCP client
  — the same caveat the outbound RFC's own Drawbacks section already carries, and unavoidable for
  any design-only RFC preceding a first real implementation.
- **The single-tool scope is deliberately narrow** — an operator who wants to expose more of
  Kelvran's own admin capabilities via MCP (see Alternatives Considered) would need a follow-up
  RFC, not an extension of this one's own stated scope.

## Alternatives Considered

**Expose a broader admin-capability surface as MCP tools/resources now** (e.g. virtual-key spend
lookups, prompt resolution, deployment listing) rather than just `chat_completion`. Rejected for
this first slice: `internal/admin`'s route table shows several plausible candidates —
`GET /admin/virtual_keys/{name}/spend` (Admin/Viewer/CostViewer-readable, already narrowly scoped)
and `GET /admin/prompts`/`GET /admin/prompts/{id}` (Viewer/Admin-readable, already global
read-only config) are the most natural next candidates precisely because they are already
read-only and already narrowly tiered — but every admin-mutation route (virtual-key
create/delete/rotate, deployment-weight mutation, prompt CRUD/promote/rollback, cache erasure,
backup, pprof) should stay HTTP-Admin-API-only for now: exposing a destructive operation as an MCP
tool callable by anything holding a bearer token is exactly the kind of blast-radius question
`THREAT_MODEL.md`'s own 2026-09-20 correction (this same session, on the compromised-admin-credential
finding from Tier-1 item 7) already flags as a live concern for the EXISTING HTTP surface — adding
an MCP-shaped second way to reach the same destructive routes, before any risk-tiering exists for
the first way (see the sibling Tier-2 RFC on admin RBAC risk-tiering), would compound rather than
scope a real gap. A read-only spend/prompt-lookup tool surface is a plausible, low-risk follow-on
once this first slice is validated — named here as a natural next RFC, not built now.

**Reuse the outbound RFC's not-yet-built `OutboundCredential` concept for inbound auth.** Rejected
as a category error: `OutboundCredential` (per the sibling RFC) is Kelvran acting as a CLIENT
against a downstream party — it has no bearing on how an inbound MCP CLIENT authenticates TO
Kelvran, which is exactly what `identity.Verifier` already does today for every other caller.

**Wait until MCP's own spec fully stabilizes before designing anything.** A real option, and — per
the overall recommendation this RFC does not change — arguably still the right default absent a
concrete demand signal. This RFC exists because scoping the tractable slice is cheap now (mirroring
the outbound RFC's own reasoning for writing itself down early) and because three independent
research passes across two weeks have now converged on the same slice as the right starting point,
making the marginal cost of writing it down essentially zero.

**Build both inbound and outbound together.** Rejected: the outbound direction has real, still-open
credential-delegation questions (per its own RFC's Unresolved Questions) that the inbound direction
does not share at all — inbound has zero new identity concept to invent. Coupling the two would
force inbound's implementation to wait on outbound's harder open questions for no real benefit.

## Unresolved Questions

- **Tool naming and input-schema exactness** — this RFC names `chat_completion` as a working name
  and points at `adapter.ChatRequest`'s shape as the input-schema source, but the exact MCP
  `inputSchema` JSON Schema translation (which canonical fields are required vs. optional, how
  `tools`/`tool_choice` nest) is left to a future implementation RFC, not decided here.
- **Streaming.** `adapter.ChatRequest`/`ChatResponse` support streaming today via a separate
  gateway code path (`HandleChatCompletionStream`); whether the first MCP tool slice supports a
  streaming tool-call result (MCP has its own notion of progressive tool results) or ships
  non-streaming-only first is not decided here.
- **Config-flag naming and default** (`mcp.enabled` above is illustrative, not committed) — left to
  implementation time, matching how every other optional-subsystem flag in this codebase (Redis
  rate limiting, config propagation, Bedrock Guardrails) was named at its own implementation time,
  not pre-decided in a design-only RFC.
- **Whether a read-only admin-capability MCP surface (spend/prompt lookups, per Alternatives
  Considered) becomes a real follow-on RFC** — named as plausible, not committed, and explicitly
  gated on this first slice actually shipping and being validated against a real MCP client first.
- **The same open question the outbound RFC carries, restated for inbound**: is there any real,
  current user demand signal for this at all? Per `docs/upgrade-research/mcp-a2a-brokering-tier1-2026-09-20.md`'s
  own confirmation, still **no** as of this RFC's date — this RFC does not manufacture a trigger,
  it only prepares one to be pulled cheaply if a real signal ever appears.

## Verification

None — this RFC is design-only, per its own Status line. No code changes accompany it; a future
implementation pass would need its own new `gateway/internal/mcp` package, tests, a real MCP-client
integration test (not just unit tests against Kelvran's own translation logic), and its own RFC
status update from "Design-only" to "Accepted, implemented."
