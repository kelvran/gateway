> **For agentic executors:** work through this task-by-task. Task 1 (identity) and Task 2 (budget) are independent of each other and can run in parallel. Task 3 (cache) is independent of both and can also run in parallel with them. Task 4 (config) depends only on Task 1's `VirtualKey` shape. Task 5 (dataplane wiring) depends on Tasks 1-4 all landing first. Task 6 (main.go + integration tests) depends on Task 5. Task 7 (docs) is last.

---

**Goal:** Replace the single static virtual key with multiple statically-configured virtual keys, each with its own per-key rate limit, USD budget, and optional allowed-model list — and close the cross-tenant cache-leakage gap this creates, in the same pass.

**Architecture:** `internal/identity` gains a real `VirtualKey` data model and returns the resolved key from `Verify`. A new `internal/budget` package tracks per-key cumulative spend in memory. `internal/cache.Key()` gains a tenant dimension. `dataplane.Pipeline` wires all of this together: per-key rate limiters, a budget tracker, an allowed-models check, and tenant-namespaced cache keys.

**Tech Stack:** Stdlib only (`crypto/sha256`, `crypto/subtle`, `sync`) — no new dependency, consistent with the rest of `gateway/`.

**Spec:** `docs/rfcs/2026-09-02-virtual-keys-budgets.md` — the exact `VirtualKey`/`Tracker`/`Key()` signatures live there; this plan implements them verbatim, it does not redesign them.

**Global Constraints** (inherited from the spec + `AGENTS.md`):
- Every existing test that authenticates with the old single static key must be updated, not deleted — the new multi-key model must remain provably at least as correct as the old single-key one.
- `cache.Key()`'s new tenant parameter must be exercised by a test proving two different keys asking an identical question get two different cache entries — this is the whole point of this pass, not an incidental detail.
- No secret VALUE (raw virtual key) may appear in any test fixture or config file — only its SHA-256 hash, per the RFC's "why hashes, not env-var names" section.
- `docs/testing/TESTING.md`'s ban on real network calls in CI applies here too — nothing in this plan needs a real upstream provider at all (identity/budget/cache/dataplane are all pure/in-memory).

---

## Task 1 — `internal/identity`: real `VirtualKey` data model

