# RFC: Embedding-based semantic L3 cache — a write-path/read-path split — design only, no code

## Status

Design-only, explicitly deferred pending its own named trigger — mirrors the exact "designed in
full, deferred" precedent already established for the Redis-backed L1/L2 cache
(`docs/rfcs/2026-09-11-gateway-redis-backed-cache-design.md`) and the MCP/A2A outbound-credential
design (`docs/rfcs/2026-09-11-gateway-mcp-outbound-credential-design.md`): de-risk the "how" now,
without committing to the "when." Directly answers Tier-2 backlog item "semantic L3 cache
write/read-split design," per `docs/upgrade-research/semantic-cache-embeddings-tier1-2026-09-20.md`
(this document's own grounding research, Finding 3), which is the first report in this project's
history to make an embedding-based L3 tier concretely buildable with existing code rather than
purely hypothetical.

**Trigger, unfired**: the research's own overall verdict is unchanged from two prior reports —
`not_yet` for shipping a live embedding-based L3 on the request path — because the real gate was
never engineering effort (which the now-shipped `POST /v1/embeddings` endpoint resolves) but real
production miss-telemetry to size the feature against, which does not exist yet (no multi-tenant
organic production traffic runs through Kelvran today). What **is** newly `build_now`-eligible,
specifically because of that endpoint's existence, is this design document itself — de-risking the
architecture so a future decision to build isn't also a from-scratch design exercise.

## Context

`docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md` ("Why not real embeddings yet") named
two independent blockers to a real embedding-based L3 tier: (1) no mature, statically-linkable
Go-native ML runtime to compute embeddings in-process, and (2) calling an embedding API on every
L3 lookup **is** an upstream call, which "silently voids the one architectural guarantee this whole
subsystem exists to provide" (`gateway/ARCHITECTURE.md`'s Request Lifecycle: a cache lookup happens
*before* the router/provider call, so a hit never touches an upstream).

`POST /v1/embeddings` (shipped 2026-09-17; `Pipeline.HandleEmbeddings`,
`gateway/internal/gateway/dataplane/dataplane.go:1977`, backed by real, tested
`adapter.EmbeddingAdapter` implementations for Bedrock Titan V2 and OpenAI `text-embedding-3-*`)
resolves neither blocker directly — it is network-bound provider translation, structurally
identical to every existing chat adapter, never local inference. What it resolves is a **third,
previously-uncosted blocker**: the engineering effort of building and hardening embedding-generation
code at all. That work — a real adapter interface, a real wire-format contract, a real
`Dimensions` knob, cost accounting that already prices embedding `Usage` with zero new code — now
exists and is reusable. Fresh pricing checked directly against AWS's and OpenAI's own pages this
round confirms embedding calls are commodity-cheap ($0.02-0.13/1M tokens) regardless of provider —
two to three orders of magnitude below the completion cost this cache exists to avoid, so dollar
cost is a settled non-issue. Latency is unchanged and remains the real constraint: the same
Bedrock Titan V2 call this endpoint proxies to was independently measured (a prior round's own
throwaway script) at 350-382ms steady-state (p50 369ms) — a real, unavoidable tax on whichever side
of a request pays it.

This document proposes the one genuinely new, concrete architecture the shipped endpoint unlocks:
confine that latency tax to exactly the traffic that needs it, by splitting *when* the embedding
call happens for a write versus a read.

## Design

### The core split

