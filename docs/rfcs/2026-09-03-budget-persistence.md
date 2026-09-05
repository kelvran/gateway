- **Status**: accepted
- **Date**: 2026-09-03
- **Author(s)**: project founder + Claude Code

## Summary

Give `internal/budget.Tracker` an optional, embedded, restart-durable backing store (`go.etcd.io/bbolt`), so a virtual key's cumulative spend survives a gateway process restart instead of silently resetting to zero. Scope is deliberately narrow: **single-instance restart-durability**, not multi-instance/distributed consistency — that's the separately-tracked, not-yet-picked "distributed rate limiting" roadmap item, and this RFC does not presuppose or partially preempt it.

## Motivation

`internal/budget`'s own package doc comment has said, since the virtual-keys-and-budgets RFC, that this is "deliberately not persisted: a gateway restart resets every key's spend to zero... a documented Phase 2 gap, not a silently accepted one." Now that spend is Decimal-precise (`docs/rfcs/2026-09-02-decimal-cost-accounting.md`), the next real gap in the same "agent-run cost accountability" story `PRD.md` leads with is that the number is exact but not durable — a deploy, crash, or scale event zeroes every key's spend, and `THREAT_MODEL.md`'s LLM10 Unbounded Consumption row already names "budgets" as an existing control this silently weakens every time the process restarts.

## Detailed Design

### Why not the already-committed Postgres control-plane store?

`gateway/ARCHITECTURE.md`'s Tech Stack table already names `Postgres (pgx/sqlc)` for "Control-plane config store" — this RFC does not build that. A real Postgres integration (connection pooling, `sqlc`-generated queries, migrations, and a running Postgres instance required for local dev and every test) is a proportionate answer to the *control-plane config* problem (deployments, virtual keys, price tables — all still static YAML today), but it is a disproportionate answer to *this* narrower problem: durably persisting one `map[string]decimal.Decimal`, upserted once per completed request. Requiring a running Postgres just to keep budget numbers across a restart would also add a new hard operational dependency to every deployment, including ones that don't need it — the same reasoning that kept OTel's default exporter at `stdout` and kept `shopspring/decimal` dependency-free applies here too.

