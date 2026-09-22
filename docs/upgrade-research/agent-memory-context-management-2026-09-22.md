# Agent Memory & Long-Context Management-as-a-Service — 2026 Production-Practice Survey (2026-09-22)

## Question

Research how LLM gateways/infra platforms are starting to offer memory/context-management as a
first-class service in 2026: context compaction strategies, memory-store patterns (Letta/MemGPT-style
architectures, Mem0, Zep), and whether any production LLM gateway (not just agent frameworks) actually
offers a memory API rather than leaving it entirely to the client. Distinct from general
multiturn-workload handling (`agentic-multiturn-workloads-2026-09-14.md`, read first — see "Prior
research checked" below) — this pass is specifically about MEMORY/CONTEXT *persistence* as a
gateway-offered capability. Give an honest verdict on whether this belongs in Kelvran's own scope (a
stateless LLM proxy per `PRD.md`) or is fundamentally out of scope, with real reasoning either way.

**Scope:** Cross-cutting — touches `gateway/` (canonical schema, adapters) and Kelvran's own
architectural identity (stateless-per-request proxy vs. stateful agent-memory platform). Does not
touch `evals/` or `api/`.

**Method:** Real external research via `web_search_exa`/`web_fetch_exa` against primary sources —
official docs (LiteLLM, Anthropic Platform Docs, OpenAI Developer Docs, AWS Bedrock AgentCore API
Reference, Zep, Mem0), a GitHub commit (verified, SHA-pinned), a peer-reviewed arXiv paper (Zep's own
architecture paper, arXiv:2501.13956), and one comparative market-analysis source cross-checked against
each vendor's own docs. Every load-bearing claim below is checked against at least the vendor's own
primary documentation; where a second, independent source corroborates it, that is stated explicitly
("confirmed N-independent-sources"); where only one source exists, that is disclosed as a single-source
claim rather than inflated to "confirmed."

**Prior research checked** (per this repo's own convention — grepped before writing, read in full where
adjacent):
- `docs/upgrade-research/agentic-multiturn-workloads-2026-09-14.md` — read in full. Confirms this is
  genuinely adjacent, not duplicate, territory: that pass's five research questions were MCP spec
  stabilization, session/prefix-affinity KV-cache reuse (an inference-engine-layer mechanism, not
  memory), agent-framework handoff schema requirements, and multi-agent fan-out rate-limit composition —
  none of which touch memory/context *persistence* as a service. That report's own RQ5 ("anything else
  a 2026 survey flags") returned zero surviving claims, so no memory-specific claim was silently dropped
  there either.
- `docs/upgrade-research/gateway-agent-framework-interop-2026-09-12.md`, `gateway-mcp-a2a-brokering-2026-09-07.md`/`-2026-09-11.md`,
  `cache-prompt-caching-aware-routing-2026-09-12.md`, `cross-provider-prompt-caching-optimization-2026-09-14.md`,
  `evals-agentic-multiturn-2026-09-11.md` — grepped for "memory"/"context window"/"Mem0"/"Zep"/"Letta"/"MemGPT";
  zero matches beyond the already-read multiturn-workloads report's own cross-references. None of these
  reports address memory-as-a-service.
- Directory-wide grep of all 130 files in `docs/upgrade-research/` for `memory|mem0|zep|letta|memgpt|context.compaction|context.window` —
  no dedicated memory/context-persistence report exists anywhere in this directory prior to this one.
  This is genuinely fresh ground, not a duplicate.
- `DECISIONS.md` — grepped for `memory`, `mem0`, `zep`, `letta`, `memgpt`. 22 hits for "memory," all of
  them describing Kelvran's own **in-memory** (as in, non-persistent, RAM-resident) storage mode for
  rate limits/budgets/identity caches (e.g., "Budget/usage tracking remains in-memory only (resets on
  restart)") — a completely different sense of the word. Zero hits for agent memory, Mem0, Zep, Letta,
  or MemGPT. No prior decision on this topic exists to reconcile against.
- `PRD.md`, `gateway/ARCHITECTURE.md`, `AGENTS.md` — read directly (not grepped) for Kelvran's own
  stated scope/non-goals and current stateless-per-request architecture; see Finding 1 and the verdict
  in the Executive Summary for what was found.

---

## Executive Summary

