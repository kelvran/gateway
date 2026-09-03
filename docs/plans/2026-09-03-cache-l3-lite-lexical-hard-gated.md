> **For agentic executors:** Task 1 (MinHash/shingling primitives) and Task 2 (entity/number/date fingerprinting) are independent of each other but both must land before Task 3 (`LexicalCache` + `inprocess` implementation), which must land before Task 4 (`Pipeline` wiring: `checkLexicalCache`). Task 5 is config + wrap-up.

---

**Goal:** A request that misses L1/L2 but is lexically near-duplicate (word substitutions, reordering, minor rewording) to a prior request hits L3-lite instead of the real upstream — gated by an entity/number/date hard-gate and a freshness/risk model, never a bare similarity threshold, per `PRD.md`/`THREAT_MODEL.md`'s existing, non-negotiable requirement.

**Architecture:** New `internal/cache/lexical.go` (MinHash/shingling primitives + the `LexicalCache` interface — a distinct shape from `cache.Cache`, since a similarity search returns scored candidates, not a single hit/miss) and a new `internal/cache/inprocess` implementation of it (brute-force Jaccard-estimate scan over an LRU-capped, tenant-partitioned candidate set — no ANN, per the RFC's own research). New `internal/gateway/dataplane/entities.go` (schema-aware entity/number/date fingerprint extraction — stays in `dataplane`, never in `cache`, per `key.go`'s documented boundary). `dataplane.Pipeline` gains a `checkLexicalCache` stage between `checkCache` (L1/L2) and the router.

**Tech Stack:** Pure Go stdlib only (`hash/fnv` or `crypto/sha256` for shingle hashing, `regexp` for number/date/capitalized-sequence extraction) — zero new `go.mod` entries, per the RFC's own "why not real embeddings yet" reasoning.

**Spec:** `docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md` — the exact MinHash construction, the hard-gate's fingerprint-equality design, the freshness/risk-model checklist, and why real embeddings are explicitly deferred all live there; this plan implements them verbatim.

**Global Constraints** (inherited from the spec + `AGENTS.md`):
- `AGENTS.md`'s own explicit "Never" rule applies directly: never ship an L3 change that removes or weakens the entity/freshness hard-gate in favor of a bare similarity threshold. Every task below that touches the gate must keep it a hard, exact-match requirement — never a soft score, never optional.
- A volatile-query bypass (weather/price/stock/etc.) skips L3 entirely — never a soft risk adjustment.
- A `LexicalCache.Search` error fails closed (skip L3, fall through toward upstream) — never fails open into "serve unchecked."
- The hard-gate and freshness-risk-model logic lives in `dataplane.go`, never inside the `LexicalCache`/`inprocess` implementation — `internal/cache` must never import `internal/adapter`.
- This is explicitly `L3-lite` — lexical (MinHash/Jaccard), not embedding-based semantic matching. No task in this plan adds an embedding dependency, a vector index, or an ANN library.

---

## Task 1 — MinHash/shingling primitives

**Files:**
- Create: `gateway/internal/cache/lexical.go`
- Create: `gateway/internal/cache/lexical_test.go`

