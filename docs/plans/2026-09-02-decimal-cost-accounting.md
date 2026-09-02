> **For agentic executors:** Task 1 is foundational (the parser fix + config types) and must land before Tasks 2/3/4 can compile against real config data, though the leaf-package type changes in Tasks 2/3/4 are independent of EACH OTHER and can run in parallel once Task 1's `decimal.Decimal` dependency is added. Task 5 (dataplane wiring) depends on 2/3/4 all landing. Task 6 depends on 5. Task 7 is last.

---

**Goal:** Replace every dollar-denominated `float64` with `decimal.Decimal`, and fix the YAML parser's eager `float64` conversion (and its boolean/numeric-string collision bug) that would otherwise defeat the whole point.

**Architecture:** No new packages — this is a type change rippling through `internal/gateway/controlplane` (parser + config types) → `internal/costaccounting`, `internal/budget`, `internal/identity` (leaf types) → `internal/gateway/dataplane` (wiring) → `cmd/gateway` (construction).

**Tech Stack:** `github.com/shopspring/decimal` — the gateway's second external Go dependency family (after OTel), zero transitive dependencies.

**Spec:** `docs/rfcs/2026-09-02-decimal-cost-accounting.md` — the exact type signatures and the parser-fix design live there; this plan implements them verbatim.

**Global Constraints** (inherited from the spec + `AGENTS.md`):
- Every money value must reach `decimal.Decimal` via a **string** conversion (`decimal.NewFromString`), never via an intermediate `float64` — that's the entire point of this pass. Any diff that calls `decimal.NewFromFloat` on a value that originated from YAML config is a regression, not a valid implementation.
- The `getFloat`/bool-parsing fix in `controlplane`'s YAML parser must not change behavior for any existing non-money field (`rate_limit.burst`/`refill_per_second`, `allowed_models` booleans) — prove this with tests, not assertion.
- `internal/telemetry` must not gain a dependency on `decimal.Decimal` — it takes a pre-formatted `string` for the cost attribute, per the RFC's explicit design choice to keep that package a dependency-free leaf.

---

## Task 1 — `github.com/shopspring/decimal` dependency + controlplane parser fix

**Files:**
- Modify: `gateway/go.mod`, `gateway/go.sum` (via `go get`)
- Modify: `gateway/internal/gateway/controlplane/config.go`
- Modify: `gateway/internal/gateway/controlplane/config_test.go`, `config_fuzz_test.go`

**Steps:**
- [ ] `go get github.com/shopspring/decimal` from `gateway/`; `go mod tidy`.
- [ ] In `parseYAMLScalar`: remove the `strconv.ParseFloat` branch entirely (numeric scalars stay as their raw source string). Replace the `strconv.ParseBool` call with an explicit check against exactly `"true"`/`"True"`/`"TRUE"`/`"false"`/`"False"`/`"FALSE"` — removing the `"0"`/`"1"`/`"t"`/`"f"` collision with numeric scalars (see RFC Motivation for the exact bug this fixes).
- [ ] Add `getDecimal(m map[string]any, key string) (decimal.Decimal, bool)`, mirroring `getFloat`'s shape: reads the raw string value and parses via `decimal.NewFromString`; returns `(decimal.Zero, false)` on a missing key or any type/parse mismatch.
- [ ] Change `ModelPriceConfig.{PromptPerToken,CompletionPerToken}` and `VirtualKeyConfig.BudgetUSD` to `decimal.Decimal`; update `Load()`'s parsing of `price_table`/`budget_usd` to call `getDecimal` instead of `getFloat`. `rate_limit.{burst,refill_per_second}` stay on `getFloat` — unchanged.
- [ ] Tests: existing `TestLoadExampleConfig` assertions for `team-alpha.BudgetUSD`/`gpt-4o` price fields updated to compare against `decimal.Decimal` values (e.g. `decimal.RequireFromString("100.0")`). **New regression test**: a config with `budget_usd: 1` (unquoted, bare digit) parses to exactly `decimal.RequireFromString("1")`, NOT the bool-collision zero value this RFC's Motivation documents as a real pre-existing bug — this is the load-bearing test for this task. **New test**: `rate_limit.burst`/`allowed_models` booleans behave identically to before (no regression from the parser change) — reuse/extend existing assertions, don't just add a new isolated test.

**Verify:** `cd gateway && go build ./internal/gateway/controlplane/... && go test ./internal/gateway/controlplane/... -fuzz=FuzzLoad -fuzztime=15s`

## Task 2 — `internal/costaccounting`: `Decimal` prices and calculation

**Files:**
- Modify: `gateway/internal/costaccounting/costaccounting.go`
- Modify: `gateway/internal/costaccounting/costaccounting_test.go`

**Steps:**
- [ ] `ModelPrice{PromptPerToken, CompletionPerToken decimal.Decimal}`; `Calculate(model string, usage Usage) decimal.Decimal` — `decimal.NewFromInt(int64(usage.PromptTokens)).Mul(price.PromptPerToken).Add(decimal.NewFromInt(int64(usage.CompletionTokens)).Mul(price.CompletionPerToken))`. Unknown model still returns `decimal.Zero` (unchanged behavior, new type).
- [ ] Tests: update existing known-model/zero-usage/unknown-model tests for the new return type. **New regression test**: reproduce the RFC's verified float64 drift scenario — compute the same cost via repeated `Calculate` calls summed manually vs. via `decimal.Decimal` arithmetic, and assert the decimal path is exact where a `float64` equivalent (constructed alongside, in the test, purely to prove the contrast) is not — this is the test that proves this whole RFC's reason for existing, not just that the code compiles with a different type.