**Write path (async, off the client's critical path):** a cache write (`writeCache`,
`dataplane.go:1603`) already happens after a full chat completion has been computed — the response
is already assembled and about to be cached. Reading the current call site directly
(`dataplane.go:2426-2429`) confirms `writeCache` — and therefore every one of today's L1/L2/lexical-L3
`Put` calls — runs **before** `runMissPath` returns to its caller, which in turn runs before
`HandleChatCompletion` serializes and returns the response to the client: this write is genuinely
on the synchronous critical path today, not merely appearing to be. This settles
`semantic-cache-embeddings-tier1-2026-09-20.md`'s own Open Question 4 with direct evidence: yes, a
background goroutine is a real, necessary change, not a no-op formalization of something already
async. The proposal: after `writeCache`'s existing synchronous L1/L2/lexical-L3 writes complete
unchanged, fire a **new**, separate embedding-generation-and-write step in a background goroutine
using `context.WithoutCancel(ctx)` — the same fire-and-forget pattern `internal/alerting` already
established for webhook delivery (`DECISIONS.md`'s `[2026-09-18]` Item 13) — so the real ~350-380ms
embedding-generation cost never delays the client's own response. This builds up an
embedding-indexed candidate set over time at zero request-latency cost to the request that funds
each write.

**Read path (synchronous, but gated strictly behind three cheaper checks):** a new incoming query
still needs its own real-time embedding call to search that index — there is no way to avoid this
without a client-blocking round-trip, so it is not attempted for free. The design confines this
cost to exactly the traffic that has already exhausted every cheaper option: the embedding search
runs **only after** L1 (exact-match), L2 (normalized-match), and L3-lite (lexical Jaccard) have all
already missed (`checkLexicalCache`, `dataplane.go:1704`, called at `dataplane.go:2279`), and only
when the query is not flagged volatile by L3-lite's existing gates. All three of those checks are
sub-millisecond in-process computation; gating the ~350-380ms embedding call behind them means it
is paid only by the fraction of traffic this tier exists to add coverage for — a real paraphrase a
lexical near-duplicate check cannot catch — never by traffic any cheaper layer already resolves.

### New interfaces, mirroring the existing `LexicalCache` shape exactly

Per `AGENTS.md`'s explicit, non-negotiable rule — *"Never let Cache call a provider directly, or
let it depend on `internal/adapter`"* — the actual embedding-generation call can never live inside
`internal/cache`. It must live in `dataplane.go`, mirroring the exact precedent `Fingerprint()`
(`dataplane/entities.go`) and `normalizeMessages`/`serializeMessages` already set for
adapter-message-touching logic the cache package itself is architecturally barred from. Concretely:
`dataplane.go` calls `p.adapters[dep.Provider].(adapter.EmbeddingAdapter)` and a caller-side upstream
invocation — the same functions `HandleEmbeddings` already calls — and passes only the resulting
`[]float32` vector into `internal/cache`. `internal/cache` never sees an `adapter.EmbeddingRequest`
and never calls a provider itself.

```go
// gateway/internal/cache/embedding.go (new, sibling to lexical.go)

// CosineSimilarity returns the cosine similarity between two equal-length
// vectors -- the embedding-space analogue of JaccardEstimate. Mismatched
// lengths or a zero-magnitude vector return 0, false rather than
// panicking or dividing by zero.
func CosineSimilarity(a, b []float32) (similarity float64, ok bool)

// EmbeddingCandidate is one Cache embedding-L3 search hit. Deliberately
// carries every LexicalCandidate provenance field UNCHANGED -- per
// AGENTS.md's "never weaken the hard-gate" rule, a cosine-similarity
// candidate must clear the identical entity/freshness/model/guardrail/
// response-format/prompt/negation/reasoning-blocks gates a lexical
// candidate already does. Similarity is a cosine score, never a Jaccard
// estimate; nothing else differs in kind from LexicalCandidate.
type EmbeddingCandidate struct {
	Resp        []byte
	Similarity  float64
	Fingerprint map[string]struct{}
	WrittenAt   time.Time
	ModelID     string
	// EmbeddingModelID is a NEW field with no LexicalCandidate analogue --
	// see "Embedding-space compatibility" below for why this is a
	// mandatory, non-optional gate, not an optional provenance field.
	EmbeddingModelID           string
	GuardrailPolicyVersion     string
	ResponseFormatFingerprint  string
	PromptFingerprint          string
	NegationFingerprint        map[string]struct{}
	ReasoningBlocksFingerprint string
}

// EmbeddingCache mirrors LexicalCache exactly -- Search returns
// zero-to-many scored candidates (never a single hit/miss), and tenant
// partitioning is enforced inside the implementation, never layered on
// by the caller, per THREAT_MODEL.md's KeyPooling mitigation.
type EmbeddingCache interface {
	Search(ctx context.Context, tenantID string, vector []float32, embeddingModelID string, k int) ([]EmbeddingCandidate, error)
	Put(ctx context.Context, tenantID string, vector []float32, embeddingModelID string, resp []byte, fingerprint map[string]struct{}, modelID string, guardrailPolicyVersion string, responseFormatFingerprint string, promptFingerprint string, negationFingerprint map[string]struct{}, reasoningBlocksFingerprint string, ttl time.Duration) error
}
```

`inprocess.EmbeddingCache` would mirror `inprocess.LexicalCache`'s exact structure: a
mutex-protected, per-tenant `tenantBucket` (`container/list.List`, front = most recently used),
independent LRU cap per tenant, brute-force cosine scan on `Search` — no ANN, no external vector
database, no pgvector. `cache-semantic-embedding-readiness-round4-2026-09-11.md`'s own Finding 4
already confirmed (checked against chromem-go and `kelindar/search`'s documented ~100,000-entry
brute-force ceiling) that Kelvran's realistic per-tenant scale never approaches where brute-force
cosine similarity becomes a real CPU cost — the identical judgment that shape this codebase's own
"leaf, zero heavy deps" discipline already applies to the lexical tier.

### Embedding-space compatibility — a new gate this design adds beyond what the research findings named explicitly

A raw `[]float32` vector is only meaningful compared against another vector from the **same**
embedding model and dimensionality. Titan V2's own `Dimensions` knob (256/512/1024) and OpenAI's
separate `text-embedding-3-small`/`-large` models each produce vector spaces that are not merely
"lower quality" relative to one another when mismatched — they are structurally incomparable;
cosine similarity between vectors from two different models is meaningless, not merely noisy. Since
an operator's deployment config can change which embedding model/dimensionality a tenant's traffic
resolves to over time, `EmbeddingCandidate.EmbeddingModelID` (new field above) must be an
**exact-match hard gate**, checked by the caller exactly like `ModelID` already is — a candidate
whose `EmbeddingModelID` doesn't match the current request's own embedding call is rejected
outright, never scored. This closes a real correctness hazard neither prior research report nor
the L3-lite RFC needed to consider, since it did not exist before an embedding-based tier was a
live design.

### Internal-only embedding calls: a lighter-weight path, not `HandleEmbeddings`' full pipeline

`semantic-cache-embeddings-tier1-2026-09-20.md`'s Open Question 2 asks whether a cache-triggered
embedding call should reuse `HandleEmbeddings`' full customer-facing pipeline (auth, per-key
rate-limit, budget reserve, guardrail scan) or bypass it as a lighter-weight internal call. This
design proposes **bypass**: `HandleEmbeddings`' pipeline exists to serve a customer's own explicit,
metered `/v1/embeddings` request — a cache-triggered call runs on every tenant's cache-checkable
traffic continuously, an entirely different traffic shape that would need its own throttling
strategy regardless (an unthrottled internal call competing for the same per-key RPM budget as a
customer's real embeddings usage is a real self-inflicted rate-limit contention bug waiting to
happen). The internal call should go directly through the adapter/upstream layer
`HandleEmbeddings` itself calls, with its own, separate, internal-only concurrency/rate bound (a
fixed worker pool or a small `ratelimit.ConcurrencyLimiter` instance scoped to this feature alone
— sized at implementation time, not this design pass) — never the customer-facing budget/rate-limit
path at all.

