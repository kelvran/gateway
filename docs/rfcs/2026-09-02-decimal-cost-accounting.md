- **Status**: accepted
- **Date**: 2026-09-02
- **Author(s)**: project founder + Claude Code

## Summary

Replace every dollar-denominated `float64` in the gateway — `costaccounting.ModelPrice`, `Calculator.Calculate`'s return value, `budget.Tracker`'s cumulative spend, `identity.VirtualKey.BudgetUSD`, and their config counterparts — with `github.com/shopspring/decimal`'s `decimal.Decimal`. This fulfills `PRD.md`'s "Decimal-precision requirement," documented as a "Phase 1 upgrade" in `internal/costaccounting`'s own package comment since the initial scaffolding pass, and closes a real, empirically-verified precision bug in the process (see Motivation).

## Motivation

`internal/costaccounting/costaccounting.go`'s own doc comment has said, since the initial scaffolding pass, "float64 arithmetic is used for this pass. PRD.md's Decimal-precision requirement is a documented Phase 1 upgrade, not silently dropped." A repo-wide search confirms every other mention of "Decimal-precision" (`gateway/ARCHITECTURE.md`, `DECISIONS.md`, prior RFCs/plans) is the same placeholder phrase — no library or approach was ever named. This RFC is that upgrade, and it makes the specific choice nothing before it made.

**This is not a theoretical concern.** A verified repro (`go run`, not assumed from memory) using Kelvran's own real per-token price fragments shows measurable drift from accumulated `float64` addition — exactly the operation `internal/budget.Tracker.Record` performs on every single request:

```
accumulated sum of 7.5e-06 x10000 = 0.07499999999999333589 (exact would be 0.075)
```

That's a ~6.6e-15 absolute error after only 10,000 additions of a realistic per-request cost fragment — small in isolation, but it compounds with every request a long-lived virtual key ever makes, and it sits directly underneath the exact number `budget.Tracker.Allow` compares against a hard cap. A budget enforcement system whose own arithmetic drifts is not a rounding nitpick; it undermines the specific guarantee `PRD.md`'s Problem Statement leads with (agent-run cost accountability).

**A second, more severe bug was found during this RFC's own research**, in the hand-rolled YAML parser (`controlplane.parseYAMLScalar`): it disambiguates booleans from other scalars via `strconv.ParseBool`, which accepts `"0"` and `"1"` as valid boolean literals (not just `"true"`/`"false"`). An operator writing `budget_usd: 1` (intending a $1.00 cap) would have that value silently parsed as the boolean `true`, fail `getFloat`'s type switch, and fall back to Go's zero value — `BudgetUSD: 0`, which `budget.Tracker.Allow`'s own documented convention treats as **unlimited**. A one-digit cap value would silently disable the cap entirely. This RFC fixes it as part of the same pass, since it's the same function this change already has to touch.

## Detailed Design

### Library choice: `github.com/shopspring/decimal`

Verified before choosing: no prior text anywhere in this repo commits to a specific library (unlike OTel, which had a named Tech Stack row before code existed) — this is a fully open decision, not a pre-approved one, and this RFC argues it on its own merits (see Alternatives Considered). `shopspring/decimal` is the de facto standard for Go decimal/financial math, has **zero required runtime dependencies** (keeping this module's dependency footprint tight — it becomes the second external dependency family after OTel, not the start of a trend), and its operator-style API (`Add`/`Mul`/`Sub`, returning new immutable values) matches this codebase's existing "no mutation" convention exactly.

### The real fix has two parts, not one

Simply swapping `float64` for `decimal.Decimal` in Go struct fields is not sufficient — money values currently *enter* the system through `controlplane`'s hand-rolled YAML parser, which already converts every unquoted numeric scalar to `float64` **before** any caller ever sees it (`parseYAMLScalar`'s `strconv.ParseFloat` branch). Reading that already-lossy `float64` back out and wrapping it in `decimal.NewFromFloat` would inherit the exact binary-to-decimal conversion artifact this RFC exists to eliminate — decimal-typing the Go fields while still round-tripping every config value through `float64` first would fix nothing.

The real fix: `parseYAMLScalar` stops eagerly converting numeric-looking scalars to `float64` at all. Every non-boolean, non-quoted-string scalar is kept as its **raw source string** in the parse tree; `getFloat` (unchanged call sites: rate-limit burst/refill — not money, `float64` precision is fine there) and a new `getDecimal` (money fields) each convert from that raw string at the point of use, with their own precision semantics. `getFloat`'s existing type switch already has a `case string: strconv.ParseFloat(...)` branch (it already handled quoted numeric values this way) — removing the eager conversion means *every* numeric scalar now takes that same branch, unquoted or not, with no behavior change for non-money fields and a real precision fix for money ones. No YAML syntax change is required — `config.example.yaml`'s existing unquoted `0.0000025`-style values now parse to exact `decimal.Decimal`s instead of first being rounded to the nearest `float64`.

