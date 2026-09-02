> **For agentic executors:** Task 1 is foundational (the storage layer) and must land before Task 2 (`Tracker` wiring), which must land before Tasks 3/4. Task 5 is last.

---

**Goal:** `internal/budget.Tracker` survives a process restart when configured with a `persist_path`, with zero behavior change when it isn't.

**Architecture:** A new `internal/budget/boltstore` package implements a small `Store` interface (`Load`/`Save`/`Close`) over `go.etcd.io/bbolt`; `budget.Tracker` gains an optional `Store` field, a new `NewTrackerWithStore` constructor, and a `Close` method; `dataplane.Pipeline` gains a `Close` method; `cmd/gateway` wires the config-driven choice.

**Tech Stack:** `go.etcd.io/bbolt` — the gateway's third external Go dependency, near-zero *new* transitive weight (its one real dependency, `golang.org/x/sys`, is already pulled in via OTel).

**Spec:** `docs/rfcs/2026-09-03-budget-persistence.md` — the exact interface/type signatures and the Postgres-vs-bbolt tradeoff live there; this plan implements them verbatim.

**Global Constraints** (inherited from the spec + `AGENTS.md`):
- `budget.NewTracker()` (no store) must keep behaving exactly as it does today — every existing test and call site that never opts into persistence must need zero changes.
- Spend is stored as the exact decimal string (`spent.String()`), never a numeric encoding of any kind — the same precision discipline as the Decimal-cost-accounting and OTel-attribute RFCs.
- `Allow` must gain zero new I/O — persistence only touches the `Record` write path.
- A `Save` failure must be logged and must not fail the request — the in-memory state is already correct; only future restart-durability for that one update is at risk.

---

## Task 1 — `go.etcd.io/bbolt` + `internal/budget/boltstore`

**Files:**
- Modify: `gateway/go.mod`, `gateway/go.sum` (via `go get`)
- Create: `gateway/internal/budget/boltstore/boltstore.go`
- Create: `gateway/internal/budget/boltstore/boltstore_test.go`

