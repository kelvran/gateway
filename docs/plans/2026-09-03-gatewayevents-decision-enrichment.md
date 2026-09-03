> **For agentic executors:** Task 1 (proto) must land and be regenerated before Task 2 (budget getter) and Task 3 (Go threading) can compile against the new message fields. Task 2 and Task 3 are independent of each other. Task 4 is tests + wrap-up (docs/changelog), last.

---

**Goal:** `api/gatewayevents/v1`'s `GatewayDecisionEvent` gains three real, populated fields the original contract RFC named and deferred: a rate-limit fail-open flag, fallback-routing detail, and budget-spend-at-decision-time — closing the exact gap that RFC's own "Alternatives Considered" flagged (these need real code restructuring, not just a new emission call).

**Architecture:** Purely additive proto change (fields 7–11 on an already-shipped message, `buf breaking`-safe under `FILE`). `checkRateLimit` widens its return in place (`(bool, bool)`, mirroring the existing `nextDeployment`-style convention). A new unexported `fallbackInfo` struct threads fallback detail through `streamDeploymentWithFallback`'s return and the inline fallback block in `HandleChatCompletion`. A new `budget.Tracker.SpentUSD` getter, called at the `budget.Allow` call site (not inside `finalize`) captures spend-at-decision-time. `finalize`'s signature widens from 8 to 11 parameters to receive all three.

**Spec:** `docs/rfcs/2026-09-03-gatewayevents-decision-enrichment.md` — the exact field definitions, the Go-level design and why it was chosen over the general-case alternatives, and the exact budget-spend-at-decision-time semantics all live there; this plan implements them verbatim.

**Global Constraints** (inherited from the spec):
- No `optional` keyword on any new proto field — see the spec's specific, checked reason (this repo's `protojson.Marshal` call has no `EmitUnpopulated`, so plain-`bool` absence and explicit-false already collapse identically).
- Fallback detail stays a flat `bool` + 2 `string`s — never a repeated/nested message. Kelvran's fallback logic structurally attempts at most one fallback; do not build a loop or a richer shape "for future-proofing."
- `budgetSpentAtDecision` must be captured at the `budget.Allow` call site, immediately before calling `Allow`, unconditionally (including on the rejection path) — never inside `finalize`. Getting this ordering wrong is the single most likely implementation mistake per the spec's own "what a naive implementation would get wrong" section.
- `fallbackInfo.happened` must only be set to `true` inside the branch where a fallback is actually attempted (`hasFallback && fallbackDep.Name != dep.Name`), never merely wherever `err != nil`.
- No changes to `UPGRADE.md` — this is confirmed non-breaking (see spec).

---

## Task 1 — Proto fields + regeneration

**Files:**
- Modify: `api/gatewayevents/v1/gatewayevents.proto`