**Verdict up front, since the briefing asked for one explicitly: building Kelvran's own memory-store
service — a Mem0/Zep-style extraction-and-retrieval pipeline, or even LiteLLM's much thinner
`/v1/memory` key-value CRUD store — is out of scope for Kelvran, and should stay out of scope, not for
lack of a precedent (one now exists, Finding 2) but because it would contradict Kelvran's own
already-real architecture and stated non-goals, not merely a paper scope line.** `gateway/ARCHITECTURE.md`
states plainly that cost/observability aggregation is "purely per-request; aggregating cost across a
session/run today [is not built]" — the dataplane's `finalize` call has no session-scoped state of any
kind, `agent_run_id` is propagated only as OTel baggage for observability, never persisted or read back.
Adding a memory store would be Kelvran's first genuinely stateful, cross-request, content-bearing
subsystem — a different kind of change than adding a fallback chain or an OTel metric, because every
other piece of Kelvran (cache, budget, rate limits) either resets on restart by design or stores
opaque numeric/hash state, never a client's actual conversation content for later retrieval. `PRD.md`'s
own Non-Goals section is explicit that Kelvran is not attempting to be an agent-hosting platform or the
broadest-feature gateway — it aims to be "the most correct one on [cost/cache/eval] failure modes," not
the one that also does memory.