**Steps:**
- [ ] `go get go.etcd.io/bbolt` from `gateway/`; `go mod tidy`; confirm (don't assume) the actual net-new transitive dependency count is small, per the RFC's claim.
- [ ] `Open(path string) (*Store, error)`: opens the bbolt file (`0o600`), creates a single bucket (e.g. `"spend"`) if absent.
- [ ] `(*Store) Load(ctx) (map[string]decimal.Decimal, error)`: iterates the bucket, parsing each value via `decimal.NewFromString` — a corrupt (non-decimal) stored value returns a clear wrapped error naming the offending key, never a silent zero or a panic.
- [ ] `(*Store) Save(ctx, keyID string, spent decimal.Decimal) error`: upserts `[]byte(keyID) -> []byte(spent.String())` in one bbolt transaction.
- [ ] `(*Store) Close() error`.
- [ ] Tests: fresh-DB `Load` returns an empty map; `Save` then `Load` round-trips a single key exactly; calling `Save` twice for the same key upserts (one entry, latest value) rather than duplicating; a value with many decimal places (e.g. `"0.0000075"`) round-trips byte-for-byte, proving no numeric coercion anywhere in the path. **The load-bearing test**: `Save` some keys, `Close`, `Open` a *new* `*Store` against the *same file path*, `Load` — confirm every key/value survived, the literal proof this feature exists to deliver. A corrupt-value test: write a non-decimal byte string directly into the bucket (bypassing the `Store` API) and confirm `Load` returns an error, not a panic or a silent zero.

**Verify:** `cd gateway && go build ./internal/budget/boltstore/... && go test ./internal/budget/boltstore/...`

## Task 2 — `internal/budget.Tracker`: optional `Store`, `Close`, persisting `Record`

**Files:**
- Modify: `gateway/internal/budget/budget.go`
- Modify: `gateway/internal/budget/budget_test.go`

**Steps:**
- [ ] Define the `Store` interface exactly as specified in the RFC.
- [ ] `Tracker` gains `store Store` and `logger *slog.Logger` fields (both nil-safe: `NewTracker()` leaves both nil, preserving today's exact behavior).
- [ ] `NewTrackerWithStore(ctx, store Store, logger *slog.Logger) (*Tracker, error)`: calls `store.Load`, hydrates `spent` from the result (empty map if `Load` returns nil), defaults `logger` to `slog.Default()` if nil.
- [ ] `Record`: after updating the in-memory `spent` map (unchanged logic), if `t.store != nil`, call `store.Save(context.Background(), keyID, newTotal)` synchronously; on error, `t.logger.Warn("budget_persist_failed", "key_id", keyID, "error", err.Error())` — the request-handling caller never sees this error, per the RFC's explicit "log-and-continue" design.
- [ ] `Close() error`: no-op if `t.store == nil`; otherwise `t.store.Close()`.
- [ ] Tests (using a small fake in-memory `Store` defined in this test file — keeping `internal/budget`'s own tests independent of `boltstore`, matching this project's existing dependency-direction discipline): `NewTrackerWithStore` hydrates pre-existing spend, immediately reflected in `Allow`; each `Record` call invokes the fake store's `Save` with the correct cumulative total, not just the delta; a `Save`-always-errors fake store proves `Record` still completes (doesn't panic, doesn't block) and logs a warning (assert against a `slog.Logger` writing to a buffer, not just "didn't crash"); `Close` calls through to the fake store's `Close`.

**Verify:** `cd gateway && go build ./internal/budget/... && go test ./internal/budget/... -race`

## Task 3 — `internal/gateway/controlplane` config + `dataplane.Pipeline.Close`

**Files:**
- Modify: `gateway/internal/gateway/controlplane/config.go`
- Modify: `gateway/internal/gateway/controlplane/config_test.go`
- Modify: `gateway/internal/gateway/dataplane/dataplane.go`
- Modify: `gateway/config.example.yaml` (add a commented-out `budget:` section — commented out, not enabled, so the example config's own behavior doesn't change)

**Steps:**
- [ ] `BudgetConfig{PersistPath string}`; `Config.Budget BudgetConfig`; parse a `budget:` section with `persist_path` (plain `getString` — no new parser work, this isn't a money field).
- [ ] `dataplane.Pipeline.Close() error`: cascades to `p.budget.Close()` if the budget field is non-nil (it's always non-nil per `NewPipeline`'s existing validation — confirm, don't assume, this can't be nil at the point `Close` might be called).
- [ ] Tests: a config with a `budget: { persist_path: "..." }` section parses correctly; a config without one leaves `Config.Budget` at its zero value (mirroring the existing `TestLoadWithoutTelemetrySectionDefaultsToZeroValue` pattern for the same "genuinely optional section" proof).

**Verify:** `cd gateway && go test ./internal/gateway/controlplane/... && go test ./internal/gateway/dataplane/...`

## Task 4 — `cmd/gateway` wiring + the real restart-survival integration test (depends on Tasks 1-3)

**Files:**
- Modify: `gateway/cmd/gateway/main.go`
- Modify: `gateway/cmd/gateway/integration_test.go`

**Steps:**
- [ ] `buildPipeline`: if `cfg.Budget.PersistPath != ""`, open a `boltstore.Store` at that path and construct the tracker via `budget.NewTrackerWithStore`; otherwise `budget.NewTracker()` exactly as today. Wrap and return any open/hydration error clearly.
- [ ] `run()`: `defer func() { _ = pipeline.Close() }()` after `buildPipeline` succeeds — the same best-effort caveat as the OTel shutdown, stated in a comment, not just implied.
- [ ] **The load-bearing new integration test**: `TestIntegrationBudgetPersistsAcrossRestart` — build a config with `budget.persist_path` pointing at a `t.TempDir()` file and a tight `budget_usd` cap, build a pipeline (`buildPipeline`, simulating gateway instance #1), send one real HTTP request that spends most of the cap, call `pipeline.Close()` (simulating a clean shutdown), then `buildPipeline` a **second, independent** `*dataplane.Pipeline` from a config pointing at the **same** `persist_path` (simulating a restart) wired into a **new** `httptest.Server`, and send one more request against it — assert the second instance's budget enforcement already reflects the first instance's spend (e.g., the cap is now exceeded, or the remaining headroom matches exactly), proving state genuinely survived a full close-and-reopen cycle through the real HTTP stack, not just at the storage-layer unit-test level from Task 1.

**Verify:** `cd gateway && go build ./... && go vet ./... && go test ./... -v -race && golangci-lint run ./...`

## Task 5 — Docs, Changelog, Wrap-Up

**Files:**
- Modify: `gateway/internal/budget/budget.go`'s package doc comment (remove the now-fulfilled "deliberately not persisted... Phase 2 gap" note, replace with what's real)
- Modify: `gateway/ARCHITECTURE.md` (mark budget persistence as real-but-scoped; note the bbolt dependency and the explicit Postgres-stepping-stone framing)
- Modify: `gateway/changelog/unreleased.md` (Added entry)
- Modify: `DECISIONS.md` (one line: bbolt over SQLite/Postgres-now, why, single-instance scope)
- Modify: `docs/agents/LOGS.md` (new append-only entry)
- Modify: `STATUS.md` (Current Phase, Verification State, Next Action)

**Verify:** re-run Task 4's full verify command once more after doc edits; cross-reference grep for every new doc's referenced paths.
