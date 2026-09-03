> **For agentic executors:** Task 1 (the `inprocess.Cache` capacity bound) must land before Task 2 (`NormalizedKey`/`NormalizeMessages`), which must land before Task 3 (`Pipeline` wiring). Task 4 is config; Task 5 is last.

---

**Goal:** A request that misses L1 (byte-exact) but matches a prior request after a *conservative*, collision-free normalization hits L2 instead of the real upstream — with zero risk of serving a wrong-content cached answer, including for messages containing pasted code.

**Architecture:** `internal/cache/inprocess.Cache` gains real LRU eviction (a direct prerequisite, not new scope creep — see the RFC's Drawbacks). `internal/cache/key.go` gains `NormalizedKey`/`NormalizeMessages` implementing exactly a 3-operation allowlist (outer whitespace trim, Unicode NFC, trailing terminal punctuation strip on the last message only). `dataplane.Pipeline` gains `CacheL2 cache.Cache` and two shared helpers (`checkCache`, `writeCache`) replacing the duplicated L1-only logic in both `dataplane.go` and `streaming.go`.

**Tech Stack:** `container/list` (stdlib, no new dependency) for LRU; `golang.org/x/text/unicode/norm` for Unicode NFC — check whether this is already a transitive dependency (likely, via OTel's own dependency tree) before treating it as a new one.

**Spec:** `docs/rfcs/2026-09-03-cache-l2-normalized-match.md` — the exact allowlist, its narrower-than-the-grounding-research scope and why, and the capacity-bound design live there; this plan implements them verbatim.

**Global Constraints** (inherited from the spec + `AGENTS.md`):
- The normalization allowlist is EXACTLY 3 operations (outer trim, NFC, trailing terminal punctuation strip on the last message). No whitespace collapsing, no case-folding, no anything else — a future addition needs a new RFC, not a quiet extension here.
- `NormalizedKey` must fold `tenantID` in as the leading hash input, identical to `Key` — no codepath may omit it, per `THREAT_MODEL.md`'s KeyPooling finding.
- `cache.Cache` (`port.go`) itself is untouched — L2 is a second instance behind the same interface, never a widened interface.
- A bare `config.yaml` with no `cache:` section must behave identically to today's L1-only behavior, except now capacity-bounded (a safety improvement, not an opt-in feature) rather than truly unbounded.
- Both `dataplane.go` and `streaming.go` must go through the same shared `checkCache`/`writeCache` helpers — no duplicated cache-lookup logic between the buffered and streaming paths.

---

## Task 1 — `inprocess.Cache` capacity bound (LRU)

**Files:**
- Modify: `gateway/internal/cache/inprocess/inprocess.go`
- Modify: `gateway/internal/cache/inprocess/inprocess_test.go` (or create if none exists — check first)

**Steps:**
- [ ] Check for an existing `inprocess_test.go`; read its current tests before changing `New`'s signature, since every existing call site (including other tests) will need updating for the new `maxEntries` parameter.
- [ ] Add a doubly-linked list (`container/list`) tracking recency; `Get` on a hit moves the entry to the front; `Put` inserts at front, evicts from the back when `len(entries) > maxEntries`.
- [ ] `New(maxEntries int) *Cache` / `NewWithClock(maxEntries int, now func() time.Time) *Cache` — `maxEntries <= 0` maps to a default of 10,000, never "unbounded."
- [ ] Update every existing call site of `inprocess.New()` (search the whole module, not just dataplane — tests too) to pass an explicit `maxEntries`.
- [ ] Tests: capacity eviction removes the *least-recently-used* entry, not an arbitrary one (verify by `Get`-touching an old entry to make it recently-used, then confirming a *different*, untouched entry gets evicted on overflow — not the touched one); confirm existing TTL-expiry tests still pass unmodified in behavior (only the constructor signature changes); a `maxEntries <= 0` construction defaults to 10,000, verified by inserting more than that many entries and confirming eviction kicks in at the right boundary (a smaller test constant, not literally 10,001 entries, is fine — verify the *default value* separately, e.g. via a package-level constant test).

**Verify:** `cd gateway && go build ./internal/cache/... && go test ./internal/cache/... -race`

## Task 2 — `NormalizedKey` + `NormalizeMessages` (depends on Task 1 only for module-wide build consistency, not logically)

**Files:**
- Modify: `gateway/internal/cache/key.go`
- Modify: `gateway/internal/cache/key_test.go` (or create if none exists)

**Steps:**
- [ ] Confirm whether `golang.org/x/text/unicode/norm` is already present (direct or transitive) via `go list -m all` before running `go get` — the RFC's Tech Stack note flags this as likely-already-present but unverified.
- [ ] `NormalizeMessages(messages []adapter.Message) string`: for each message, trim outer whitespace from `Content`, apply `norm.NFC.String(...)`; for the LAST message only, after trimming, strip a single trailing `.`/`!`/`?` if present. Serialize the result the same deterministic way `serializeMessages` already does (reuse that function or an equivalent — don't invent a second serialization format).
- [ ] `NormalizedKey(tenantID, model, normalizedMessages string, temperature *float64, maxTokens *int) string` — identical structure to `Key`, same tenant-first hash input discipline, same delimiter scheme.
- [ ] Tests: `NormalizeMessages` on a message with leading/trailing whitespace, mixed NFC-equivalent Unicode forms, and a trailing `?` produces the expected normalized string; **the load-bearing safety test**: two messages containing Python code blocks that differ ONLY in indentation depth (e.g. 2-space vs 4-space) must produce DIFFERENT normalized output (proving the allowlist's deliberate scope-narrowing actually holds in code, not just in the RFC's prose) — this is the single most important test in this whole feature, since it's the concrete case that motivated narrowing the allowlist in the first place. `NormalizedKey` includes `tenantID` as a leading hash input, verified by confirming two different tenants with byte-identical normalized messages produce different keys (mirroring `TestKey`'s existing cross-tenant proof for `Key`, if one exists — check first).

**Verify:** `cd gateway && go build ./internal/cache/... && go test ./internal/cache/... -race`

## Task 3 — `Pipeline` wiring: `checkCache`/`writeCache`, both call sites (depends on Tasks 1-2)

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go`
- Modify: `gateway/internal/gateway/dataplane/streaming.go`
- Modify: `gateway/internal/gateway/dataplane/dataplane_test.go` / `streaming_test.go` (test-helper updates for the new `Config.CacheL2` field)
- Create: `gateway/internal/gateway/dataplane/cache_l2_test.go` (or similar — new tests specific to this wiring)

**Steps:**
- [ ] `Config` gains `CacheL2 cache.Cache` (required, validated in `NewPipeline` like every other dependency — no silent "L2 disabled if nil" mode; `cmd/gateway`'s wiring always constructs one, matching how `Config.Limiter`/`Config.Budget` are always pre-built).
- [ ] `checkCache(ctx, l1Key, l2Key string) (cached []byte, hit bool)`: checks `p.cache.Get` first; on miss, checks `p.cacheL2.Get`; on an L2 hit, best-effort promotes into L1 (`_ = p.cache.Put(ctx, l1Key, cached, p.cacheTTL)`) before returning.
- [ ] `writeCache(ctx, l1Key, l2Key string, encoded []byte)`: best-effort `Put` to both `p.cache` and `p.cacheL2`, using each layer's own configured TTL (`p.cacheTTL` for L1, a new `p.cacheL2TTL` for L2).
- [ ] Update `HandleChatCompletion` (`dataplane.go`) and `HandleChatCompletionStream` (`streaming.go`): both now compute `l1Key := cache.Key(...)` (unchanged) AND `l2Key := cache.NormalizedKey(vk.ID, req.Model, cache.NormalizeMessages(req.Messages), req.Temperature, req.MaxTokens)`, call `checkCache` instead of the old single `p.cache.Get`, and `writeCache` instead of the old single `p.cache.Put`.
- [ ] Tests: an L1 miss + L2 hit returns the cached response AND results in a subsequent byte-identical repeat becoming an L1 hit (proving promotion actually happened — check via a second request and confirming, e.g., a counter/mock proves L2 wasn't consulted the second time); a genuine miss at both layers, after a real upstream call, results in the response being retrievable from EITHER an exact repeat (L1) or a normalized-but-not-exact repeat (L2) on subsequent requests; the existing cross-tenant cache-isolation tests (`TestHandleChatCompletionCacheIsolatedAcrossVirtualKeys` and its streaming mirror) still pass unmodified in behavior, now also proven for L2 specifically (two different tenants, normalized-equivalent-but-not-identical messages, must NOT share a cache entry).

**Verify:** `cd gateway && go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./...`

## Task 4 — `controlplane` config (depends on Task 3)

**Files:**
- Modify: `gateway/internal/gateway/controlplane/config.go`
- Modify: `gateway/internal/gateway/controlplane/config_test.go`
- Modify: `gateway/config.example.yaml` (commented-out `cache:` section, matching the `rate_limit:`/`budget:` precedent — not enabled by default)

**Steps:**
- [ ] `CacheConfig{TTLSeconds, MaxEntries int; L2 CacheL2Config}`, `CacheL2Config{TTLSeconds, MaxEntries int}`; `Config.Cache CacheConfig`; parse a `cache:` section (`ttl_seconds`, `max_entries`, nested `l2: { ttl_seconds, max_entries }`) — all optional, all defaulting exactly as the RFC specifies when omitted.
- [ ] Tests: a config with a full `cache:` section (including nested `l2:`) parses every field correctly; a config without one leaves `Config.Cache` at its zero value (mirroring the existing `TestLoadWithoutTelemetrySectionDefaultsToZeroValue` pattern).

**Verify:** `cd gateway && go test ./internal/gateway/controlplane/...`

## Task 5 — `cmd/gateway` wiring + docs, changelog, wrap-up (depends on Task 4)

**Files:**
- Modify: `gateway/cmd/gateway/main.go` (`buildPipeline`: construct `CacheL2` via `inprocess.New(...)` using `cfg.Cache.L2.MaxEntries` — defaulted per the RFC if zero; resolve L1's own `MaxEntries`/`TTLSeconds` from `cfg.Cache` the same way, replacing the currently-hardcoded 5-minute default with a config-driven one that still defaults to 5 minutes when unset)
- Modify: `gateway/cmd/gateway/integration_test.go` (or a new integration test file) — a real end-to-end HTTP test proving a normalized-but-not-exact repeat request gets a cache hit (mock-upstream call count stays at 1)
- Modify: `gateway/ARCHITECTURE.md`, `gateway/changelog/unreleased.md`, `DECISIONS.md`, `docs/agents/LOGS.md`, `STATUS.md`

**Steps:**
- [ ] Wire `CacheL2`/capacity/TTL config through `buildPipeline` exactly as above.
- [ ] **The load-bearing new integration test**: send a request, then a second request that's byte-different but normalization-equivalent (e.g. added trailing whitespace and a trailing `?`) against the SAME virtual key — assert the second request gets a 200 with the same content and the mock upstream's call count stays at 1 (an L2 hit, not a real second upstream call). Also send a third request with genuinely different content (proving no false-positive collision) and confirm the mock upstream call count increments — a real miss, not an over-eager match.
- [ ] Update docs per the RFC's own framing — explicitly note both what's real (3-operation allowlist, LRU-bounded L1+L2) and what's deferred (whitespace-collapse/case-folding, pending safe code-detection).

**Verify:** re-run Task 3's full verify command once more after wiring + doc edits; push and watch the real CI run to completion before considering this done, per this session's own "don't trust a check until it's actually run" discipline.