### Billing attribution: left open, not defaulted

Open Question 3 — whether a cache-triggered embedding call's real dollar cost should be attributed
to the requesting tenant's own budget or treated as unbilled gateway infrastructure overhead — is
deliberately **not resolved** by this design. `HandleEmbeddings`' own cost-accounting path bills the
caller of an explicit customer request; an internal cache-write embedding call has no natural
caller to bill the same way. This needs an explicit product decision before any write-path
embedding generation ships — silently defaulting to either answer would be a real, undisclosed
policy choice.

### Related, smaller, separately-shippable items this research also surfaces

- **A `X-Kelvran-Cache-Similarity` response header**, exposing the raw computed similarity score
  (L3-lite today, extendable to embedding-L3 if it ships) on every cache decision — mirrors
  LiteLLM's own shipped `x-litellm-semantic-similarity` header and Kelvran's own existing
  `X-Kelvran-Overhead-Duration-Ms` precedent for disclosing server-computed signals. Independently
  buildable today against the existing lexical tier; not gated on this RFC's own trigger.
- **`THREAT_MODEL.md`'s EU AI Act scoping note** (line 89) explicitly names "the semantic cache
  moves from future work to an active RFC" as its own revisit trigger. This document is arguably
  that trigger — a short scoping-rationale update to that note is real, cheap follow-on work
  independent of whether the feature itself ever ships.
- **The cache-erasure gap** (`DECISIONS.md`'s `[2026-09-18]` Item 10: `POST /admin/cache/erase`
  does not reach L3-lite's similarity-based lookup, only L1/L2) would extend unchanged to an
  `EmbeddingCache` tier by default, being a second similarity-based lookup structurally identical
  in this respect. Any real implementation of this design must plumb erasure through
  `EmbeddingCache` from day one, not repeat the gap a second time.

## Alternatives considered

**Ship the read path only, calling the adapter/upstream layer directly with no write-path split at
all.** Rejected: this pays the real ~350-380ms tax on every cache-checkable miss with no way to
build up embedding coverage cheaply beforehand, and still voids the "hit never touches upstream"
guarantee on the read side for zero mitigating benefit the split provides for free.

**A pgvector/HNSW/external vector database.** Rejected, per
`cache-semantic-embedding-readiness-round4-2026-09-11.md`'s own already-established finding: no
external vector database is needed at Kelvran's realistic per-tenant scale, and introducing one
would be new infrastructure this codebase's "leaf, zero heavy deps" discipline doesn't need yet.

**A bare cosine-similarity threshold with no entity/freshness hard-gate**, matching GPTCache's or a
naive Portkey-style implementation. Rejected outright — `AGENTS.md`'s explicit "never weaken the
hard-gate" rule forbids this regardless of any latency/simplicity benefit; every existing
`LexicalCandidate` provenance field must carry over to `EmbeddingCandidate` unchanged, per this
design's own core structure above.

**Defaulting the billing-attribution question to "unbilled infrastructure overhead" now, to avoid
leaving it open.** Rejected: this is a real, undecided product policy question the research
explicitly could not resolve from code alone — defaulting it silently would be a hidden decision,
not a design simplification.

## Verification

None — this RFC is design-only, per its own Status line. A future implementation pass would need
its own new `internal/cache/embedding.go` (interfaces + `CosineSimilarity`) and
`internal/cache/inprocess/embedding.go` (the concrete tenant-partitioned store), tests mirroring
`lexical_test.go`'s existing shape plus a new adversarial hard-gate test proving an
`EmbeddingModelID` mismatch is rejected outright regardless of cosine score, and its own RFC status
update from "Design-only" to "Accepted, implemented" once the production-traffic trigger named
above actually fires.