**Steps:**
- [ ] Append fields 7–11 (`rate_limit_fail_open`, `fallback_happened`, `fallback_from_deployment`, `fallback_reason`, `budget_spent_usd`) after the existing `Outcome outcome = 6;` line, with the exact doc comments from the spec's Detailed Design section (each comment explains the field's meaning AND its interaction with `Outcome` — don't abbreviate these away).
- [ ] `make gen-proto` (regenerates both `gateway/api/gatewayevents/v1/gatewayevents.pb.go` and `evals/evals/contracts/gatewayevents/v1/gatewayevents_pb2.py`) — requires `protoc-gen-go`/`protoc` on `PATH` (this session's known local quirk: `export PATH="$(go env GOPATH)/bin:$PATH"` first if `make gen-proto` reports `protoc-gen-go: executable file not found`).
- [ ] `make check-proto` after regeneration but before committing anything else — confirms the regenerated bindings are what's about to be committed (this target diffs against the git-committed state, so run it, inspect the diff is exactly the expected new-fields diff, then let the commit itself make it clean).
- [ ] `cd api && buf breaking --against '.git#branch=main'` — confirm 0 violations (this is the empirical proof the spec's "additive-only, FILE-category-safe" claim holds, not just the theoretical argument).

**Verify:** `make gen-proto && make check-proto` (expect a diff before commit, clean after); `cd api && buf lint && buf breaking --against '.git#branch=main'` — both clean.

## Task 2 — `budget.Tracker.SpentUSD`

**Files:**
- Modify: `gateway/internal/budget/budget.go`
- Modify: `gateway/internal/budget/budget_test.go`

**Steps:**
- [ ] Add `func (t *Tracker) SpentUSD(keyID string) decimal.Decimal` — lock/read `t.spent[keyID]`, exactly mirroring `Allow`'s existing locking idiom (`t.mu.Lock(); defer t.mu.Unlock()`). No store interaction (pure in-memory read, like `Allow`).
- [ ] Test: a never-recorded key returns the zero-value `decimal.Decimal{}` (renders as `"0"` via `.String()`), not a panic or a map-nil-dereference issue (Go's zero-value-map-read semantics already make this safe, but assert it explicitly — this is exactly the kind of "verify, don't assume" case this project's own testing discipline calls for).
- [ ] Test: after one `Record(keyID, cost)` call, `SpentUSD(keyID)` returns exactly `cost` (not `cost` plus/minus any rounding — decimal equality, not float tolerance).
- [ ] Test: `SpentUSD` never mutates `t.spent` (calling it twice in a row returns the same value both times) — a read-only guarantee worth pinning explicitly since it's adjacent to `Record`'s mutation path in the same locked section style.

**Verify:** `cd gateway && go build ./internal/budget/... && go test ./internal/budget/... -race`

## Task 3 — Go threading: `checkRateLimit`, fallback detail, `finalize` wiring

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go`
- Modify: `gateway/internal/gateway/dataplane/streaming.go`
- Modify: `gateway/internal/gateway/dataplane/dataplane_test.go`
- Modify: `gateway/internal/gateway/dataplane/streaming_test.go`
- Modify: `gateway/internal/gateway/dataplane/gatewayevents_test.go`

**Steps:**
- [ ] Widen `checkRateLimit(ctx, vk) bool` to `(ok bool, failedOpen bool)`, exactly per the spec's code block — update both call sites (`dataplane.go:438`, `streaming.go:73`) to capture the second return value into a `rateLimitFailedOpen` local declared in the existing var block (assign with `=`, matching how `dep`/`cacheHit` are already assigned so the deferred `finalize` closure sees the final value on every return path).
- [ ] Add the unexported `fallbackInfo` struct (in `dataplane.go`, alongside other Pipeline-internal types) — exactly the 3 fields from the spec.
- [ ] `HandleChatCompletion`'s inline fallback block: capture `fallback = fallbackInfo{happened: true, from: dep.Name, reason: err.Error()}` BEFORE the `dep = fallbackDep` reassignment, and only inside the `hasFallback && fallbackDep.Name != dep.Name` branch (not the outer `if err != nil`).
- [ ] `streamDeploymentWithFallback`: widen its return to `(adapter.ChatResponse, Deployment, fallbackInfo, error)`, capturing the same way, before the `dep = fallbackDep` reassignment inside `streaming.go`. Update its one call site in `HandleChatCompletionStream` to capture the new return value into the same `fallback` local.
- [ ] Add `rateLimitFailedOpen bool`, `fallback fallbackInfo`, `budgetSpentAtDecision decimal.Decimal` to both `HandleChatCompletion`'s and `HandleChatCompletionStream`'s existing var blocks.
- [ ] At both `budget.Allow` call sites (`dataplane.go:443`, `streaming.go:77`), insert `budgetSpentAtDecision = p.budget.SpentUSD(vk.ID)` immediately BEFORE the `if !p.budget.Allow(...)` line — unconditional, not inside the `if`.
- [ ] Widen `finalize`'s signature to accept `rateLimitFailedOpen bool, fallback fallbackInfo, budgetSpentAtDecision decimal.Decimal` (in addition to its existing 8 params) — update both deferred call sites (`dataplane.go:424`, `streaming.go:61`) to pass the 3 new locals.
- [ ] Inside `finalize`, populate the 5 new `GatewayDecisionEvent` fields exactly per the spec's code block (`RateLimitFailOpen`, `FallbackHappened`, `FallbackFromDeployment`, `FallbackReason`, `BudgetSpentUsd: budgetSpentAtDecision.String()`).
- [ ] Test (`dataplane_test.go`, mirroring existing `TestOutcomeForClassifiesEverySentinelError`-style full-pipeline assertions): a rate-limit backend error (Redis-backed limiter configured, backend unreachable) produces a logged `gatewayevents_v1` field with `rate_limit_fail_open: true`; a normal in-memory rate-limit pass produces `false`.
- [ ] Test: a request whose first deployment fails and falls back to a second succeeding one produces `fallback_happened: true`, `fallback_from_deployment` equal to the first deployment's name, and a non-empty `fallback_reason`; a request with no eligible fallback (single deployment configured) that errors produces `fallback_happened: false`.
- [ ] Test (streaming-specific, per the spec's explicitly-flagged failure mode): a streaming request that sends its first chunk successfully and THEN errors must report `fallback_happened: false` even though the overall request errors — never conflate "this response errored" with "this response fell back."
- [ ] Test: a `BUDGET_EXCEEDED` rejection populates `budget_spent_usd` with the key's real spend at rejection time (not `"0"` unless the key genuinely has zero prior spend) — this is the load-bearing proof that the field survives the rejection path, not just the success path.
- [ ] Update every existing test that calls `finalize` directly or constructs a `Config`/`Pipeline` and asserts on `gatewayevents_v1` JSON fields (`gatewayevents_test.go`) to account for the 3 new parameters/fields — existing assertions on the 6 original fields must still pass unchanged (this is a purely additive change to those tests' expectations, not a rewrite).

**Verify:** `cd gateway && go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./...` — all packages `ok`, `0 issues`, race-clean.

## Task 4 — Docs, changelog, wrap-up

**Files:**
- Modify: `gateway/ARCHITECTURE.md` (if the Cache/telemetry-adjacent description of `GatewayDecisionEvent`'s field set needs updating)
- Modify: `gateway/changelog/unreleased.md`
- Modify: `DECISIONS.md`
- Modify: `docs/agents/LOGS.md`
- Modify: `STATUS.md`

**Steps:**
- [ ] Add a `## Added` entry to `gateway/changelog/unreleased.md` describing the 3 new fields, mirroring the style/detail level of the original `api/gatewayevents/v1` entry already in `gateway/changelog/0.1.0.md`.
- [ ] Append one `DECISIONS.md` line at the true chronological end (re-check `tail` immediately before appending — this project's own established gotcha).
- [ ] Append one `docs/agents/LOGS.md` entry (Files touched / Intent-summary / Decisions made / Verification performed / Bugs found / Next steps), matching every prior entry's format.
- [ ] Update `STATUS.md`'s Current Phase / Last Completed Task / Next Action / Verification State sections.
- [ ] Full `make verify` from repo root (build + vet + lint + test + check-proto, both deployables) — must pass clean before commit.

**Verify:** `make verify` (root) passes end-to-end; `git diff` reviewed in full before committing.
