- **Status**: accepted
- **Date**: 2026-09-03
- **Author(s)**: project founder + Claude Code

## Summary

Ship **Cache L3-lite**: a third cache layer catching near-duplicate *phrasing* (word substitutions, reordering, minor rewording) that L2's narrow 3-operation allowlist deliberately doesn't touch — via MinHash/shingling lexical similarity, not embedding-based semantic similarity. Every hit passes an entity/number/date hard-gate and a freshness/risk model before being served, per `PRD.md`'s and `THREAT_MODEL.md`'s existing, non-negotiable requirement that L3 never ship on a bare similarity threshold. **Real embedding-based semantic matching is explicitly deferred to a later RFC** — this pass's own grounding research found it isn't safely buildable today (see "Why not real embeddings yet" below), the same way `docs/rfcs/2026-09-03-cache-l2-normalized-match.md` shipped a narrower allowlist than its own research proposed once a real risk surfaced.

## Motivation

`PRD.md` names L3 as one of two concrete differentiators this whole project exists to build correctly where the surveyed competitors don't: *"Caching is a coin flip, not a judgment... 2026 security research (CacheAttack, KeyPooling) shows this same mechanism is actively exploitable: 86-90% hijack rates, cross-tenant leakage in every gateway tested."* `THREAT_MODEL.md`'s Cache STRIDE table already commits to concrete mitigations before any L3 code existed: an entity/number/date hard-gate, a freshness/risk model, provenance metadata, and a similarity floor never below ~0.9 for open-ended traffic. This RFC is the first pass at actually building that — grounded via a dedicated dynamic-workflow research pass (5 parallel angles + synthesis; see Research Trail) rather than assumed from the threat model's prose alone.

## Detailed Design

### The cited papers, verified — and what they actually validate