The research converges on a genuinely new landscape data point that didn't exist the last time this
directory's sibling agentic-workload report was written (2026-09-14): **one production LLM gateway,
LiteLLM, shipped a real `/v1/memory` CRUD API in v1.83.14 (April 2026)** — a direct, confirmed answer to
the research brief's core question ("does any production gateway offer a memory API rather than leaving
it to the client"). But what LiteLLM shipped is deliberately thin: a namespaced key-value store with no
extraction, deduplication, or retrieval-ranking intelligence of its own — explicitly built to be
"consumed by [LiteLLM's] new agent loop," i.e., LiteLLM is expanding into being an agent-hosting
platform itself, a scope Kelvran's own `PRD.md` explicitly disclaims. Kelvran's two closest peer *pure*
gateways — Portkey and Helicone — confirm the opposite default: independently verified as having no
memory/state-persistence feature at all ("No / Not documented" on both, per a third-party comparison
cross-checked against each vendor's own docs), leaving conversation state entirely to the client, same
as Kelvran does today.

Separately — and this is the part of the 2026 landscape most worth Kelvran actually tracking — **the
real memory and context-compaction intelligence in 2026 is landing at the model-provider layer, not the
gateway layer.** Anthropic shipped server-side compaction (`compact_20260112`), context editing
(tool-result/thinking-block clearing), and a full Managed-Agents memory-store API (`agent-memory-2026-07-22`,
versioned, ABAC-scoped, file-addressed). OpenAI's Responses API shipped an equivalent server-side
compaction primitive (`context_management.compact_threshold` and a standalone `/responses/compact`
endpoint) plus a persistent, no-TTL `Conversations` API. AWS Bedrock AgentCore ships a full managed
Memory service with short-term event storage and long-term extraction strategies (`SEMANTIC`,
`SUMMARIZATION`, `USER_PREFERENCE`) — directly relevant because Kelvran already has a real Bedrock
Converse adapter. None of these are gateway features Kelvran would need to *build* — they are
provider-native capabilities a client can already opt into today, and Kelvran's adapters currently have
no canonical-schema field to let a client's request for one of them pass through cleanly. That is a
real, narrow, currently-unaddressed gap — Finding 1 below — and it is the one genuinely actionable,
in-scope-adjacent item this research surfaced, sitting squarely in the same "normalize a
provider-native marker across adapters" pattern Kelvran already used for `CacheControl` and tool-choice
forcing.

Third-party memory-layer services — Zep (a temporal-knowledge-graph "Context Lake," built on the
open-source Graphiti engine) and Mem0 (an extraction/deduplication/hybrid-retrieval pipeline) — are the
actual Letta/MemGPT-descendant architectures the research brief named. Both are explicitly
application-layer sidecars: Mem0's own docs state "Mem0 sits between your application and your model,"
and Zep's SDK is invoked directly by the calling application, "works with any agent framework, or
none." Neither integrates at, or expects to integrate at, the gateway/proxy layer — they are peers to a
gateway in a request path, not something a gateway subsumes.

---

## Findings, ranked

### 1. Provider-native context compaction (Anthropic, OpenAI) is real, shipped, and currently invisible to Kelvran's canonical schema — a narrow, genuinely in-scope normalization gap (Small–Medium)

**What:** Both Anthropic and OpenAI now ship server-side context-compaction primitives as opt-in request
fields, not client-side SDK tricks. Anthropic's Claude Platform: `context_management.edits` accepts
`clear_tool_uses_20250919` (clears old tool results past a token trigger, configurable `keep`/`clear_at_least`/
`exclude_tools`) and `clear_thinking_20251015` (manages extended-thinking-block retention), gated behind
the `context-management-2025-06-27` beta header; separately, **`compact_20260112`** (beta header
`compact-2026-01-12`) is full server-side compaction — the API summarizes earlier turns into an opaque,
non-human-readable `compaction` content block once the conversation crosses a token threshold (minimum
50K), and the client serializes that block back into the next request instead of the full history.
OpenAI's Responses API ships the equivalent: `context_management` with a `compact_threshold` field
triggers inline server-side compaction during a normal `/responses` call, emitting an encrypted
compaction item in the stream; a **standalone `/responses/compact` endpoint** additionally supports
fully stateless, ZDR-friendly compaction — you send a full context window and get back a compacted one
to pass into the next call. Both mechanisms are opaque by design (Anthropic's spec says the compaction
content "is not intended to be human-interpretable"; OpenAI's is described identically as "opaque").

**Why it matters for Kelvran specifically:** `gateway/ARCHITECTURE.md`'s Canonical Schema & Provider
Adapters section documents six real normalization points Kelvran already handles this exact way —
unknown-field preservation (Gemini's `thoughtSignature` must round-trip verbatim), and, most directly
relevant, `Message.CacheControl`/`ContentPart.CacheControl` (per `docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md`),
which normalizes Anthropic's `cache_control`, Bedrock's `cachePoint`, and OpenAI's `prompt_cache_key`
behind one canonical marker, letting each adapter collapse it into whatever shape its own provider
actually uses. `context_management`/compaction is structurally the same class of problem — a
provider-native, opt-in, per-request marker with genuinely different shapes across providers — but
unlike `CacheControl`, it has **no canonical-schema field at all today**. A client sending a request
through Kelvran that wants Anthropic's `compact_20260112` or OpenAI's `context_management.compact_threshold`
today has no normalized way to ask for it; whether it's silently dropped or requires bypassing Kelvran's
canonical schema entirely was not verified against Kelvran's own adapter code in this pass (see Open
Questions) — but no existing RFC or `ARCHITECTURE.md` section names this field as already handled, and a
directory-wide grep of `docs/upgrade-research/` and `DECISIONS.md` for `context_management`/`compaction`/`compact_threshold`
returned zero hits, confirming this is a genuinely new observation, not a re-litigation of settled
ground.

**Consistent with settled decisions?** Directly additive to, not a revisit of, the `CacheControl`
normalization pattern (`docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md`) and the tool-choice
normalization pattern (`docs/rfcs/2026-09-14-gateway-tool-choice-normalization.md`) — same shape of
problem, same established solution pattern, no PRD.md scope-out touched. Crucially, this is **not** the
same thing as building a memory store (Finding 2's verdict): passing through a provider's own opaque
compaction marker keeps Kelvran exactly as stateless as it is today — the provider does the summarizing
and holds/returns the opaque blob; Kelvran stores nothing new and adds no session-scoped state of its
own. This distinction — normalize-a-passthrough-marker vs. own-the-state — is the dividing line between
what's in scope here and what (Finding 2) is not.

**2026 best practice grounding:**
- Anthropic's context editing and compaction: confirmed directly against Anthropic's own Platform Docs
  (`platform.claude.com/docs/en/build-with-claude/context-editing`, `.../context-windows`) and the
  first-party cookbook (`platform.claude.com/cookbook/tool-use-context-engineering-context-engineering-tools`,
  dated 2026-03-20) — two independent pages from the same primary source agreeing on identifier names,
  beta headers, and default thresholds [platform.claude.com — confirmed, single-vendor primary source,
  internally consistent across 3+ distinct doc pages].
- OpenAI's Responses API compaction: confirmed directly against OpenAI's own developer docs
  (`developers.openai.com/api/docs/guides/compaction`, `.../websocket-mode`) [developers.openai.com —
  confirmed, single-vendor primary source, internally consistent across 2 distinct doc pages].
- Both providers converged independently on the same design shape (opaque server-computed summary block,
  client-replayed on the next call) within roughly the same window (Anthropic's `compact_20260112` beta
  header implies a January 2026 rollout; OpenAI's guide is undated but describes a comparable mechanism)
  — this convergence across two independently-developed APIs is itself a signal the pattern is real
  production practice, not a one-off experiment, though this specific "convergence" framing is this
  report's own synthesis from the two vendors' docs, not a claim either vendor makes about the other.

**Concrete next step:** No RFC needed yet — this is a "worth tracking, not urgent" finding, the same
tier `gateway-2026-09-06.md` used for its own Finding 6 (provider-side prompt caching vs. Kelvran's own
cache, "genuinely unresolved, not a 'no'"). First actual step, when a real client need surfaces: verify
against Kelvran's own adapter code whether an unrecognized top-level `context_management` field on an
inbound `ChatRequest` is currently silently dropped (likely, since it's not part of the canonical
schema) or passed through as an unknown field the way Gemini's `thoughtSignature` is — this pass did not
check the adapter code directly, only the documented normalization points, so this is stated as an open
question (see below), not a confirmed gap in Kelvran's actual behavior.

**Effort:** Small (if the goal is simply "don't silently drop the field, pass it through opaquely to the
one adapter that understands it") to Medium (if the goal is a proper canonical marker normalized across
providers, matching `CacheControl`'s treatment) — bounded either way, touching the canonical schema plus
2 of 5 adapters (only Anthropic and OpenAI-shaped surfaces currently have anything to normalize;
Bedrock/Gemini/openaicompat have no equivalent compaction primitive documented as of this research).

---

### 2. LiteLLM shipped a real gateway-level memory API (`/v1/memory`) — but it is bare key-value storage bolted onto LiteLLM's own emerging agent-platform ambitions, not memory intelligence; Kelvran building an equivalent is out of scope (Out of scope — no build effort)

**What:** LiteLLM's proxy shipped `POST/GET/PUT/DELETE /v1/memory` in v1.83.14 (dated Apr 27, 2026, per
LiteLLM's own release-notes index) — full CRUD on a `LiteLLM_MemoryTable` (Prisma-backed Postgres table),
with `key`/`value`/JSON `metadata`, `user_id`/`team_id` scoping, key-prefix filtering ("Redis-style
namespace scanning"), pagination, and an audit trail (`created_by`/`updated_by`). The feature's own
commit message (GitHub `70492ce`, PR #26218, verified commit) states the storage shape deliberately
"match[es] the Letta + mem0 hybrid model so future structured fields can be added without a schema
migration" — but the endpoint itself does none of Letta's or Mem0's actual work: there is no LLM-driven
extraction, no deduplication, no conflict resolution, no semantic search. It is exactly what its name
says — a namespaced key-value store — with the extraction/retrieval intelligence left entirely to
whatever calls it. LiteLLM's own May 2026 townhall blog post frames this explicitly as part of a new
"Agent Platform" launch alongside "Adaptive Routing" and "Prompt Compression," and the commit message
says the table is "consumed by the new agent loop" — i.e., LiteLLM is not adding memory to stay a better
proxy; it is adding memory as one piece of becoming an agent-hosting platform in its own right, a
materially different product ambition than a proxy/gateway.

**Why it matters for Kelvran specifically:** this is the single most direct, confirmed answer to the
research brief's core question — yes, a production LLM gateway now offers a memory API rather than
leaving it entirely to the client, refuting a "no gateway does this" framing outright. But it does not
follow that Kelvran should build the same thing. Two independent, closer peers to Kelvran's own
stated positioning — Portkey and Helicone, both pure request-routing gateways, not agent platforms —
were checked and **confirmed to have no memory/state-persistence feature at all**: a third-party
comparison table (`agenticindex.io/compare/helicone-vs-portkey`) scores "Memory & State Persistence" as
"No / Not documented" for both, and this was cross-checked directly against Portkey's own docs (which
describe two caching mechanisms — a Control Plane config cache and an LLM response cache — and nothing
resembling conversation memory) and a second comparison source (`llmtools.cc`) that likewise never
mentions a memory feature for either vendor while describing both products' feature sets in detail.
Kelvran's own architecture (Finding, Executive Summary) already matches the Portkey/Helicone default, not
LiteLLM's: stateless per-request, no session-scoped content storage, `agent_run_id` used only for
observability tagging.

**Consistent with settled decisions?** No explicit prior decision exists in `DECISIONS.md` on this exact
question (confirmed by grep — see "Prior research checked" above), so this finding is establishing a
new, explicit verdict rather than reconciling against a stale one. It is, however, directly consistent
with `PRD.md`'s existing Non-Goals ("Kelvran does not aim to be the broadest-provider gateway... it aims
to be the most correct one on the specific failure modes above" — cost/cache/eval correctness, not
feature breadth) and with `gateway/ARCHITECTURE.md`'s documented stateless-per-request design. Building
Kelvran's own `/v1/memory`-equivalent would be the first subsystem in Kelvran that stores a client's
actual content (not a hash, not a numeric counter, not a cached *response* keyed by request-hash the way
`cache.Cache` already does) for arbitrary later retrieval — a genuinely different kind of commitment than
anything shipped so far, and one neither `PRD.md` nor any RFC has ever proposed.

**2026 best practice grounding:**
- LiteLLM's `/v1/memory` feature: confirmed via 4 independent artifacts from the same vendor — the
  live docs page (`docs.litellm.ai/docs/memory_management`), the dated release notes
  (`docs.litellm.ai/release_notes/v1.83.14/v1-83-14`), the actual GitHub commit (`70492cee`, PR #26218,
  marked "Verified: yes" by GitHub), and the May 2026 townhall blog post naming it part of a new "Agent
  Platform" launch [docs.litellm.ai + github.com/BerriAI — confirmed, 4 same-vendor artifacts, internally
  consistent, including the raw commit diff which is difficult to fabricate].
- Portkey/Helicone lacking any memory feature: confirmed via 2 independent third-party comparison
  sources (`agenticindex.io`, `llmtools.cc`), cross-checked against Portkey's own self-hosting docs
  (`portkey.ai/docs/self-hosting/cache-behavior`, `.../concepts/caching`) which describe only
  config/response caching, never conversation memory [2 independent third-party sources + 1 vendor
  primary source, confirmed].

**Concrete next step:** None — this is a deliberate non-build verdict, not a deferred build item. Worth
one line in `gateway/ARCHITECTURE.md` or a short `DECISIONS.md` entry the next time this area is
touched, explicitly naming that LiteLLM's move was evaluated and declined, so a future research pass
doesn't re-propose it as an undiscovered gap the way this repo's own "doc-vs-code staleness" gotcha
warns against for the *opposite* direction (claims going stale forward) — this is the same discipline
applied to a *rejected* idea, so it doesn't get silently re-litigated as if unconsidered.

**Effort:** N/A (explicit decision not to build).

---

### 3. The real 2026 memory/context intelligence is landing at the model-provider and agent-platform layer, not the gateway layer — Anthropic Managed Agents memory stores and AWS Bedrock AgentCore Memory are the concrete 2026 precedents, and AgentCore is directly relevant given Kelvran's existing Bedrock adapter (Informational — no build effort)

**What:** Beyond the client-side `memory_tool` (`memory_20250818`, unchanged in its client-side-only
design since its 2025-09-29 launch), Anthropic separately ships a genuinely server-side **Managed
Agents Memory Store** API (beta header `agent-memory-2026-07-22`): workspace-scoped collections of
text documents (`memory_stores`), each memory addressed by a hierarchical path, with full CRUD,
optimistic-concurrency preconditions (`content_sha256`), immutable version history (`memver_...`,
30-day retention with compliance-grade `redact`), and `read_write`/`read_only` access control per
mount. Stores attach to a session's sandbox as a live-synced directory. AWS Bedrock AgentCore's Memory
service is architecturally similar but distinct: `CreateMemory` accepts one or more `memoryStrategies`
(`summaryMemoryStrategy`, `userPreferenceMemoryStrategy`, or a `SEMANTIC` built-in), events (raw
conversation turns) are written as short-term memory with a configurable retention window (up to 365
days), and the service *itself* runs the extraction — long-term "memory records" are asynchronously
derived from events per the configured strategy, retrievable via `RetrieveMemoryRecords` (a real
semantic search with `topK`/`metadataFilters`) or `ListMemoryRecords`.

**Why it matters for Kelvran specifically:** AgentCore Memory is not a hypothetical competitor
feature — it is a capability of the exact provider (Bedrock) Kelvran already has a real, shipped
Converse adapter for. A client already using Kelvran to reach Bedrock could, today, separately stand up
an AgentCore Memory resource and call it directly, entirely outside Kelvran — this pass found no
evidence AgentCore Memory is exposed *through* the Converse API surface Kelvran's adapter calls (it is a
distinct `bedrock-agentcore`/`bedrock-agentcore-control` API family, confirmed against AWS's own API
reference), so there is nothing for Kelvran's existing Bedrock adapter to normalize even if it wanted
to — this is a separate AWS service, not a field on the Converse request/response Kelvran already
handles. This finding is therefore purely informational: it confirms where the 2026 market is actually
building memory intelligence (the model-provider/agent-platform layer, with the provider's own LLM doing
extraction under the hood), which directly supports the Finding 2 verdict that a gateway attempting the
same thing (LiteLLM) is doing something structurally different from, and thinner than, what the
providers themselves already ship.

**Consistent with settled decisions?** No conflict with any existing Kelvran decision — this is new
landscape information, not a revisit. It does sharpen `PRD.md`'s own "Kelvran is not an inference engine"
non-goal by extension: Kelvran is also not, and per this finding's own reasoning should not become, a
memory-extraction engine, since that capability already exists natively at the provider layer for at
least two of Kelvran's five adapters' providers (Anthropic, Bedrock) with zero evidence the other three
(OpenAI, Gemini, openaicompat targets) lack an equivalent — this pass did not check OpenAI/Gemini/
self-hosted-runtime-native memory offerings and states that as a real gap (see Open Questions), not a
"no."

**2026 best practice grounding:**
- Anthropic Managed Agents memory stores: confirmed directly against Anthropic's own Platform Docs
  (`platform.claude.com/docs/en/managed-agents/memory`, `.../api/http/beta/memory_stores/memories.md`) —
  two distinct primary-source pages, internally consistent on beta headers, endpoint shapes, and
  retention rules [platform.claude.com — confirmed, single-vendor primary source, internally consistent].
- AWS Bedrock AgentCore Memory: confirmed directly against 5 distinct AWS API-reference/dev-guide pages
  (`ListMemoryRecords`, `RetrieveMemoryRecords`, `GetMemoryRecord`, the SDK memory guide, the
  "create a memory store" guide) — all from `docs.aws.amazon.com`, internally consistent on the
  short-term/long-term split, strategy types, and CLI/SDK/console creation paths [docs.aws.amazon.com —
  confirmed, single-vendor primary source, but unusually thorough internal cross-consistency across 5
  independent doc pages, which is the strongest confidence this report can honestly claim for a
  single-vendor-family source without an independent third party's confirmation].

**Concrete next step:** None — purely informational for now. Worth a one-line mention in
`gateway/ARCHITECTURE.md`'s Bedrock-adapter notes the next time that section is touched, so a future
reader doesn't independently rediscover "wait, doesn't Bedrock already have memory?" as if it were news —
answer: yes, but it's a separate AWS API family, not something Kelvran's existing Converse adapter
touches or would need to change to support a client using it directly.

**Effort:** N/A (informational).

---

### 4. Zep and Mem0 are real, production Letta/MemGPT-descendant memory-layer architectures — and both are explicitly application-layer sidecars, confirming memory-as-a-service and gateway-as-a-service are different market layers by design, not by omission (Informational — no build effort)

**What:** Zep (built on the open-source Graphiti engine, 20,000+ GitHub stars per Zep's own materials)
implements a **bi-temporal knowledge graph**: three hierarchical subgraph tiers (episode, semantic
entity, community), where facts carry four timestamps (valid-from/valid-to for real-world truth,
observed/recorded for provenance) and superseded facts are invalidated, never deleted, enabling
point-in-time queries. Retrieval combines cosine semantic search, BM25 keyword search, and graph
breadth-first search, then reranks. Zep's 2025 architecture paper (arXiv:2501.13956) reports beating
MemGPT's own Deep Memory Retrieval benchmark (94.8% vs. 93.4%) and up to 18.5% accuracy gains with 90%
latency reduction on the harder LongMemEval benchmark — both are the paper's own self-reported
evaluation numbers, not independently reproduced by a third party in this pass. Mem0, in its current
(v3/Platform) architecture, runs a five-stage pipeline on every `add()` call — context lookup,
single-pass LLM fact extraction, hash-based deduplication, batch embedding, and entity extraction — and
a parallel three-signal retrieval on every `search()` call (semantic, BM25 keyword, entity-graph boost,
fused into one score); its own docs report LongMemEval accuracy of 93.x% on its managed platform (again,
vendor-reported, with an explicitly disclosed "±1 point confidence interval due to judge inconsistency"
and an explicit disclosure that "Open-source users should expect directionally similar gains but not
identical numbers" — a rare, honest vendor caveat worth noting).

**Why it matters for Kelvran specifically:** these are exactly the "memory-store patterns" the research
brief named (Letta/MemGPT-style architectures), and both are unambiguous, self-described
application-layer components, not gateway/proxy-layer ones. Mem0's own core-concepts doc states plainly:
"Mem0 sits between your application and your model. You send conversation turns to `add`, then call
`search` before the next model request." Zep's marketing states its SDK "works with any agent framework,
or none" and is invoked directly by the calling application (`client.thread.add_messages`,
`client.thread.get_user_context`). Neither vendor positions itself as something a gateway would
subsume, replace, or sit in front of — they sit *beside* whatever routes the actual model call, which is
exactly the layer Kelvran (or any of its five adapters' providers) occupies. This directly answers
the research brief's framing question about where Letta/MemGPT-descended architectures actually live in
production in 2026: one layer up the stack from a gateway, called explicitly by the application, not
threaded transparently through gateway traffic.

**Consistent with settled decisions?** No conflict — reinforces, rather than revisits, the Finding 2
verdict. If a Kelvran user wants Zep- or Mem0-style memory today, they already can, entirely
independently of which gateway (Kelvran, LiteLLM, Portkey, or none) sits between their app and the
model — nothing about Kelvran's current stateless design blocks or complicates that integration, since
Zep/Mem0 calls happen at the application layer, not inside Kelvran's request path at all.

**2026 best practice grounding:**
- Zep architecture: confirmed against the peer-reviewed arXiv paper (arXiv:2501.13956, Zep's own
  authors) plus Zep's current product docs (`help.getzep.com/graph-overview`, `help.getzep.com/v2/memory.mdx`)
  and marketing pages (`getzep.com`, `getzep.com/platform/context-lake`) — paper and current docs are
  consistent on the core bi-temporal-graph mechanism; the "Context Lake" framing and specific latency
  claims (sub-200ms) are 2026 marketing material from the vendor itself, not independently benchmarked
  in this pass [arxiv.org + getzep.com — confirmed for the core architecture (paper + docs agree);
  performance claims are vendor-self-reported only, flagged as such, not cross-checked against a third
  party].
- Mem0 architecture: confirmed against Mem0's own current docs (`docs.mem0.ai/platform/features/graph-memory`,
  GitHub `core-concepts/how-it-works.mdx`, `skills/mem0/references/architecture.md`) and the original
  Mem0 arXiv paper (arxiv.org/html/2504.19413v1) — the current v3 architecture (single-pass ADD-only
  extraction, entity-linking-replaces-graph-store) is a real, documented evolution from the paper's
  original 4-operation (ADD/UPDATE/DELETE/NOOP) design, confirmed by Mem0's own v2.0.0 release notes
  explicitly describing the change and its rationale [docs.mem0.ai + github.com/mem0ai + arxiv.org —
  confirmed, vendor primary source plus the vendor's own peer-reviewed paper, internally consistent
  across the architectural evolution between the two].
- Broader 2026 academic landscape survey (SimpleMem, LightMem, MemoryOS, A-Mem, CompassMem, GAM as
  distinct memory-consolidation approaches beyond Zep/Mem0): sourced from a single arXiv paper
  (arxiv.org/html/2607.29377, "Zero-Mem") — this is a single-source claim, not cross-checked against a
  second independent source, and is disclosed here as background landscape color rather than a
  load-bearing claim about any specific system's production maturity.

**Concrete next step:** None — informational, confirming rather than changing anything about Kelvran's
own scope.

**Effort:** N/A (informational).

---

## Top 3 do next

1. **Do not build a Kelvran-native memory store** (Finding 2). This is the single most important
   output of this research pass, and it's a decision, not a build item — worth a short explicit note in
   `gateway/ARCHITECTURE.md` or `DECISIONS.md` the next time this area is touched, naming that LiteLLM's
   `/v1/memory` move was found and evaluated, so it doesn't get silently re-proposed as an undiscovered
   gap in a future pass.

2. **Verify, then (if warranted) normalize, provider-native context-compaction markers** (Finding 1).
   Small effort to check today (does Kelvran's canonical schema currently silently drop an inbound
   `context_management` field, or is there already an unknown-field-preservation path it rides on?);
   Small–Medium effort to add a proper canonical marker if the check reveals a real gap and a client
   need exists. This is the one item in this report that is both genuinely actionable and clearly
   in-scope, following the exact pattern already established for `CacheControl` and tool-choice.

3. **Track AWS Bedrock AgentCore Memory as Bedrock-adapter-relevant landscape, not a build item**
   (Finding 3). No action needed now; worth a one-line mention in the Bedrock adapter's own notes so a
   future reader doesn't waste time rediscovering that this AWS capability exists and is (correctly)
   untouched by Kelvran's Converse-focused adapter.

---

## Caveats

- **This report's single strongest, most-corroborated finding (LiteLLM's `/v1/memory`) is also the
  least architecturally deep** — it is a bare KV store, not memory intelligence, and this report is
  explicit about that distinction throughout rather than letting the existence of *a* gateway memory API
  imply Kelvran should build a comparable one.
- **Zep's and Mem0's own performance benchmarks (DMR, LongMemEval, BEAM) are vendor-self-reported** in
  every source this pass found — neither was cross-checked against an independent third-party
  reproduction. Both vendors' own docs disclose this indirectly (Mem0's explicit "±1 point confidence
  interval" and "OSS users should expect directionally similar but not identical numbers" caveats are
  unusually honest for vendor marketing, and are reproduced here rather than smoothed over).
- **Anthropic's and AWS's memory/compaction features are each confirmed only against that single
  vendor's own documentation family**, not cross-checked against an independent third party covering the
  same feature — both are graded "confirmed" in this report on the strength of multiple *independent
  pages within the same vendor's docs* agreeing internally, which is real corroboration of internal
  consistency but not the same as third-party verification. This is disclosed explicitly rather than
  inflating these to the same confidence tier as the LiteLLM finding (which does have an independent,
  outside-the-vendor GitHub commit as one of its four sources).
- **This pass did not check Google Gemini/Vertex AI or any self-hosted-runtime (vLLM/TGI/Ollama)
  memory-adjacent offering** — Finding 3's claim that "the real intelligence is at the provider layer"
  is grounded in Anthropic and AWS Bedrock specifically, both real and relevant to Kelvran's own
  adapters, but the other three providers Kelvran adapts to (OpenAI beyond the Responses-API compaction
  primitive in Finding 1, Gemini, and openaicompat self-hosted targets) were not surveyed for an
  equivalent memory service and this is disclosed as a real gap, not folded into the finding as if
  checked.
- **This pass did not verify Kelvran's own adapter/canonical-schema code directly** for Finding 1's
  claim about `context_management` handling — the claim that "no canonical-schema field exists for this
  today" is grounded in the absence of any RFC, `ARCHITECTURE.md` section, or upgrade-research doc naming
  it, not a direct code read; see Open Questions.
- **The topic is evolving fast even within 2026 itself** — LiteLLM's Memory API is 5 months old as of
  this research date and was explicitly labeled "beta" as of LiteLLM's own May 2026 townhall post;
  Anthropic's Managed Agents memory-store beta header (`agent-memory-2026-07-22`) is barely 2 months old.
  Neither should be treated as a permanently settled 2026 baseline — a re-check in 6-12 months, per this
  directory's own "reconsideration" convention, would be reasonable if any client demand signal for
  memory-adjacent features appears in the meantime.

## Open questions

1. **Does Kelvran's canonical `ChatRequest` schema currently silently drop an inbound `context_management`
   field, or ride on an existing unknown-field-preservation path** (per `gateway/ARCHITECTURE.md`'s
   normalization point #4)? Finding 1's "genuine gap" framing depends on this, and this pass did not
   verify it directly against the adapter code — a fast, concrete follow-up check for whoever picks this
   up next, not a full research pass.
2. **Would any real Kelvran client ever actually want provider-native compaction normalized through
   Kelvran**, or is this purely theoretical until a client asks? Unlike `CacheControl` (which Kelvran
   auto-populates by default on system prompts, an active choice on Kelvran's part), compaction is
   opt-in, per-call, and currently has zero demand signal named anywhere in this repo.
3. **Do OpenAI (beyond the Responses-API compaction primitive already covered in Finding 1), Gemini, or
   any of the self-hosted `openaicompat` targets (vLLM/TGI/Ollama/LocalAI) ship an equivalent
   provider-native memory or long-context-management service** the way Anthropic and AWS Bedrock do
   (Finding 3)? Not surveyed in this pass — a real gap, not answered "no."
4. **If MCP ever standardizes a memory/context primitive** (no evidence of this in the spec as surveyed
   by `agentic-multiturn-workloads-2026-09-14.md`, which found MCP's July 2026 revision moved toward
   *less* server-held state, not more), would that change anything about Finding 1's normalization
   calculus? Purely speculative — no evidence surfaced in this pass that MCP is heading in a
   memory-standardizing direction at all.
