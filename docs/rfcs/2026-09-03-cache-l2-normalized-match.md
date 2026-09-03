- **Status**: accepted
- **Date**: 2026-09-03
- **Author(s)**: project founder + Claude Code

## Summary

Add Cache L2 (normalized-match): a second `cache.Cache` instance checked on an L1 miss, keyed on a *conservatively* normalized form of the request rather than the byte-exact form L1 uses. A hit promotes into L1; a genuine miss writes back to both layers. `internal/cache/inprocess.Cache` gains a real capacity bound (LRU eviction) as a direct prerequisite — it has none today, and running two instances doubles an existing, real unbounded-memory gap rather than introducing a new one. Per `PRD.md`'s own scope line ("exact-match (L1) and normalized-match (L2) layers at v1"), this is in-scope v1 work, distinct from L3 (semantic, embedding+HNSW, hard-gate-mandatory), which stays unbuilt.

## Motivation

L1 only helps on byte-identical repeats. Real traffic — the same logical question with different whitespace, a trailing `?` added or dropped, a different Unicode input method — misses L1 for no good reason. `gateway/ARCHITECTURE.md`'s own Request Lifecycle already names L2 as the next pipeline stage after L1 ("`cache lookup, L2 normalized match → hit → log, return`"); this RFC builds it.

## Detailed Design

### The normalization allowlist — narrower than this RFC's own grounding research recommended, and why

Grounded via a dynamic-workflow research pass (4 parallel angles + synthesis — see Research Trail). The research's own safe-normalization-boundary angle proposed a 6-operation allowlist (whitespace trim, whitespace collapse, Unicode NFC, case-folding of the "natural-language envelope," trailing punctuation strip, JSON envelope canonicalization), reasoning from prose examples: "Georgia" the country vs. the US state, percentage rounding, negation words, word-reordering.

Every one of those examples is prose. None considers **pasted code** — and Kelvran is an AI-infrastructure gateway PRD.md frames as serving agent traffic, where "here's my code, why does it fail" is a realistic, common message shape, not an edge case. Checked against the real schema (`adapter.Message.Content` is a plain string — no structured fenced-code/quoted-span field to key off of): two of the six proposed operations have a genuine collision risk the research's own examples didn't surface:

- **Internal whitespace collapse** is unsafe in the presence of indentation-sensitive code. Two messages pasting the same question with Python snippets differing only in indentation depth (a real, meaningful difference — Python's indentation is syntax, not decoration) would collapse to the *same* normalized key, serving a cached answer about the wrong code's behavior.
- **Case-folding** is unsafe for code/identifiers even without markdown fences. `"Is 'Select' a valid variable name in Python"` and `"Is 'select' a valid variable name in Python"` are different questions (a capitalized identifier vs. a reserved word) that case-folding would silently conflate. The research's own proposed fix — exclude fenced/quoted spans — requires parsing markdown-style code spans out of a free-text field reliably, which the research itself flagged as asserted, not verified, against Kelvran's actual schema. Getting that parsing wrong in either direction (a missed fence lets a real collision through; a false-positive fence just costs hit rate) is a real implementation-correctness risk for a first pass whose entire point is being conservative.

**v1's actual allowlist — three operations, each with zero plausible collision risk regardless of content type (prose or code), verified by construction, not by enumerating counterexamples**:

1. **Outer (leading + trailing) whitespace trim**, per message. Trimming a message's *outer* edges never touches internal structure/indentation — a code block's own leading whitespace lives inside the trimmed content, untouched.
2. **Unicode NFC normalization** (never NFKC — confirmed via the research: NFKC folds compatibility forms that change meaning, e.g. `x²`→`x2`, full-width digits, roman numerals; NFC merges only codepoint sequences Unicode itself defines as canonically identical). Doesn't touch whitespace, case, or content structure at all.
3. **Trailing terminal punctuation strip** — a single trailing `.`, `!`, or `?` stripped from the *last* message only, after step 1's trim. Code blocks don't end in a bare `?`/`!`, and a trailing `.` at the very end of an otherwise-trimmed message is essentially never syntactically significant in any mainstream language's statement-ending position without other content following it.

JSON-envelope canonicalization (key order, structural whitespace) was in the research's list too but isn't actually a new operation to build: `serializeMessages` already `json.Marshal`s a `[]adapter.Message` slice — Go struct field order is fixed by the type definition, not iteration order, so the encoding is already canonical today, for L1 as much as L2. Nothing to add here.

**Explicitly deferred, not rejected**: internal whitespace collapse and case-folding remain real, valuable normalizations with a real safe implementation path (reliable code/quote-span detection) — just not a v1 concern. Revisit if real hit-rate data ever justifies the added parsing complexity and risk.

### Architecture: a second `cache.Cache` instance, not an interface change

`cache.Cache` (`port.go`) is untouched. `gateway/ARCHITECTURE.md`'s own Request Lifecycle already models L1 and L2 as separate sequential pipeline stages, each with independent hit/return semantics — not one call hiding two tiers. A composite-`Get` approach is also structurally awkward here: L2's key must be derived from the *normalized* pre-hash content, and L1's key is already a SHA-256 hash — you can't un-hash it to normalize it.

```go
// internal/cache/key.go — sibling to Key, same primitive-only contract,
// same tenantID-must-be-present discipline (THREAT_MODEL.md's KeyPooling
// finding — there must be no codepath that computes an L2 key without it).
func NormalizedKey(tenantID, model, normalizedMessages string, temperature *float64, maxTokens *int) string

// NormalizeMessages applies exactly the 3-operation allowlist above —
// nothing else, ever, without a new RFC.
func NormalizeMessages(messages []adapter.Message) string
```

`dataplane.Config`/`Pipeline` gain `CacheL2 cache.Cache`, wired in `cmd/gateway/main.go` as a second `inprocess.New(...)` call (see capacity-bound section below for its constructor args). Both `HandleChatCompletion` (`dataplane.go`) and `HandleChatCompletionStream` (`streaming.go`) currently duplicate the same L1-only lookup/write-back block — this RFC extracts a shared pair of helpers on `Pipeline`, mirroring the `checkRateLimit` precedent from `docs/rfcs/2026-09-03-distributed-rate-limiting.md`:

```go
// checkCache checks L1 then L2 (in that order), promoting an L2 hit into
// L1 (best-effort — a promotion failure never affects the response
// already found). Returns the raw cached bytes and whether either layer
// hit.
func (p *Pipeline) checkCache(ctx context.Context, l1Key, l2Key string) (cached []byte, hit bool)

// writeCache writes to both layers eagerly, best-effort, on a genuine
// miss — matching gateway/ARCHITECTURE.md's "cache write-back (all
// layers)" line. No lazy/async L2 population: the response is already in
// hand, and a background subsystem for it would be unjustified complexity.
func (p *Pipeline) writeCache(ctx context.Context, l1Key, l2Key string, encoded []byte)
```

`tenantID` flows into `NormalizedKey` exactly as it already does into `Key` — the leading hash input, never optional, never a codepath that omits it.

### Prerequisite: `inprocess.Cache` gets a real capacity bound

Checked directly, not assumed: `inprocess.Cache` today is a bare `map[string]cacheEntry` with lazy, `Get`-triggered TTL expiry and **no capacity bound of any kind** — no max-entries cap, no byte budget, no background sweeper. An entry written and never fetched again lingers forever. This is a real, pre-existing gap in the already-shipped L1 cache, not something L2 introduces — but running a second instance for L2 doubles the blast radius of an existing unbounded-memory risk, which this RFC isn't willing to ship without addressing.

Adds real, minimal LRU eviction via the standard `container/list` + map pattern (doubly-linked list tracking recency, evict from the back on overflow):

```go
func New(maxEntries int) *Cache
func NewWithClock(maxEntries int, now func() time.Time) *Cache
```

`Get` on a hit moves the entry to the front of the recency list; `Put` inserts at the front and evicts from the back if `len(entries) > maxEntries`. `maxEntries <= 0` mapped to a sane default (10,000), not "unbounded" — there is no longer an unbounded mode by design.

### Config

```yaml
cache:
  ttl_seconds: 300        # optional; L1 default, matches today's unwritten 5-minute default made explicit
  max_entries: 10000      # optional; per-instance cap, applies to L1
  l2:
    ttl_seconds: 75       # optional; shorter than L1's default — see TTL rationale below
    max_entries: 10000    # optional; L2's own independent cap — the two instances never share one map
```

New `controlplane.CacheConfig`/`CacheL2Config`, parsed the same optional-section way as `budget:`/`rate_limit:`. Omitting the whole section preserves today's exact behavior for L1 (5-minute TTL, now-bounded at a default 10,000-entry cap instead of truly unbounded — a safety improvement applied uniformly, not gated behind opting in, since "unbounded" was never a deliberate feature).

### TTL rationale

Per the research: TTL is conceptually about content freshness, not match-precision risk — and the allowlist above is designed to make an L2 collision structurally impossible, not just less likely within a time window. TTL is not a substitute for that guarantee. Still, L2's default TTL (75s) is shorter than L1's (5 min) as defense-in-depth, consistent with this project's general posture of layering independent controls (mirroring, e.g., budget's cap existing independently of rate-limiting's fail-open policy in the distributed-rate-limiting RFC) rather than relying on any single mechanism alone.

## Drawbacks

- A second in-process cache instance doubles L1's own per-request memory footprint at the same entry cap — bounded now (see capacity-bound section), but real.
- The narrowed v1 allowlist (3 operations, not the research's proposed 6) means a lower hit-rate uplift than a less-conservative design would achieve. Accepted deliberately — see "narrower than this RFC's own grounding research recommended, and why" above.
- No industry precedent exists for this exact three-tier scheme among the products surveyed (confirmed via this RFC's own research: Portkey and LiteLLM are strictly two-tier; GPTCache's pre-processing hook does content extraction, not normalization; Redis LangCache's "exact" mode folds in case-insensitivity but isn't a separately named tier). Kelvran is defining this safety envelope from first principles, not inheriting a validated pattern — stated plainly, not overstated as proven practice.

## Alternatives Considered

1. **The research's full 6-operation allowlist (including whitespace collapse and case-folding with fenced-span exclusion)** — rejected for v1 for the code-content collision risk identified above; the exclusion logic needed to make it safe was itself unverified against Kelvran's actual message schema.
2. **A single `Get` call hiding both tiers internally (widen `cache.Cache`'s interface)** — rejected: `ARCHITECTURE.md`'s own Request Lifecycle already models L1/L2 as separate stages, and L2's key can't be derived from L1's already-hashed key, making a truly hidden composite awkward without changing `Get`/`Put`'s signature everywhere (every implementation, every test, the dormant `grpcserver`/`grpcclient` seam).
3. **Lazy/async L2 population instead of eager write-back on every miss** — rejected: the response is already in hand at write-back time; an async subsystem for it is unjustified complexity with no offsetting benefit (matches this project's recurring "don't build a mechanism the data doesn't need yet" pattern).
4. **Leave `inprocess.Cache` unbounded and ship L2 anyway** — rejected: doubling an already-real unbounded-memory risk without addressing it would be shipping a known gap silently, which this project's own conventions don't do.

## Unresolved Questions

- No incident/postmortem exists for a normalization-caused wrong cache hit in any real LLM cache (confirmed via this RFC's own research) — the code-content collision risk this RFC identifies is reasoned from first principles, not observed in production anywhere, including Kelvran's own (nonexistent yet) production traffic.
- No real hit-rate data validates L2's actual uplift for Kelvran's traffic shape; the research's cited +16.1pp figure is from an unrelated project's different traffic mix, not a Kelvran measurement.
- Whether reliable code/quote-span detection is ever worth building to safely re-admit whitespace-collapse/case-folding to the allowlist — deferred, not decided against permanently.
- `max_entries`' right default (10,000) is a reasonable-sounding round number, not derived from any real memory budget or measured request-size distribution — revisit once real traffic exists.

## Research Trail

Grounded via a dynamic-workflow research pass (4 parallel angles: competitor precedent for a normalized-match tier, the safe-normalization boundary, architecture integration against the real current code, and eviction/TTL policy — plus a synthesis). The synthesis's own proposed allowlist was independently narrowed after this pass, by extending its own "construct a concrete counterexample" method to a content type (pasted code) none of its four angles happened to consider — verified against `internal/adapter.Message`'s real schema (a plain string field, no structured code/quote markup) before concluding the research's proposed fenced-span exclusion wasn't safely implementable for a first pass. The `inprocess.Cache` capacity-bound gap was verified by direct code inspection, not assumed from the research's own characterization of it.