This RFC is an explicit, bounded stepping stone: an embedded store scoped to exactly the data that needs restart-durability today, not a rejection of the Postgres commitment. If/when a real Postgres-backed control plane lands, migrating budget spend into it is a natural, much smaller follow-up than building the whole control plane just to fix this one gap now. This mirrors two precedents this project already set deliberately: Cache is embedded in Gateway with a dormant gRPC extraction seam (`docs/decisions/0002-cache-embedded-in-gateway.md`), and virtual keys are static YAML with the real control-plane API explicitly deferred (`gateway/ARCHITECTURE.md`'s `/internal/admin` line) — ship the minimal real thing now, document the upgrade path, don't build the big thing speculatively.

### Library choice: `go.etcd.io/bbolt`, not SQLite

The initial lean going into this RFC's research was an embedded SQLite driver (`modernc.org/sqlite`, pure-Go/no-CGO). Research changed that: `modernc.org/sqlite`'s own documentation flags a real, non-trivial maintenance burden — *"you should use in your go.mod file the exact same version of modernc.org/libc as seen in the go.mod file of this repository"* (a tracked upstream fragility, not a hypothetical) — and it pulls in a meaningfully heavier transitive tree (a full CGo-free transpilation of the SQLite C amalgamation) for a use case that needs no SQL at all: one key, one value, upsert-by-key.

`go.etcd.io/bbolt` is a better fit specifically for this narrow shape: pure Go, near-zero transitive dependencies (its only real dependency, `golang.org/x/sys`, is already in this module's dependency graph via OTel — so this RFC adds no *new* transitive dependency at all), a stable file format and API for years, battle-tested at scale (etcd itself, among others), and a transactional `Update`/`View` API that already gives exactly the "commit durably before the caller gets control back" guarantee this design wants, with no hand-rolled WAL/crash-recovery logic to get wrong.

### `Store` interface and `boltstore` implementation

```go
// internal/budget: Store persists budget spend durably across process
// restarts. Optional — a Tracker constructed via NewTracker (no store)
// is unchanged: pure in-memory, resetting on restart, exactly as today.
type Store interface {
    Load(ctx context.Context) (map[string]decimal.Decimal, error)
    Save(ctx context.Context, keyID string, spent decimal.Decimal) error
    Close() error
}
```

`internal/budget/boltstore` implements `Store` over a single bbolt bucket, keyed by `keyID` (raw bytes), valued by **the exact decimal string** (`spent.String()`, raw bytes) — never a numeric encoding of any kind. This is deliberate and load-bearing: bbolt has no notion of a "float column" to misuse the way a naive SQL schema might, but the discipline is stated explicitly anyway, matching the same precision-preservation reasoning as the YAML-parser fix and the OTel-attribute string choice in the two prior RFCs — the boundary between Kelvran's money type and any external representation must never round-trip through anything but decimal text.

### `budget.Tracker` changes

```go
func NewTrackerWithStore(ctx context.Context, store Store, logger *slog.Logger) (*Tracker, error)
// Loads existing spend immediately (a restart resumes exactly where it
// left off) and hooks Record to persist synchronously — see below.

func (t *Tracker) Close() error // closes the underlying store, if any
```

`Record` persists **synchronously, before returning** when a store is configured — no async-flush window, no batch/debounce that could lose the last few updates on a crash. This is a deliberate correctness-over-throughput choice: bbolt's own write path is fast enough at Kelvran's current scale (a single gateway instance, one write per completed request) that the latency cost is acceptable, and getting a genuine zero-data-loss guarantee for free from bbolt's transactions is worth more than shaving the write off the hot path — revisit only if real production latency numbers say otherwise (see Unresolved Questions).

A persistence failure (`Save` returns an error) is logged (`budget_persist_failed`, at Warn level) and the request still completes normally — the in-memory `spent` map is already correct for enforcement purposes; only *future* restart-durability for that one update is at risk, which is a real but non-fatal degradation, not a reason to fail a request that already has a correct in-memory answer.

`Allow` is completely unchanged — it never touches the store, so the hot *read* path (checked on every request, before any upstream call) has zero new I/O. Only `Record` (called once per completed request, in the already-deferred `finalize` step) gets the new write.

### Config and wiring

```yaml
budget:
  persist_path: "kelvran-budget.db"   # optional; omit for pure in-memory (today's behavior)
```

`buildPipeline` (not a new call in `run()`, unlike `telemetry.Init` — this has no global side effect, so it stays exactly where every other dependency `buildPipeline` already constructs lives) opens the store when `persist_path` is set, otherwise constructs `budget.NewTracker()` exactly as today — every existing integration-test call site that never sets this field needs zero changes. `dataplane.Pipeline` gains a `Close() error` (cascading to `budget.Tracker.Close()`), and `run()` defers it. Originally best-effort only (exercised on a clean `ListenAndServe` return, not a real SIGTERM), matching the OTel RFC's shutdown func's own original caveat — both became real on every exit path once `docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md`'s graceful-shutdown gap was closed on 2026-09-05.

## Drawbacks

- Third external Go dependency family (after OTel, `shopspring/decimal`) — mitigated: near-zero *new* transitive weight, since `golang.org/x/sys` is already pulled in.
- Synchronous per-request persistence adds write latency to every completed request when enabled. Accepted for now; not measured against real production traffic yet (there is none) — see Unresolved Questions.
- Single-instance only. Running more than one gateway instance against the same `persist_path` file is unsupported and unsafe (bbolt takes an exclusive file lock — a second instance would fail to open the file, not silently corrupt it, which is a safe failure mode, but still a real deployment-topology constraint worth stating plainly). Multi-instance budget consistency is explicitly not this RFC's problem to solve.
- Diverges (temporarily, by design) from `gateway/ARCHITECTURE.md`'s Postgres control-plane commitment for this one narrow slice of state — addressed above, not glossed over.

## Alternatives Considered

1. **Wait for/build the real Postgres control-plane store now instead** — rejected as disproportionate: this RFC's whole problem is "one map, upserted per-key," and Postgres integration would add a hard external-service dependency to every deployment for a data shape that doesn't need a relational database.
2. **`modernc.org/sqlite`** — rejected after research surfaced its own documented `modernc.org/libc` version-pinning fragility and heavier transitive footprint, for a use case with no actual query needs.
3. **A flat JSON snapshot file** — rejected: no built-in crash-atomicity (a write interrupted mid-flush corrupts the *entire* file, every key, not just the one being updated); getting real atomicity back would mean hand-rolling write-to-temp-then-rename discipline, at which point bbolt already provides a better-tested version of the same guarantee for free.
4. **Async/batched persistence (write every N seconds or N calls instead of every Record call)** — rejected for v1: reintroduces exactly the data-loss window this RFC exists to close. A real per-request latency complaint, backed by real numbers, would be the trigger to revisit this — not assumed now.

## Unresolved Questions

- Real p99 latency impact of synchronous per-request bbolt writes, once there's real production traffic to measure against — none exists yet.
- Whether `persist_path` should default to *something* (e.g. `./kelvran-budget.db` next to the config file) rather than requiring explicit opt-in — left as opt-in-only for v1, so a bare `config.yaml` with no `budget:` section behaves identically to before this RFC, with zero surprise new files appearing on disk.
- Whether/when budget spend should migrate into the eventual real Postgres control-plane store, superseding this bbolt-backed interim store — not decided here, tracked only as the natural follow-up this RFC's Motivation section already names.
