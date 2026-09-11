# RFC: MCP/A2A outbound-credential delegation — design only, no code

## Status

Design-only, explicitly deferred pending its own named trigger. Phase 5 of the v2-upgrade-research
plan (`docs/upgrade-research/gateway-mcp-a2a-brokering-2026-09-11.md`). Mirrors the exact
"designed in full, deferred" precedent already established for the Redis-backed cache
(`docs/rfcs/2026-09-11-gateway-redis-backed-cache-design.md`): scope the "how" now, without
committing to the "when" — MCP/A2A brokering itself remains genuinely unbuilt
(`gateway/internal/mcp` has zero code, confirmed via grep this session), and this RFC does not
change that.

## Date

2026-09-11

## Author(s)

Session agent (Claude Code), per the v2-upgrade-research plan's Phase 5.

## Summary

Kelvran's virtual-key model (`internal/identity.VirtualKey`) authenticates and authorizes a
*caller* against Kelvran's own gateway. It has no concept for a *second*, distinct credential leg:
Kelvran itself acting as a client against a downstream MCP server or A2A agent on that caller's
behalf. This RFC sketches — without implementing — an outbound-credential abstraction for that
second leg, informed by how two real peer gateways (LiteLLM, Portkey) already solve this problem
in production.

## Motivation

`docs/upgrade-research/gateway-mcp-a2a-brokering-2026-09-11.md` (Finding, `confidence: high`)
found that **no peer gateway extends its own inbound/virtual-key credential into the credential
used for the outbound leg to a downstream MCP server** — both LiteLLM and Portkey layer a
separate, distinct mechanism:

- **LiteLLM** ships 5 distinct `auth_type` patterns for its MCP gateway, including RFC 8693
  On-Behalf-Of (OBO) token exchange — which itself requires TWO simultaneous credentials (the
  caller's own token AND a service credential) to mint the downstream token. Never a bare reuse
  of the inbound virtual/API key.
- **Portkey** authenticates centrally via its own Admin/Workspace API key, then *injects* a
  separate, per-MCP-server credential it manages independently — the inbound key never becomes
  the outbound one.

This directly contradicts the naive assumption that Kelvran's `VirtualKey` could simply be reused,
unchanged, as the credential Kelvran presents to a downstream MCP server once brokering is built.
Scoping this now — while the finding is fresh and before any real implementation pressure exists
to reach for the nearest-at-hand shortcut (reusing `VirtualKey` as-is) — is cheap; discovering the
gap mid-implementation, after `internal/mcp` already has real call sites depending on a wrong
assumption, would not be.

`docs/users/USER_GUIDE.md`'s own MCP/A2A section (line 86) already states the *intended* (but
unbuilt) design: "Registering an MCP server or A2A agent will go through the same identity/budget
objects as outbound LLM routing." This RFC does not contradict that framing — Kelvran's
`identity`/`budget` objects remain the right place to anchor *authorization* (does this caller's
virtual key have quota/permission to invoke this tool at all) — but sharpens it: authorization via
the existing identity objects is a separate question from *which literal credential bytes* get
handed to the downstream MCP server, and the research confirms real systems keep those two
questions structurally separate.

## Detailed Design

### The two credential legs stay conceptually distinct

1. **Inbound leg (already real, unchanged by this RFC)**: a caller authenticates to Kelvran with
   a `VirtualKey`. Kelvran's existing `internal/identity`/`internal/budget`/`internal/ratelimit`
   objects decide whether that caller is authorized, has quota, etc. — exactly as they do for
   outbound LLM routing today.
2. **Outbound leg (the gap this RFC scopes, not yet built)**: once authorized, Kelvran itself
   becomes a client of a downstream MCP server or A2A agent. That downstream party has its own,
   separate credential requirement Kelvran must satisfy — a bearer token, an OAuth client
   credential, a mutual-TLS cert, etc., depending on what the operator registered for that server.

### Sketch: an `OutboundCredential` object, analogous to (not derived from) `VirtualKey`

A future implementation would introduce a new, small identity object — sketched here only in
shape, not as a committed interface:

- Registered by an operator per MCP server/A2A agent (mirroring how `Deployment`s are registered
  per upstream LLM provider today), not derived from or shared with any caller's `VirtualKey`.
- Resolved at brokering time from the caller's already-authorized request context — i.e., "this
  `VirtualKey` is authorized to invoke tools on MCP server X" resolves to "use `OutboundCredential`
  Y for the actual downstream call" — a lookup/mapping step, never a passthrough of the caller's
  own inbound secret.
