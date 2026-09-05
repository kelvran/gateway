# RFC: Soft budget-cap warning threshold (log-only)

## Status

Accepted, implemented 2026-09-05.

## Context

`docs/rfcs/2026-09-02-virtual-keys-budgets.md`'s own Unresolved Questions named this gap explicitly: "Whether `ErrBudgetExceeded` should eventually distinguish 'hard cap, request rejected outright' from 'soft cap, request allowed but flagged' — left as hard-cap-only for v1; no evidence yet that a soft-budget mode is needed." The fresh backlog audit re-surfaced it (item 7). A ground-truth research pass confirmed `budget.Tracker.Allow` is still a pure boolean today — no intermediate state exists anywhere.

Resolved via an explicit brainstorming check-in (`AskUserQuestion`), presenting 4 options ranging from "no soft cap" to "log + event + metric": the user chose **log only** — allow the request, emit a structured warning when spend crosses a new threshold, no new API surface (no `GatewayDecisionEvent` field, no OTel counter).

## Design

### `BudgetWarnPercent`: a percentage of `BudgetUSD`, not a second absolute dollar value

`identity.VirtualKey`/`controlplane.VirtualKeyConfig`/`admin`'s `virtualKeyRequest` all gain `BudgetWarnPercent float64` (config key `budget_warn_percent`, e.g. `0.8` for 80%). Deliberately a *percentage of `BudgetUSD`*, not a second absolute USD figure: an absolute threshold would silently drift out of sync if `BudgetUSD` is ever changed (e.g. raising a key's cap from $100 to $200 would leave a stale $80 "warning" threshold now meaning 40%, not 80%) — a percentage stays proportional automatically. Zero (the default) disables the warning entirely, matching this codebase's existing "0 = disabled/unlimited" convention already used for `BudgetUSD` and `BudgetResetIntervalSeconds`.

Wired into **both** places a virtual key's budget fields already exist: `cmd/gateway/main.go`'s YAML-config construction and `internal/admin`'s live `POST /admin/virtual_keys/{name}` mutation surface — a feature added to one without the other would be a silent asymmetry against the Admin API's own "matches config capability" intent.

### `checkBudgetWarnThreshold`: called from `finalize`, right after the real charge

A new `Pipeline.checkBudgetWarnThreshold(vk)` is called from `finalize`, immediately after `p.budget.Record`, only on a billable completion (mirroring `docs/rfcs/2026-09-05-gateway-cost-double-counting.md`'s own `billable` gate — a cache hit or coalesced follower never re-triggers this either, since spend didn't actually change). It reads the *post-charge* spend via `p.budget.SpentUSD`, compares against `BudgetUSD.Mul(decimal.NewFromFloat(BudgetWarnPercent))`, and logs `budget_warn_threshold_crossed` (with `key_id`/`spent_usd`/`budget_usd`/`warn_percent`) when spend is at or above that line. Nothing about the request itself changes — the response, `err`, and every other `finalize` step run identically regardless.

Deliberately re-logs on **every** billable completion while spend remains over threshold, rather than tracking "already warned this period" state — the simplest correct behavior, and matches this codebase's own existing precedent at `checkRateLimit`'s `ratelimit_backend_unavailable` line, which also logs every occurrence rather than only the first. Tracking per-period warn-state would need new persisted state for a purely observational, log-only feature — out of proportion to what was asked for.

## Alternatives considered

**No soft cap** — rejected by the user's explicit choice.

**Log + event field** (add `budget_warn_crossed` to `GatewayDecisionEvent`) / **log + event + metric** (also an OTel counter, mirroring `kelvran.ratelimit.fail_open`) — both rejected by the user's explicit choice of the plain log-only option; either remains a small, well-scoped follow-on RFC if ever needed.

**An absolute second USD threshold instead of a percentage** — rejected; see "Design" above for the drift hazard this specifically avoids.

**Checking pre-charge (`budgetSpentAtDecision`) instead of post-charge** — rejected; the pre-charge value doesn't yet reflect the very request that might cross the threshold, so it would lag by one request rather than accurately reporting "you just crossed it."

## Verification

`go build ./... && go vet ./...` clean. `golangci-lint run ./...` → `0 issues`. `go run github.com/fe3dback/go-arch-lint@v1.18.0 check` → clean. `go mod tidy` → empty diff (no new dependency). `go test ./... -race` → every package `ok` except the same pre-existing, already-documented rootless-Docker failure.

Extended `internal/gateway/controlplane/config_test.go`'s `TestLoadExampleConfig` with a `BudgetWarnPercent == 0.8` assertion against the new `config.example.yaml` field. New `internal/gateway/dataplane/budget_warn_threshold_test.go`: `TestFinalizeLogsBudgetWarnThresholdCrossed` (a real nonzero-priced pipeline; a single request whose cost pushes spend at/above threshold logs the warning, and still succeeds) and `TestFinalizeDoesNotLogBudgetWarnBelowThreshold` (the negative case — spend far below threshold never logs it). Sanity-checked by temporarily removing the `checkBudgetWarnThreshold` call: the positive test failed with the log buffer missing the expected line entirely, then restored.
