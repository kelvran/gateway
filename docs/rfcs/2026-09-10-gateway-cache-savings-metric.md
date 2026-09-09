# RFC: "$ saved by cache tier" OTel metric

## Status

Accepted, implemented 2026-09-10.

## Context

A `docs/upgrade-research/cache-next-upgrade-round2-2026-09-09.md` deep-research pass (Finding 2) surfaced a direct code read: `dataplane.finalize`'s cost computation (`cost = p.costCalc.Calculate(...)`) is gated only on `err == nil` — **not** on `billable` — so a cache hit still gets a real, priced notional cost. An existing code comment right above that computation already names the exact intended use: "cost is still computed and reported via telemetry/the log line either way (real, informational 'what this would have cost' data, e.g. for a cache-savings dashboard)." Combined with the already-shipped `kelvran.cache.hit`/`kelvran.cache.layer` attributes on the same span (`telemetry.RecordChatCompletionResult`), Kelvran already has 100% of the raw per-request data needed for a "$ saved by cache tier" metric — the gap is purely that no aggregated instrument exists yet. Kelvran ships zero in-repo dashboard/visualization anywhere (`docs/operations/TELEMETRY.md` states explicitly "no backend has been chosen yet") — this project is OTel-export-only, so the fix is a new exported metric, not a UI.

## Design

A new instrument, `kelvran.cache.savings_usd` (a `Float64Counter`), follows the exact registration pattern every other instrument in `internal/telemetry/telemetry.go` already uses: a package-level `var` constructed once via a `must*` helper (`mustInt64Counter`/`mustFloat64Histogram` already existed; this adds `mustFloat64Counter`, following the identical shape). `RecordCacheSavings(ctx, layer, savingsUSD)` increments it, dimensioned by the same `AttrKelvranCacheLayer` key the span attribute already uses — so one query groups both signals identically. Called from `dataplane.finalize`, right alongside the existing `RecordChatCompletionResult`/`RecordChatCompletionMetrics` calls, gated on `cacheInfo.Hit()` (mirroring `RecordChatCompletionMetrics`'s own "only record when meaningful" convention — a miss would otherwise produce a misleading zero-value data point). `cost` is guaranteed populated at this call site on every real hit: `finalize` only leaves `cost` at its zero value when `err != nil`, and a cache hit path never sets `err`.

This gives an operator's own Prometheus/Grafana a real, already-aggregatable exported counter to sum/graph by layer directly, rather than a raw-span aggregation query they'd have to write themselves.

## Alternatives considered

**Documentation-only (a query recipe in `TELEMETRY.md`, no new instrument)** — rejected: the research's own framing ("the gap is purely an aggregation/dashboard query over already-emitted OTel attributes") technically supports this, but a genuinely new, first-class exported counter is strictly more useful to an operator than "here's a query you write yourself over raw span/log data" — and costs one small, well-precedented addition following an existing pattern.

**An `ObservableCounter` (callback-based) instead of a plain `Float64Counter`** — rejected: there's no natural "current total" state to observe on a callback cadence here; the value genuinely accumulates one increment per request, exactly matching `Float64Counter`'s own semantics and every other counter this package already ships.

## Verification

New `mustFloat64Counter` helper mirrors `mustInt64Counter`/`mustFloat64Histogram` exactly. The new counter is verified by **extending** the existing shared test, `TestRecordCacheL3GateOutcomeIncrementsPerGateAndOutcome` — per that test's own documented gotcha, OTel Go's global meter only delegates to a real `MeterProvider` once per test binary, so a separate new test function would silently pass while verifying nothing. Added `RecordCacheSavings` calls (two for `"L1"`, one for `"L3"`, zero for `"L2"`) inside the existing swap window, and a new `kelvran.cache.savings_usd` collection branch (`metricdata.Sum[float64]`, distinct from the L3-gate-outcome counter's `Sum[int64]`) asserting per-layer sums match exactly, including that `"L2"` (never recorded) sums to zero. Full `cd gateway && go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./... && go run github.com/fe3dback/go-arch-lint@v1.18.0 check && gofmt -l . && go mod tidy` clean except the same pre-existing, already-documented rootless-Docker environmental failures.