- Credential *material* itself (a bearer token, an OAuth client secret, etc.) follows the exact
  existing `*Env`-suffixed environment-variable convention `DeploymentConfig` already uses for
  every upstream LLM provider credential today (`APIKeyEnv`, `AccessKeyIDEnv`, etc.) — never a
  new secrets-handling pattern invented for this one subsystem.

### Where LiteLLM's OBO pattern would or wouldn't apply

RFC 8693 On-Behalf-Of token exchange is real, production-proven (LiteLLM ships it), and lets a
downstream server's own audit trail see the ORIGINAL caller's identity, not just "some request
from Kelvran" — a real advantage over Portkey's simpler "inject a per-server credential
Kelvran manages" shape when the downstream MCP server's own authorization model cares about the
original caller. Whether Kelvran needs OBO's full complexity (a token-exchange endpoint, a second
simultaneous credential) or Portkey's simpler injected-credential model is explicitly left open —
see Unresolved Questions.

### Guardrail scanning of brokered tool-call results — already covered, not a gap this RFC needs to close

`docs/upgrade-research/gateway-mcp-a2a-brokering-2026-09-11.md`'s own finding on this (confirmed
against real code, not assumed): `dataplane.serializeMessages` already performs a full,
role-agnostic JSON marshal of every message in `req.Messages` before pre-call guardrail scanning,
with zero role-based skip logic — a `role:"tool"` message representing a fed-back MCP tool-call
result is already scanned identically to any other message today. A regression test proving this
already exists (`TestHandleChatCompletionPreCallScansToolResultMessagesFedBackFromAnEarlierTurn`,
`gateway/internal/gateway/dataplane/guardrail_test.go`, added 2026-09-11), and `THREAT_MODEL.md`
already cites it (lines 14/71/75/103). This RFC's outbound-credential design does not need to
duplicate or re-verify that finding — it is orthogonal to credential delegation.

## Drawbacks

A new identity-object concept, however small, is a real ongoing maintenance surface once built —
another kind of credential to register, rotate, and audit, on top of `VirtualKey`,
`DeploymentConfig`'s upstream-provider credentials, and `budget.Tracker`. This RFC does not build
it, so the cost is deferred, but a future implementation RFC will need to weigh this honestly
against the alternative of *not* supporting MCP/A2A brokering at all for longer.

## Alternatives Considered

**Reuse `VirtualKey` unchanged as the outbound credential.** Rejected by the research itself — no
real peer gateway does this; both LiteLLM and Portkey deliberately layer a separate mechanism.
Reusing `VirtualKey` would also leak Kelvran-internal credential material (however indirectly)
toward a downstream party that has no business relationship with Kelvran's own callers.

**Do nothing now — wait until MCP/A2A implementation actually starts.** A real option, and
arguably the *default* absent this RFC — but the research finding is fresh and cheap to record
now; waiting risks the exact "nearest-at-hand shortcut" failure mode this RFC exists to head off,
per the Motivation section above.

**Build the full OBO token-exchange flow now, speculatively.** Rejected as premature — MCP's own
spec is still mid-breaking-change (per the same research pass's other findings on MCP spec
stability), and Kelvran has no first real MCP integration to validate against yet.

## Unresolved Questions

A future implementation RFC — triggered once MCP's own spec stabilizes AND Kelvran has a first
real MCP/A2A integration to build against, per this same research pass's other findings — would
still need to decide:

- OBO token exchange (LiteLLM's shape) vs. a simpler injected per-server credential (Portkey's
  shape) — or something else entirely once real downstream MCP servers' own auth requirements are
  known concretely, rather than surveyed secondhand.
- Whether `OutboundCredential` registration belongs in the same YAML config file as
  `DeploymentConfig`, or a new, separate section — not decided here.
- Credential rotation/revocation semantics — `DeploymentConfig`'s upstream-provider credentials
  have no live rotation story today either; whether MCP/A2A's outbound leg should do better from
  day one, or match that existing precedent, is open.
- Per-tool scoping (a downstream MCP server may expose many tools; does one `OutboundCredential`
  cover the whole server, or does scoping go finer) — genuinely undecided, and likely depends on
  what real downstream MCP servers' own permission models turn out to support.

## Verification

None — this RFC is design-only, per its own Status line and this phase's own scope. No code
changes accompany it; a future implementation pass would need its own new identity-object
package, tests, and its own RFC status update from "Design-only" to "Accepted, implemented."