All three works `THREAT_MODEL.md`/`PRD.md` cite are real, independently verified (one spot-checked directly against its live arXiv abstract before writing this RFC, not merely trusted from the research pass's report):

- **CacheAttack** = Zhang, Liu, Xie, Huang, She, *"From Similarity to Vulnerability: Key Collision Attack on LLM Semantic Caching"* (ICML 2026). 86% LLM-response-hijacking rate, 90.6% against agentic tool invocation, a financial-agent case study. Its core thesis is structural, not a checklist: cache-hit locality and cryptographic collision-resistance are *mutually exclusive design goals* — *"every viable defense reduces caching efficiency… no lossless solution."* That ceiling is stated here plainly, not glossed over.
- **"When Cache Poisoning Meets LLM Systems"** = Wu et al. (NDSS 2026). 82–89% success rates against production semantic-caching integrations at major cloud providers. Its own tested defenses (perplexity-, paraphrase-, and classifier-based checks against the *query alone*) all failed. The one thing it validated with a real F1 score (0.87, explicitly "not perfect") is a **query↔response consistency check** — re-verifying the *cached response* still answers the *incoming query*, not entity extraction.
- **KeyPooling** = Sun et al., *"Measuring Where LLM API Relay Paths Collapse Prompt Cache Isolation"* (arXiv:2608.17485). About credential-pooling-driven cross-tenant leakage — orthogonal to the other two, already satisfied by this project's existing tenant-namespace-in-the-key discipline (`docs/rfcs/2026-09-02-virtual-keys-budgets.md`).

**Stated plainly, not smoothed over**: none of the three papers propose or test entity/number/date extraction as their defense. That pattern is real and used in industry practice (a Google Cloud engineering writeup, independent OSS cache-guard tools), layered onto `THREAT_MODEL.md`'s own requirement — not something the cited academic research itself validated. The single most evidence-backed mitigation these papers actually support is the query↔response consistency check, which this RFC does **not** implement (see "Why not real embeddings yet" — it needs the same ML infrastructure this pass avoids for the same reasons). This is a real, acknowledged gap, not a claim that this RFC closes the strongest mitigation path — see Unresolved Questions.

### Why not real embeddings yet

Checked directly against this project's own architecture, not assumed: `gateway/ARCHITECTURE.md`'s Request Lifecycle guarantees a cache lookup happens *before* the router/provider call specifically "so a hit never touches an upstream." Calling an embedding API (OpenAI, Voyage — Anthropic doesn't offer one, confirmed against its own docs) on every L3 lookup **is** an upstream call; it silently voids the one architectural guarantee this whole subsystem exists to provide, on both the read and write path. The in-process alternative (a Go ML runtime) isn't safely buildable today either: official cgo ONNX Runtime Go bindings merged days before this research (no tagged release), cgo breaks the `golang:1.25-alpine → scratch` static-binary story outright, and pure-Go alternatives are pre-v1/`v0.0.1-rc1`/explicitly labeled "EXPERIMENTAL." A sentence-embedding model file is 80-100MB+, a fundamentally different distribution artifact than today's ~5MB scratch image, for a solo maintainer with zero production traffic yet to justify the ops burden.

MinHash/shingling needs none of this: pure Go stdlib (`hash/fnv` or `crypto/sha256` for shingle hashing), in-process, zero new `go.mod` entries. It's honestly scoped as **lexical near-duplicate matching, not semantic paraphrase understanding** — and it still needs the exact same hard-gate work real embeddings would need, so nothing here is wasted once/if a real embedding-based L3 ever gets its own later RFC, gated on real miss-telemetry showing lexical matching's actual paraphrase-miss rate (not assumed now, since no production traffic exists to measure it).

### MinHash/shingling design

```go
// internal/cache/lexical.go (new)

// Shingles splits normalizeMessages' output into overlapping k-word
// shingles (k=3) — the standard MinHash unit. Operates on the SAME
// normalized text L2 already produces (dataplane.normalizeMessages),
// so L3 inherits L2's own conservative safety guarantees (never
// collapsing code indentation or folding case) for free, rather than
// re-deriving its own, possibly less careful, text-processing pass.
func Shingles(normalizedText string, k int) []string

// MinHashSignature computes an N-value MinHash signature (N=128) from a
// shingle set — N independent hash-permutation minimums, the standard
// MinHash construction. A fixed-size []uint64, not an embedding: no
// external model, no network call, pure arithmetic over the shingle
// hashes.
func MinHashSignature(shingles []string, n int) []uint64

// JaccardEstimate returns the fraction of matching positions between
// two same-length signatures — an unbiased estimator of the true
// Jaccard similarity between the underlying shingle sets.
func JaccardEstimate(a, b []uint64) float64
```

### Entity/number/date hard-gate

Per the research's own honest framing (industry practice, not paper-validated) — and deliberately simple, auditable, no gazetteer dependency:

```go
// internal/gateway/dataplane/entities.go (new) — schema-aware work,
// stays in dataplane per the existing serializeMessages/normalizeMessages
// precedent; internal/cache never imports internal/adapter (key.go's own
// documented boundary).

// Fingerprint extracts a query's entity/number/date signature: every
// regex-matched number (int/decimal/currency/percentage), every
// regex-matched date, and every capitalized multi-token sequence (a
// coarse proper-noun proxy — no gazetteer, no NER model). Order-
// independent (returned as a set), since "is Paris in this query" must
// hold regardless of where in the sentence it appears.
func Fingerprint(messages []adapter.Message) map[string]struct{}
```

The hard-gate itself is a set-equality check between the current query's fingerprint and the candidate's fingerprint (captured as provenance at write time): **not a subset check, not a similarity score — exact match, or the candidate is rejected outright.** A query mentioning `$92` can never match a cached entry fingerprinted for `$250`; a query mentioning `Paris` can never match one fingerprinted for `London`. This is deliberately blunt rather than clever — matching the same "closed allowlist, not a normalizer that guesses at equivalence" discipline `docs/rfcs/2026-09-03-cache-l2-normalized-match.md` already established for L2's own normalization allowlist.

### Freshness/risk model

A concrete, minimal, real checklist — every item either already-available data or a cheap regex check, none requiring new infrastructure:

1. **`writtenAt` provenance** on every L3 entry (the same field L1/L2 already track via TTL, extended here as explicit provenance rather than pure expiry, per `THREAT_MODEL.md`'s Repudiation mitigation requiring cache hits be "annotated with provenance... so a downstream failure is traceable").
2. **Volatility bypass**: a query matching a small keyword/regex list (`weather`, `price`, `stock`, `score`, `today`, `current`, `now`, relative-date terms) skips L3 entirely — a hard bypass straight to upstream, not a soft risk score, since time-sensitive queries have no honest "freshness budget" at all.
3. **Per-content-type Jaccard threshold**, never below ~0.9 for open-ended, user-facing traffic per `THREAT_MODEL.md`'s existing floor — see Unresolved Questions for why this exact number's transfer from embedding-cosine to Jaccard similarity isn't independently validated here.
4. **Entity/number/date hard-gate** (above) — exact fingerprint match, no partial credit.
5. **`model_id`+`model_version` exact match** between write-time provenance and the current request's resolved deployment — a mismatch is an automatic miss, never partial credit, since different model versions can legitimately give different correct answers to the same question.
6. Every check's outcome (which bucket, which threshold, entity match result, model match result) is written into the same provenance record `THREAT_MODEL.md` already requires — one record, not a parallel log.

### Architecture: `LexicalCache`, not a widened `cache.Cache`

```go
// internal/cache/lexical.go — a distinct interface, mirroring L2's own
// RFC's rejection of a hidden composite Get: a similarity search
// returns zero-to-many scored candidates, not a single hit/miss.
type LexicalCandidate struct {
    Resp        []byte
    Similarity  float64
    Fingerprint map[string]struct{} // the STORED query's entity/number/date set
    WrittenAt   time.Time
    ModelID     string
}

type LexicalCache interface {
    Search(ctx context.Context, tenantID string, signature []uint64, k int) ([]LexicalCandidate, error)
    Put(ctx context.Context, tenantID string, signature []uint64, resp []byte, fingerprint map[string]struct{}, modelID string, ttl time.Duration) error
}
```

The hard-gate and freshness-risk-model checks live in `dataplane.go` as a new `checkLexicalCache` step — **not** inside the `LexicalCache` implementation — mirroring the existing `normalizeMessages`/`serializeMessages` precedent (schema-aware extraction stays in `dataplane`; `cache` stays decoupled from `adapter.Message`, per `key.go`'s own documented boundary) and, more importantly, keeping the single highest-consequence check in this codebase auditable in one function rather than requiring archaeology through a vector-index-shaped package:

```go
func (p *Pipeline) checkLexicalCache(ctx context.Context, vk *identity.VirtualKey, req adapter.ChatRequest, signature []uint64) (cached []byte, hit bool) {
    if isVolatileQuery(req.Messages) {
        return nil, false // bypass: skip L3 entirely, never a soft score
    }
    candidates, err := p.cacheL3.Search(ctx, vk.ID, signature, l3SearchK)
    if err != nil {
        return nil, false // fail-closed: a search error skips L3, never bypasses the gate
    }
    queryFingerprint := Fingerprint(req.Messages)
    for _, c := range candidates {
        if !fingerprintsEqual(queryFingerprint, c.Fingerprint) {
            continue
        }
        if !freshnessRiskModel(c.WrittenAt, c.ModelID, currentModelID(vk, req), c.Similarity) {
            continue
        }
        return c.Resp, true
    }
    return nil, false
}
```

Wired as its own stage between `checkCache` (L1/L2) and the router, matching `gateway/ARCHITECTURE.md`'s Request Lifecycle, which already lists L3 as a distinct lookup line with its own hard-gate/freshness clause, not folded into L1/L2's plain "hit → log, return."

## Drawbacks

- **The single most evidence-backed mitigation (the NDSS query↔response consistency classifier, F1=0.87) is not implemented in this pass.** It needs a trained classifier — the same real-ML-infrastructure gap this RFC already ruled out real embeddings for. This is a genuine, acknowledged limitation, not a claim that this RFC closes the strongest available defense.
- Lexical (MinHash/Jaccard) matching catches phrasing drift, not true paraphrase/semantic equivalence — a real ceiling on this layer's hit-rate uplift, stated honestly rather than oversold as "L3 is done."
- CacheAttack's own structural thesis applies here too: any cache-hit-locality mechanism trades some collision-resistance for hit rate. The hard-gate and freshness model reduce, not eliminate, this risk — matching the paper's own "no lossless solution" finding rather than claiming a perfect fix.
- No production traffic exists yet to validate the ~0.9 Jaccard threshold, the volatility keyword list's real false-negative rate, or the coarse entity extraction's real false-positive/negative rate on genuine traffic.

## Alternatives Considered

1. **Real embedding-based semantic L3 now** — rejected for this pass; see "Why not real embeddings yet." Not rejected permanently — deferred to a later RFC gated on real miss-telemetry.
2. **A gazetteer-backed named-entity recognizer for the hard-gate** — rejected for v1: adds a real maintenance burden (place/org name lists) for a check that a simpler capitalized-multi-token-sequence extractor already covers for the concrete collision examples in the cited threat model (a different city, a different amount) without needing to classify entity *type*, only entity *identity*.
3. **A single global Jaccard/similarity threshold instead of per-content-type buckets** — rejected: `THREAT_MODEL.md`'s own cited attack data shows adversarial queries scoring above typical fixed thresholds; a graduated, content-type-aware threshold is the more defensible design even without perfect calibration data yet.
4. **Burying the hard-gate inside the `LexicalCache` implementation** — rejected for the same reason `docs/rfcs/2026-09-03-cache-l2-normalized-match.md` kept `normalizeMessages` out of `internal/cache`: the gate needs `adapter.Message` schema access, which `cache` is explicitly barred from, and burying Kelvran's single highest-consequence security check inside a swappable adapter risks silent drift if a second concrete implementation is ever wired in.

## Unresolved Questions

- Whether `THREAT_MODEL.md`'s "~0.9" similarity floor, specified for embedding-cosine similarity, is the right numeric threshold for Jaccard-estimate similarity over MinHash signatures — these have different statistical properties, and this RFC applies the same number without independent calibration. Revisit once real traffic gives real data.
- No real production traffic exists to validate any hit-rate assumption for L3-lite, the volatility keyword list's coverage, or the coarse entity extractor's precision/recall on genuine traffic.
- Whether/when a real embedding-based semantic L3 (and the NDSS-validated query↔response consistency classifier alongside it) is worth building — deliberately not decided here; the trigger is concrete miss-telemetry showing lexical matching's real paraphrase-miss volume, not a fixed timeline.
- MinHash's real shingle size (`k`) and signature length (`N`) tuning — this RFC's constants (k=3, N=128) are standard textbook defaults, not tuned against Kelvran's actual traffic shape, which doesn't exist yet.

## Research Trail

Grounded via a dynamic-workflow research pass (5 parallel angles: the hard-gate's mechanism grounded in the actual cited papers, the embedding-generation approach, ANN library choice, a concrete freshness/risk-model checklist, and architecture integration — plus a synthesis). One primary-source claim (CacheAttack's real existence, title, authors, and headline numbers) was independently spot-checked directly against its live arXiv abstract before being written into this RFC, confirming the research pass's own citation work was accurate rather than fabricated. The synthesis's own conclusion — that a full semantic L3 is too large and insufficiently justified for one pass, and that the RFC should scope itself down to a lexical "L3-lite" instead — was adopted directly, mirroring `docs/rfcs/2026-09-03-cache-l2-normalized-match.md`'s own precedent of narrowing scope once a real risk or complexity gap surfaced mid-research rather than forcing the originally-imagined full feature into one pass.