**Files:**
- Modify: `gateway/internal/identity/identity.go`
- Modify: `gateway/internal/identity/identity_test.go` (or create if it's currently inline — check the existing file name first)

**Steps:**
- [ ] Define `VirtualKey{ID, KeyHash, BudgetUSD, AllowedModels map[string]struct{}, RateLimitBurst, RateLimitRefill float64}` exactly as specified in the RFC.
- [ ] `NewVerifier(keys []VirtualKey) (*Verifier, error)`: validate non-empty `keys`, validate each `KeyHash` is a well-formed 64-char hex string, reject duplicate hashes with a new `ErrDuplicateKeyHash`. Internally index by hash (`map[string]*VirtualKey`) for O(1) lookup — per the RFC's Drawbacks section, this supersedes the earlier "loop + ConstantTimeCompare per key" idea; the presented token's own SHA-256 hash is computed once and looked up directly.
- [ ] `Verify(authorizationHeader string) (*VirtualKey, error)`: extract the bearer token (unchanged prefix logic), hash it, look it up. Keep `ErrMissingHeader`/`ErrInvalidKey` as the exact same sentinels callers already depend on.
- [ ] Tests: multiple keys resolve independently and correctly; an unknown token returns `ErrInvalidKey`; missing/malformed header returns `ErrMissingHeader`; `NewVerifier` rejects an empty slice, a malformed hash, and a duplicate hash (3 distinct error-path tests, not one combined one).

**Verify:** `cd gateway && go build ./internal/identity/... && go test ./internal/identity/...`

## Task 2 — `internal/budget` (new package)

**Files:**
- Create: `gateway/internal/budget/budget.go`
- Create: `gateway/internal/budget/budget_test.go`

**Steps:**
- [ ] `Tracker{}` / `NewTracker()` / `Allow(keyID string, capUSD float64) bool` / `Record(keyID string, costUSD float64)`, exactly as specified in the RFC. Mutex-protected `map[string]float64`.
- [ ] `Allow` semantics: `capUSD <= 0` always returns true (unlimited). Otherwise true iff cumulative spend so far is `< capUSD`.
- [ ] Tests: a key with no recorded spend is always allowed under a positive cap; recording spend up to and past the cap flips `Allow` to false at the correct boundary (off-by-one matters here — test the exact boundary, not just "clearly under" and "clearly over"); a zero/negative cap is always allowed regardless of recorded spend; two different key IDs track independently (recording spend for one never affects the other's `Allow` result); concurrent `Record` calls from multiple goroutines never lose an update (a `go test -race` -covered test, not just a sequential one).

**Verify:** `cd gateway && go build ./internal/budget/... && go test ./internal/budget/... -race`

## Task 3 — `internal/cache`: tenant-namespaced `Key()`

**Files:**
- Modify: `gateway/internal/cache/key.go`
- Modify: `gateway/internal/cache/key_test.go`
- Modify: `gateway/internal/cache/key_fuzz_test.go`

**Steps:**
- [ ] Add a leading `tenantID string` parameter to `Key()`; fold it into the hash input alongside the existing four fields (e.g. `"tenant=%s\x00model=%s\x00..."`).
- [ ] Update every existing unit test call site to pass a tenant ID.
- [ ] Add the load-bearing new test: two calls with different `tenantID` but otherwise byte-identical arguments must produce different keys. Also add the inverse sanity check (same `tenantID` + same everything else still produces the same key — `Key` must still be deterministic, not accidentally randomized).
- [ ] Update `FuzzKey`'s seed corpus and fuzz function signature to include a `tenantID` argument; the existing "never panics" / "deterministic" / "64-hex-char output" properties are unchanged, just now exercised across an added dimension.

**Verify:** `cd gateway && go build ./internal/cache/... && go test ./internal/cache/... && go test ./internal/cache/... -fuzz=FuzzKey -fuzztime=15s`

## Task 4 — `internal/gateway/controlplane`: `virtual_keys` config section

**Files:**
- Modify: `gateway/internal/gateway/controlplane/config.go`
- Modify: `gateway/internal/gateway/controlplane/config_test.go`
- Modify: `gateway/internal/gateway/controlplane/config_fuzz_test.go` (only if the fuzzed shape needs updating to stay representative)
- Modify: `gateway/config.example.yaml` (replace `api_key_env` with a `virtual_keys` block using **real hashes of made-up example keys**, never a plaintext example secret)

**Steps:**
- [ ] Remove `Config.APIKeyEnv` (the old single-key field) entirely — this is a breaking format change, deliberately, per the RFC.
- [ ] Add `Config.VirtualKeys []VirtualKeyConfig` where `VirtualKeyConfig` mirrors YAML's shape: `Name` (the mapping key), `KeyHash`, `BudgetUSD`, `AllowedModels []string`, `RateLimitBurst`, `RateLimitRefill`. The hand-rolled `parseYAMLMini` currently has **no list support** (documented in its own doc comment) — `allowed_models: ["gpt-4o"]` needs either (a) a minimal addition to `parseYAMLMini` for flow-style `[a, b, c]` scalars only (no block-style YAML lists, no nesting), or (b) modeling it as a mapping-keyed-by-model-name like `deployments` already does (e.g. `allowed_models: {gpt-4o: true}`). Prefer (b) — it reuses the exact pattern `deployments`/`price_table` already establish, and this parser's whole design philosophy is "support exactly the subset this config's shape needs," not more.
- [ ] `Load` validates: at least one virtual key configured (mirroring the existing "at least one deployment" requirement), each `KeyHash` is present, no duplicate `Name`s (map keys already guarantee this) — duplicate *hash* validation stays in `identity.NewVerifier`, not duplicated here.
- [ ] Tests: loading the updated `config.example.yaml` succeeds and produces the expected `VirtualKeyConfig` values; a config missing `virtual_keys` entirely fails with a clear error; a config with an `allowed_models` mapping round-trips correctly.

**Verify:** `cd gateway && go build ./... && go test ./internal/gateway/controlplane/...`

## Task 5 — Dataplane wiring (depends on Tasks 1-4)

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go`
- Modify: `gateway/internal/gateway/dataplane/streaming.go`
- Modify: `gateway/internal/gateway/dataplane/dataplane_test.go`
- Modify: `gateway/internal/gateway/dataplane/streaming_test.go`

**Steps:**
- [ ] `Config` gains `Budget *budget.Tracker` (required) — `RateLimit` no longer takes a single pre-built `*ratelimit.TokenBucket`; instead `Pipeline` builds one `*ratelimit.TokenBucket` per configured `identity.VirtualKey` at `NewPipeline` time, from that key's `RateLimitBurst`/`RateLimitRefill`, stored in a `map[string]*ratelimit.TokenBucket` keyed by `VirtualKey.ID`. (`Config` needs the verifier's resolved keys available at construction time to build this map — thread `[]identity.VirtualKey` through `Config` alongside the existing `*identity.Verifier`, or expose an accessor on `Verifier`; pick whichever keeps `NewPipeline` simplest, it's an implementation detail the RFC doesn't pin down.)
- [ ] Add sentinels `ErrBudgetExceeded` and `ErrModelNotAllowed` (new, alongside the existing `ErrRateLimited`).
- [ ] `HandleChatCompletion`: `Verify` now returns `(*identity.VirtualKey, error)`. Order of checks after auth: allowed-models → per-key rate limit → budget → cache lookup (now `cache.Key(vk.ID, req.Model, ...)`) → ... unchanged from here. `logRequest` gains a `vk *identity.VirtualKey` parameter, adds `"virtual_key_id", vk.ID` to both its error and success log lines, and calls `p.budget.Record(vk.ID, cost)` on the success path (mirroring the RFC's "call it once cost is known, including on partial/zero-usage requests" note).
- [ ] `HandleChatCompletionStream`: identical set of changes, applied to the streaming entry point — allowed-models/rate-limit/budget checks before the cache lookup, tenant-namespaced cache key, `vk.ID` threaded into its own `logRequest` call.
- [ ] Tests (in `dataplane_test.go`): a request from an unlisted model for a key with a non-empty `AllowedModels` is rejected with `ErrModelNotAllowed` *before* the rate limiter or budget tracker are ever touched (assert this ordering, not just the end error); two different keys' rate limits are provably independent (exhausting key A's bucket never blocks key B); a request from a key at its budget cap returns `ErrBudgetExceeded` and never reaches the cache or upstream; **the load-bearing test**: two different keys sending byte-identical requests each get a cache MISS against the other (proving `cache.Key`'s tenant dimension is actually wired through end-to-end, not just unit-tested in isolation in Task 3) — then each key's own second identical request IS a cache hit. Mirror the same set (adapted for streaming) in `streaming_test.go`.

**Verify:** `cd gateway && go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./...`

## Task 6 — `cmd/gateway/main.go` + integration tests (depends on Task 5)

**Files:**
- Modify: `gateway/cmd/gateway/main.go`
- Modify: `gateway/cmd/gateway/integration_test.go`

**Steps:**
- [ ] `buildPipeline`: build `[]identity.VirtualKey` from `cfg.VirtualKeys` (no env-var resolution needed for the hash itself — it's not a secret, per the RFC), construct the `Verifier`, construct a `budget.Tracker`, wire both into the new `dataplane.Config` shape from Task 5.
- [ ] `writeErrorResponse`: add `errors.Is(err, dataplane.ErrBudgetExceeded) → 429` and `errors.Is(err, dataplane.ErrModelNotAllowed) → 403`.
- [ ] Update every existing integration test helper (`newIntegrationServer`, `newIntegrationServerWithProvider`, `newIntegrationServerAnthropic`) to configure `virtual_keys` instead of the old `api_key_env`/single-key model — these helpers currently generate a plaintext test key string and pass it as `gatewayKey`; they now also need to compute and configure that same string's SHA-256 hash.
- [ ] New integration tests: a request with a valid key for a model outside its `allowed_models` gets a real HTTP 403; a request from a key with an exhausted budget gets a real HTTP 429 with a body distinguishable from a rate-limit 429 (assert the error message content differs, since the status code alone doesn't distinguish the two); two different valid keys sending the identical request each independently get a 200 with a cache MISS the first time (drive this through the mock upstream's call counter exactly like the existing `TestIntegrationRepeatedRequestServedFromCache` does, but with 2 keys × 1 request each → **2** upstream calls, not 1 — proving no accidental cross-tenant cache sharing through the real HTTP path, not just the dataplane unit test from Task 5).

**Verify:** `cd gateway && go build ./... && go vet ./... && go test ./... -v -race && golangci-lint run ./...`

## Task 7 — Docs, Changelog, Wrap-Up

**Files:**
- Modify: `gateway/ARCHITECTURE.md` (mark `/internal/identity`'s virtual-keys/budgets as ACTIVE, `/internal/budget` as a new package, note teams/hierarchical-scope remain target-only; update the Cache Subsystem section's tenant-namespace sentence from aspirational to real)
- Modify: `gateway/config.example.yaml` (already updated in Task 4 — just confirm its comments describe the real, shipped shape, not the old one)
- Modify: `docs/users/USER_GUIDE.md` (how an operator generates a virtual key: `openssl rand -hex 32`, hash it, add it to `virtual_keys`)
- Modify: `gateway/changelog/unreleased.md` (Added entry)
- Modify: `DECISIONS.md` (one line: multi-tenant virtual keys + budgets shipped, cache tenant-namespacing closed in the same pass, distributed rate limiting deferred a second time deliberately)
- Modify: `docs/agents/LOGS.md` (new append-only entry)
- Modify: `STATUS.md` (Current Phase, Verification State, Next Action)

**Verify:** re-run Task 6's full verify command once more after doc edits; cross-reference grep for every new doc's referenced paths (mirroring the exact check used after the streaming pass).