**Steps:**
- [ ] `Shingles(normalizedText string, k int) []string`: splits on whitespace, returns overlapping k-word windows (k=3 default, but take k as a parameter — don't hardcode it inside the function, since Task 5's config section may want it tunable later even if this pass doesn't wire that yet).
- [ ] `MinHashSignature(shingles []string, n int) []uint64`: the standard MinHash construction — n independent hash-permutation minimums over the shingle set. Use a single well-distributed hash function (e.g. FNV-64 or a SHA-256-derived value) combined with n distinct, cheap integer permutations (e.g. `hash*a[i] + b[i] mod largePrime`), not n separate cryptographic hashes — this is the standard efficient MinHash implementation pattern, not a novel design.
- [ ] `JaccardEstimate(a, b []uint64) float64`: fraction of matching positions between two equal-length signatures; must handle mismatched-length inputs by returning an error or a defined sentinel — decide which during implementation and document the choice, don't leave it an implicit panic.
- [ ] Tests: two shingle sets with significant overlap produce a MinHash signature pair with a Jaccard estimate close to their true Jaccard similarity (compute true Jaccard directly via set intersection/union for the test's expected value, don't just assert "some value > 0"); two completely disjoint shingle sets produce an estimate close to 0; identical input produces exactly 1.0; empty input is handled without a panic (defined behavior, not a crash).

**Verify:** `cd gateway && go build ./internal/cache/... && go test ./internal/cache/... -race`

## Task 2 — Entity/number/date fingerprinting

**Files:**
- Create: `gateway/internal/gateway/dataplane/entities.go`
- Create: `gateway/internal/gateway/dataplane/entities_test.go`

**Steps:**
- [ ] `Fingerprint(messages []adapter.Message) map[string]struct{}`: regex-extracts numbers (int/decimal/currency-prefixed/percentage-suffixed), regex-extracts dates (a bounded, explicit set of common formats — ISO 8601, `MM/DD/YYYY`, `Month Day[, Year]` — don't attempt exhaustive date-format coverage, document what's NOT covered rather than silently missing formats), and extracts capitalized multi-token sequences (consecutive Title-Case words, a coarse proper-noun proxy, no gazetteer) from every message's `Content`. Returns a set (map keys), order-independent.
- [ ] Tests: **the load-bearing safety tests** — a query mentioning `$92` and a query mentioning `$250` must produce different fingerprints (proving the hard-gate's whole reason for existing actually holds); a query mentioning `Paris` and a query mentioning `London` must produce different fingerprints; a query mentioning `2024-01-15` and one mentioning `2024-01-16` must produce different fingerprints. Also test the null/negative case: two genuinely paraphrased queries with NO numbers/dates/capitalized entities (e.g. "how do I reverse a list" vs. "what's the way to reverse a list") must produce EMPTY, therefore EQUAL, fingerprints — proving the gate doesn't block legitimate lexical near-duplicates that carry no entity/number/date content at all.

**Verify:** `cd gateway && go build ./internal/gateway/dataplane/... && go test ./internal/gateway/dataplane/... -race`

## Task 3 — `LexicalCache` interface + `inprocess` implementation (depends on Tasks 1-2)

**Files:**
- Modify: `gateway/internal/cache/lexical.go` (add the interface/candidate types)
- Create: `gateway/internal/cache/inprocess/lexical.go`
- Create: `gateway/internal/cache/inprocess/lexical_test.go`

**Steps:**
- [ ] Define `LexicalCandidate`/`LexicalCache` exactly per the RFC's sketch.
- [ ] `inprocess`'s implementation: an LRU-capped (reuse the same `container/list` pattern `inprocess.Cache` already established for L1/L2, per `docs/rfcs/2026-09-03-cache-l2-normalized-match.md`), tenant-partitioned (a `map[tenantID][]entry`, or a single map keyed by `tenantID+internal-index` — decide the concrete shape during implementation, but tenant partitioning must be structural, never a post-hoc filter, per `THREAT_MODEL.md`'s KeyPooling mitigation) candidate store. `Search` does a brute-force `JaccardEstimate` scan over the tenant's own candidate set only (never across tenants) and returns candidates sorted by similarity descending, capped at `k`.
- [ ] Tests: **the load-bearing tenant-isolation test** — two different tenants with lexically-identical stored entries must never have one tenant's `Search` return the other's candidate, mirroring L1/L2's own cross-tenant isolation test precedent exactly; capacity eviction behaves like L1/L2's own LRU (least-recently-*searched*, not just least-recently-written); `Search` against an empty/cold cache returns zero candidates, not an error.

**Verify:** `cd gateway && go build ./internal/cache/... && go test ./internal/cache/... -race`

## Task 4 — `Pipeline` wiring: `checkLexicalCache` (depends on Task 3)

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go`
- Modify: `gateway/internal/gateway/dataplane/streaming.go`
- Modify: `gateway/internal/gateway/dataplane/dataplane_test.go` / `streaming_test.go` (test-helper updates for `Config.CacheL3`)
- Create: `gateway/internal/gateway/dataplane/lexical_cache_test.go`

**Steps:**
- [ ] `Config` gains `CacheL3 cache.LexicalCache` (required, validated in `NewPipeline` like every other dependency — no silent "disabled" mode, matching `CacheL2`'s own precedent).
- [ ] `isVolatileQuery(messages []adapter.Message) bool`: the volatility keyword/regex bypass — a small, explicit, documented keyword list (weather/price/stock/score/today/current/now/relative-date terms), not an open-ended heuristic.
- [ ] `freshnessRiskModel(writtenAt time.Time, storedModelID, currentModelID string, similarity float64) bool`: implements the RFC's checklist items 1, 3, 5 (staleness budget, per-content-type threshold — a single bucket/threshold is fine for this first pass per the RFC's own acknowledgment that real calibration data doesn't exist yet; document this as a known simplification, don't silently under-deliver against the RFC's own "per-content-type" framing without saying so — either implement real buckets now or explicitly note in a comment that this pass ships one bucket and defers real per-content-type tiering).
- [ ] `checkLexicalCache(ctx, vk, req, signature) (cached []byte, hit bool)` exactly per the RFC's sketch — volatility bypass first, then search, then per-candidate fingerprint-equality + freshness-risk-model checks, fail-closed on a search error.
- [ ] Wire `checkLexicalCache` into both `HandleChatCompletion` and `HandleChatCompletionStream`, between the existing `checkCache` (L1/L2) call and the router/deployment-resolution step. On a genuine miss at all three layers, write back to L1, L2, AND L3 (extending `writeCache` or adding a parallel `writeLexicalCache` call — decide the cleanest shape during implementation, but all three layers must be populated on a real upstream response, matching `gateway/ARCHITECTURE.md`'s "write-back (all layers)" line).
- [ ] Tests: **the load-bearing full-pipeline safety test** — two requests that are lexically near-duplicate (paraphrased, no numbers/dates/entities involved) must hit L3 (upstream call count stays flat); two requests that are lexically near-duplicate but differ in a mentioned number/date/entity must NOT hit L3 (upstream call count increments — the hard-gate actually blocking a would-be false match); a volatile query (containing "weather") must never reach L3 even if a lexically-similar volatile query was cached moments before (bypass proven, not just asserted); a `LexicalCache.Search` error must result in the request still succeeding via a real upstream call, never an unchecked serve.

**Verify:** `cd gateway && go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./...`

## Task 5 — `cmd/gateway` wiring + config + docs, changelog, wrap-up (depends on Task 4)

**Files:**
- Modify: `gateway/cmd/gateway/main.go` (construct the `inprocess` `LexicalCache`, wire into `dataplane.Config`)
- Modify: `gateway/internal/gateway/controlplane/config.go` (+ test) — extend `CacheConfig` with an `L3` sub-section (`max_entries`, `ttl_seconds`, `similarity_threshold`) mirroring `L2`'s own shape
- Modify: `gateway/config.example.yaml`
- Create: `gateway/cmd/gateway/lexical_cache_integration_test.go` — a real end-to-end HTTP test proving a lexically-paraphrased repeat is served from L3 (mock-upstream call count stays flat) while an entity-mismatched near-duplicate is a real miss
- Modify: `gateway/ARCHITECTURE.md`, `gateway/changelog/unreleased.md`, `DECISIONS.md`, `docs/agents/LOGS.md`, `STATUS.md`, `AGENTS.md` (if its own "Never" bullet about the hard-gate needs a pointer to this RFC — check first, don't assume)

**Steps:**
- [ ] Wire `CacheL3` config through `buildPipeline` exactly mirroring `CacheL2`'s existing pattern.
- [ ] **The load-bearing new integration test**: send a request, then a second, lexically-paraphrased-but-not-normalization-equivalent request (different word order or a synonym swap, no shared numbers/dates/entities) — assert the second gets a 200 with the cached content and the mock upstream's call count stays at 1 (an L3 hit). Then send a third request differing only in a mentioned dollar amount from the first — assert it's a real miss (call count increments), proving the hard-gate blocks it despite high lexical similarity.
- [ ] Update docs per the RFC's own framing — explicitly note this is `L3-lite` (lexical, not semantic), what's real (MinHash/Jaccard + hard-gate + freshness model) and what's deferred (real embeddings, ANN, the NDSS query↔response consistency classifier) — don't let any doc imply full semantic L3 is done.

**Verify:** re-run Task 4's full verify command once more after wiring + doc edits; push and watch the real CI run to completion before considering this done, per this session's own "don't trust a check until it's actually run" discipline.