The boolean-disambiguation bug is fixed in the same function: replace the permissive `strconv.ParseBool` call with an explicit check against `"true"`/`"True"`/`"TRUE"`/`"false"`/`"False"`/`"FALSE"` only — removing the `"0"`/`"1"`/`"t"`/`"f"` collision with numeric scalars. This is *more* consistent with the parser's own documented minimalism ("supports exactly the subset this config's shape needs"), not less.

### Type changes (mechanical, once the above is fixed)

```go
// internal/costaccounting
type ModelPrice struct { PromptPerToken, CompletionPerToken decimal.Decimal }
func (c *Calculator) Calculate(model string, usage Usage) decimal.Decimal // was float64; unknown model -> decimal.Zero

// internal/budget
type Tracker struct { spent map[string]decimal.Decimal }
func (t *Tracker) Allow(keyID string, capUSD decimal.Decimal) bool
func (t *Tracker) Record(keyID string, costUSD decimal.Decimal)

// internal/identity
type VirtualKey struct { /* ... */ BudgetUSD decimal.Decimal }

// internal/gateway/controlplane
type ModelPriceConfig struct { PromptPerToken, CompletionPerToken decimal.Decimal }
type VirtualKeyConfig struct { /* ... */ BudgetUSD decimal.Decimal }
```

### Where `decimal.Decimal` stops: OTel attributes and JSON logs

OTel's attribute value model has no decimal type (`string`/`int64`/`float64`/`bool`/slices thereof). Converting `kelvran.cost.usd` back to `float64` for the span attribute would reintroduce the exact precision loss this RFC removes, one hop before the data leaves the process. Instead, `telemetry.ChatCompletionResult.CostUSD` becomes a plain `string` (the caller formats via `cost.String()`, which is exact) — this also keeps `internal/telemetry` from taking on a dependency on the money-type choice at all, matching its existing "dependency-free leaf" design from the OTel RFC. The structured JSON log line's `cost_usd` field changes from a bare JSON number to a JSON string for the same reason (`decimal.Decimal` implements `MarshalJSON`/`MarshalText` as a quoted string) — a deliberate, documented format change, not an accidental one.

## Drawbacks

- Second external Go dependency family. Mitigated: zero transitive dependencies, extremely widely used, small and stable API surface.
- `cost_usd`'s JSON log/attribute shape changes from a number to a string. Any external log-parsing tooling that assumed a bare number breaks. Accepted: there is no external consumer yet (no release has shipped), and a string is the only representation that doesn't silently reintroduce float rounding at the boundary.
- `decimal.Decimal`'s zero value is a valid, usable zero (confirmed, not assumed) — but every call site that previously relied on Go's implicit `float64` zero-value default (e.g. `budget.Tracker`'s map returning `0` for an unseen key) must be re-verified to behave identically with `decimal.Decimal`'s zero value. Verified as part of Task 3 below, not assumed.

## Alternatives Considered

1. **`math/big.Rat`** (stdlib, zero dependency) — rejected: exact rational arithmetic is a different mental model than fixed-point decimal currency (no canonical decimal-string rendering or rounding-mode API), and JSON/log serialization would need to be hand-rolled from scratch.
2. **`github.com/cockroachdb/apd`** — rejected for now: built for SQL `DECIMAL`-context parity (explicit rounding `Context` objects, error-returning ops), which matters when a Go program must exactly replicate a real database's rounding rules. Kelvran has no Postgres-backed control-plane store yet (still deferred) — there is no database rounding contract to match today, so `apd`'s extra ceremony buys nothing yet. Revisit if/when a real persistent store lands.
3. **Plain `int64` fixed-point (e.g. micro-dollars)** — rejected: Kelvran's real per-token prices (e.g. `0.0000025`/token) need sub-micro-dollar resolution in places; a fixed integer scale factor either loses exactly the precision this RFC exists to preserve, or requires hand-rolled variable-scale bookkeeping at every multiplication — reinventing, less safely, what a decimal library already does correctly.
4. **Fix only the Go struct types, leave the YAML parser's eager `float64` conversion alone** — rejected: this would ship a "Decimal-precision" feature that silently round-trips every config-sourced price/budget through `float64` first, fixing nothing for the actual entry point of every dollar value in the system.

## Unresolved Questions

- Whether `decimal.Decimal` should also propagate into `evals/`'s cost-per-run metrics — `evals/` has zero `Decimal` usage today (verified, not assumed) and no cost-accounting code of its own yet; this is a natural follow-up once `evals/` actually ingests real cost data (blocked on the still-deferred `api/gatewayevents` contract), not decided here.
- Whether a rounding mode/precision cap should be enforced when serializing `decimal.Decimal` to JSON (e.g. bounding to 6 decimal places) — left unbounded for this pass (full precision, whatever `decimal.NewFromString` parsed); revisit if a real backend ever needs a fixed-width numeric column.