**Verify:** `cd gateway && go test ./internal/costaccounting/...`

## Task 3 — `internal/budget`: `Decimal` spend tracking

**Files:**
- Modify: `gateway/internal/budget/budget.go`
- Modify: `gateway/internal/budget/budget_test.go`

**Steps:**
- [ ] `Tracker.spent map[string]decimal.Decimal`; `Allow(keyID string, capUSD decimal.Decimal) bool` (`capUSD.Sign() <= 0` → unlimited; else `spent[keyID].LessThan(capUSD)`); `Record(keyID string, costUSD decimal.Decimal)` (`costUSD.Sign() < 0` → ignored, unchanged semantics; else `spent[keyID] = spent[keyID].Add(costUSD)`).
- [ ] Explicitly verify (don't assume) `decimal.Decimal{}`'s zero value behaves as zero for an unseen map key — add a dedicated test asserting `Allow` on a never-recorded key with a positive cap returns `true` without any explicit initialization, exactly mirroring the old `float64` map's implicit-zero behavior.
- [ ] Update the existing boundary/independence/concurrency tests (exact-at-cap rejected, just-under-cap allowed, negative-cost-ignored, cross-key independence, the 100×100 concurrent `Record` race test) for the new type — the concurrency test in particular must still prove no lost updates under `-race`, now summing `decimal.Decimal` values instead of `float64`.

**Verify:** `cd gateway && go test ./internal/budget/... -race`

## Task 4 — `internal/identity`: `Decimal` budget field

**Files:**
- Modify: `gateway/internal/identity/identity.go`
- Modify: `gateway/internal/identity/identity_test.go`

**Steps:**
- [ ] `VirtualKey.BudgetUSD decimal.Decimal`.
- [ ] Update `TestVerifyPreservesConfiguredFields`'s `BudgetUSD` assertion for the new type; no other identity behavior changes (hash matching, duplicate detection, etc. are untouched by this field's type).

**Verify:** `cd gateway && go test ./internal/identity/...`

## Task 5 — Dataplane wiring + telemetry's string boundary (depends on Tasks 2-4)

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go`
- Modify: `gateway/internal/telemetry/result.go`
- Modify: `gateway/internal/gateway/dataplane/dataplane_test.go`, `telemetry_wiring_test.go`
- Modify: `gateway/internal/telemetry/result_test.go`

**Steps:**
- [ ] `dataplane.go`'s `finalize`/`logRequest`: `cost` becomes `decimal.Decimal` throughout; `logRequest`'s `cost_usd` structured-log field now logs `cost.String()` (a JSON string, not a bare number — per the RFC's explicit, documented format change).
- [ ] `telemetry.ChatCompletionResult.CostUSD` changes from `float64` to `string` — `internal/telemetry` gains no new dependency; the caller in `dataplane.go` passes `cost.String()`. `RecordChatCompletionResult` sets `AttrKelvranCostUSD` via `attribute.String` instead of `attribute.Float64`.
- [ ] Update every test that constructs a `budget.Tracker`/`identity.VirtualKey` with a literal `BudgetUSD`, or asserts on `kelvran.cost.usd`'s attribute value (`v.AsFloat64()` → `v.AsString()`, with the expected value now a decimal string like `"0.0000575"`, not a `float64` literal).

**Verify:** `cd gateway && go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./...`

## Task 6 — `cmd/gateway` wiring + integration tests (depends on Task 5)

**Files:**
- Modify: `gateway/cmd/gateway/main.go` (mostly automatic type flow-through in `buildPipeline` — verify, don't assume, that no explicit conversion is missing)
- Modify: `gateway/cmd/gateway/integration_test.go`

**Steps:**
- [ ] Confirm `buildPipeline` needs no logic changes beyond the types already flowing through it correctly (`cfg.PriceTable`/`cfg.VirtualKeys` are now `decimal.Decimal`-typed at the source).
- [ ] Update `TestIntegrationBudgetExceededReturns429DistinctFromRateLimit`'s hardcoded cost-threshold values/comments to the new decimal reality — re-verify the exact cost arithmetic it depends on (`7*0.0000025 + 4*0.00001`) is still correct with the fixed parser (it should be identical in *value*, only the underlying type changed) — do not just change the type annotation and assume the number still works, actually re-run it.
- [ ] Update `newIntegrationServerMultiKey`'s `BudgetUSD`/`PriceTable` construction to use `decimal.Decimal` literals.

**Verify:** `cd gateway && go build ./... && go vet ./... && go test ./... -v -race && golangci-lint run ./...`

## Task 7 — Docs, Changelog, Wrap-Up

**Files:**
- Modify: `gateway/internal/costaccounting/costaccounting.go`'s package doc comment (remove the now-fulfilled "Phase 1 upgrade" note)
- Modify: `gateway/ARCHITECTURE.md` (mark cost accounting's "Decimal-precision" note as real; note the new dependency)
- Modify: `gateway/changelog/unreleased.md` (Added/Fixed entries — this is also a **Fixed** entry, since the boolean-collision budget bug is a real correctness fix, not just a feature)
- Modify: `DECISIONS.md` (one line: library choice + the two real bugs found)
- Modify: `docs/agents/LOGS.md` (new append-only entry)
- Modify: `STATUS.md` (Current Phase, Verification State, Next Action)

**Verify:** re-run Task 6's full verify command once more after doc edits; cross-reference grep for every new doc's referenced paths.
