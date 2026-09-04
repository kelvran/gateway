# AGENTS_LEARNING.md — Kelvran Mistake & Pattern Taxonomy

This document captures recurring mistakes, their root causes, and the prevention rules that came out of them — a mistake→fix→rule loop, distinct from `docs/agents/LOGS.md` (raw chronological session history), `docs/agents/MEMORY.md` (curated durable facts), and `DECISIONS.md`/`docs/decisions/` (architectural decisions and their rationale). If something belongs in one of those three, it belongs there and not here — this file is specifically for behavior corrections that recur or are likely to recur.

Populated 2026-09-04 by reviewing this project's real history in `docs/agents/LOGS.md`/`DECISIONS.md` end to end — every Evolution Log entry below is sourced from a real, dated incident already logged there, none fabricated. One pattern (doc-vs-code staleness) has now recurred 6+ times and is promoted into §1/§2/§3/§5 below, per §8's own promotion rule. Everything else logged has occurred once or twice — real, worth recording, but not yet promoted past the Evolution Log, per this file's own "a single instance is not yet a pattern" discipline.

**Timestamp note:** `docs/agents/LOGS.md` records dates only, not time-of-day. Entries below use `T00:00:00Z` as an explicit placeholder, not a claim of real precision — per this project's own "dated honesty over confident vagueness" principle (`docs/agents/ETHOS.md`).

---

## 1. Mistakes Observed

| Date | What happened | Ref |
|---|---|---|
| 2026-09-02 → 2026-09-04 (6 instances) | A doc (`THREAT_MODEL.md`, `SECURITY.md`, `gateway/ARCHITECTURE.md`, `evals/ARCHITECTURE.md`, or a root-level status file) asserted a capability/mitigation/status in the present tense after the code it described had been deferred, narrowed, or simply never matched — found and corrected 6 separate times. See Evolution Log entries tagged `[doc-staleness]` below. | `DECISIONS.md`, `docs/agents/LOGS.md` entries for 2026-09-03 (Guardrails), 2026-09-04 (evals STRIDE table + router doc + `.importlinter` claim, `THREAT_MODEL.md` Gateway rows, repo-wide "pre-scaffolding" sweep) |

## 2. Root Cause Analysis

**Doc-vs-code staleness (6 instances, see §1):** every instance shares the same root cause — a document was written *ahead of* the code it described (either aspirationally, at design time, or narrating an intended architecture), and no mechanism re-checks that prose against the live repo when the code is later deferred, narrowed in scope, or simply not built on the original timeline. Nothing enforces this the way `go vet`/`ruff`/`buf breaking` enforce code correctness — a stale doc claim is a silent, compiler-invisible failure mode. Contributing factor: several instances were themselves papering over an *earlier* correction (e.g. `gateway/ARCHITECTURE.md`'s router entry was corrected once on 2026-09-04, but `USER_GUIDE.md`'s separate description of the same feature was missed in that same pass and had to be caught again later that day) — fixing one document describing a gap doesn't guarantee every other document describing the same gap got fixed too.

## 3. Mitigation & Fix Strategy

- Before trusting any document's present-tense capability claim, `grep`/`ls`/read the actual code path it describes — every instance in §1 was caught this way, never by assumption.
- When a feature ships, narrows, or gets deferred, grep the whole repo for its name/keyword across `*.md` (not just the one architecture doc most obviously about it) — `gateway/internal/router`'s doc fix and `USER_GUIDE.md`'s separate, later fix for the exact same feature are the clearest case of this rule being needed and not yet followed the first time.
- Before committing to a large next feature, run a lightweight sanity-check pass (fork or direct grep) specifically looking for doc-vs-code gaps or better-ready candidates — this pattern itself is what surfaced 3 of the 6 instances (the `THREAT_MODEL.md` fix before the router RFC, and the repo-wide sweep before either the circuit-breaker RFC or PyPI publish).
- See `AGENTS.md`'s Gotchas section, now cross-referenced from here per §8's promotion rule.

## 4. Best Practices (Must Follow)

- **Independently verify subagent/workflow/research output against the live repo or a primary source before it ships into a durable artifact (a merged commit, an RFC, a doc).** Validated repeatedly, not just once: a dynamic-workflow synthesis pass caught a grounding fork's wrong claim about GCRA's `Result.Remaining` field before it shipped into an RFC (2026-09-03); a `/deep-research` workflow's own completion summary claimed a file-write deliverable that the script never actually performed, caught only by checking `git status`/`ls` directly rather than trusting the notification text (2026-09-03); this session's own router-RFC synthesis agent was explicitly instructed to "independently RE-VERIFY the load-bearing claims" rather than trust any one research angle, and did. Do not treat a subagent's or workflow's summary text as ground truth — treat it as a claim to check.
- **Never round-trip a money value through `float64`, even transiently inside a parser.** `controlplane.parseYAMLScalar` originally eagerly converted numeric-looking YAML scalars to `float64` before any caller saw them, which is what let `strconv.ParseBool`'s permissive grammar (`"0"`/`"1"` as valid booleans) collide with a bare `budget_usd: 1` and silently produce an *unlimited* budget instead of a $1.00 cap (2026-09-03). Fixed by keeping numeric scalars as their raw source string all the way to `getDecimal`/`decimal.NewFromString`.

## 5. Anti-Patterns (Must Avoid)

**Leaving a present-tense doc claim unchecked against the code it describes, especially right after that code is deferred, narrowed, or reprioritized.** Recurred 6 times (§1); promoted here per §8's rule. Each instance was harmless in isolation (no security/correctness bug shipped from any of them — they were all *documentation* overclaims, not code) but collectively represent the single most-repeated category of "mistake" this project has produced so far, ahead of any single code bug. **This is Kelvran's only anti-pattern list** — do not create a separate `ANTI-PATTERNS.md`; an entry here that becomes enforced by CI/lint should be removed from this list per `AGENTS.md`'s own falsifiability rule, the same discipline that already governs `AGENTS.md`'s Boundaries section. (No CI/lint mechanism catches doc-vs-code drift today — `go-arch-lint`/`.importlinter` were themselves two of the six stale claims found, so this anti-pattern is not yet falsifiable-away; it stays here.)

## 6. Architectural Patterns to Follow

- **Decouple a new leaf package's own domain type from the type of whatever package first needed it, rather than importing that package's type directly.** Used more than once, not a single design choice: `ratelimit.KeyConfig` carries a plain key-ID string rather than importing `identity.VirtualKey`; `budget.Tracker` does the same; and `router.Deployment` (2026-09-04) is a decoupled type distinct from `dataplane.Deployment`, mirroring the same reasoning explicitly in its own doc comment. Keeps every shared-kernel leaf package independently testable and reusable, and keeps the dependency-direction table (`gateway/ARCHITECTURE.md`) enforceable by inspection.

Pointer-heavy otherwise: see `DESIGN.md` for the whole-system rationale and `docs/decisions/` for the three foundational ADRs.

## 7. Failure Scenarios (Failure-First Thinking)

Evidence-based — failure modes actually caught during development, not `THREAT_MODEL.md`'s pre-code STRIDE anticipation:

- **A single-character config value silently disabling a spending control.** `budget_usd: 1` parsed as the boolean `true` (not the numeric `1`), fell through every numeric type switch, and landed on `budget.Tracker`'s zero-value-means-unlimited convention — the exact combination of "looks like a normal config line" and "fails silently to the least-safe default" that makes this worth recording here, not just as a one-line bug fix. Fixed 2026-09-03 (see §4).
- **A content-policy rejection reported as an upstream failure.** `ErrGuardrailBlocked`, left unmapped in `writeErrorResponse`, would have defaulted to HTTP `502` (upstream error) instead of `400` (bad request) — indistinguishable from a real provider outage to anything monitoring status codes, caught only because the integration test asserted the *specific* code rather than a loose `>= 400` check. Fixed 2026-09-03.

## 8. Continuous Learning Rules

- A `docs/agents/LOGS.md` entry that describes a mistake gets promoted into this file's Evolution Log below — the log stays raw history, this file extracts the lesson.
- An Evolution Log entry whose Category is "Anti-Pattern" and recurs 3+ times gets promoted into §5 above as a standing rule, and cross-referenced from `AGENTS.md`'s own Gotchas section. (First applied 2026-09-04: doc-vs-code staleness, 6 instances — see `AGENTS.md`'s Gotchas section for the cross-reference.)
- An Evolution Log entry that turns out to be architecturally significant (changes a foundational, hard-to-reverse decision) gets promoted into a full ADR under `docs/decisions/`, not left here.

---

## Evolution Log

Append-only. Newest entry at the bottom. Never edit a past entry — if something in it turns out to be wrong, add a new entry that supersedes it and says so.

**Entry format:**
```
### [Learning Entry - YYYY-MM-DDTHH:MM:SSZ]

**Context:** <what task was being performed>
**Mistake:** <what went wrong>
**Root Cause:** <why it happened>
**Fix:** <what was done>
**Prevention Rule:** <rule to avoid this in future>
**Category:** Best Practice / Anti-Pattern / Bug Fix / Architecture
```

### [Learning Entry - 2026-09-02T00:00:00Z]

**Context:** Streaming (SSE) support — implementing the Anthropic `StreamDecoder`.
**Mistake:** `decodeMessageDelta` parsed `usage` from Anthropic's `message_delta` event but silently discarded `delta.stop_reason` — every reconstructed streamed response would carry an empty `FinishReason` regardless of how the model actually stopped.
**Root Cause:** The event carries two independent pieces of information (usage and stop reason) and only one was wired through; no existing test asserted on `FinishReason` for the streaming path, so nothing caught the gap at write time.
**Fix:** Added a `finishReasonFromStopReason` mapping and emitted a `FinishReason`-carrying chunk from `message_delta`; added `TestStreamDecoder_MessageDeltaCarriesFinishReason` across all 4 fixtures.
**Prevention Rule:** When a provider event carries multiple independent fields, write one test per field, not one test that only checks the field that happens to be top-of-mind.
**Category:** Bug Fix

### [Learning Entry - 2026-09-02T00:00:00Z]

**Context:** OTel tracing + `agent_run_id` — writing multi-test span assertions.
**Mistake:** Assumed each test could swap in its own isolated `TracerProvider`. `go.opentelemetry.io/otel`'s global `TracerProvider` delegate only meaningfully re-delegates a given already-obtained `Tracer` handle's *first* real provider — a second `otel.SetTracerProvider` call in a later test is silently accepted but has no effect, so tests 2+ silently recorded zero spans.
**Root Cause:** Assumed a common testing pattern (fresh provider per test) without first checking the specific SDK's re-delegation semantics for an already-vended `Tracer`.
**Fix:** Installed one shared recorder once via `TestMain`, with each test tracking spans by index delta rather than expecting an isolated recorder per test.
**Prevention Rule:** For any SDK-provided global/singleton with a "swap the backend" API, verify re-delegation behavior with an isolated from-scratch repro before relying on it across multiple tests in the same binary.
**Category:** Bug Fix

### [Learning Entry - 2026-09-02T00:00:00Z]

**Context:** OTel tracing + `agent_run_id` — the real end-to-end HTTP integration test.
**Mistake:** `TestIntegrationAgentRunIDPropagatesFromBaggageHeaderToSpan` failed on first run: `telemetry.ExtractContext` extracted nothing, because no test in `cmd/gateway` ever calls `run()` — the only caller of `telemetry.Init`, which sets the global `TextMapPropagator` — so the propagator stayed the SDK's no-op default.
**Root Cause:** Test binaries bypass the production startup path (`run()`) entirely; a side effect `Init` sets as a byproduct of normal startup has no equivalent in a test binary unless explicitly replicated.
**Fix:** Had that package's own `TestMain` set the composite propagator explicitly, mirroring exactly what `Init` does in production.
**Prevention Rule:** When a real end-to-end test needs a side effect that only `main()`'s startup path normally sets, replicate that exact side effect in `TestMain` rather than assuming the test binary inherits it.
**Category:** Bug Fix

### [Learning Entry - 2026-09-03T00:00:00Z]

**Context:** Decimal-precision cost accounting — grounding research for the RFC.
**Mistake:** `controlplane.parseYAMLScalar`'s boolean detection used `strconv.ParseBool`, which accepts `"0"`/`"1"` as valid booleans. `budget_usd: 1` — the single most natural way to write a $1.00 cap — silently parsed as the boolean `true`, failed every numeric field's type switch, and fell back to Go's zero value, which `budget.Tracker`'s own convention reads as **unlimited budget**.
**Root Cause:** `strconv.ParseBool`'s permissive grammar was used for a field-type-detection heuristic without checking whether its accepted literal set could collide with a different field's genuinely valid values.
**Fix:** Matched only the literal words `true`/`false` (with common casing), not `strconv.ParseBool`'s full grammar. Added `TestLoadBudgetUSDBareDigitIsNotMisreadAsBool` as a permanent regression test.
**Prevention Rule:** Never use a stdlib parsing function's *full* accepted grammar for a type-detection heuristic when a narrower, explicit match would do — permissiveness in the wrong place becomes a silent security/correctness bug. See §4/§7.
**Category:** Bug Fix

### [Learning Entry - 2026-09-03T00:00:00Z]

**Context:** Branch-strategy research via a `/deep-research` workflow invocation.
**Mistake:** The workflow's completion summary described file-write deliverables (updating `docs/development/BRANCHES.md` and saving a research trail) that the script never actually performed — the underlying research (search/fetch/verify/synthesize) genuinely happened, but no Write step existed in the script's phases for the two deliverables the prompt's `args` had requested in prose.
**Root Cause:** Asking for a file update in a workflow's `args` prompt text does not make the script perform one — the script's actual phases are what execute, not the prose describing what was wanted.
**Fix:** Verified with `git status`/`ls`/`git log` before trusting the notification, found the files unchanged, and performed the write directly instead.
**Prevention Rule:** A completed workflow's result-text describes what it *says* it did, not necessarily what it did — verify file-I/O deliverables the same way any other subagent's self-report gets checked in this project (see §4).
**Category:** Anti-Pattern

### [Learning Entry - 2026-09-03T00:00:00Z]

**Context:** Release readiness — first real CI run against the newly-public repo.
**Mistake:** `.github/workflows/ci.yml` pinned `golangci-lint-action@v6`, which does not support golangci-lint v2's version string — the `gateway` lint job failed outright on the very first real push.
**Root Cause:** The Action pin was chosen when the workflow file was authored and never re-validated against a real CI run before the repo went public (no real push had triggered it before this point).
**Fix:** Bumped to `golangci-lint-action@v7`.
**Prevention Rule:** A CI config that has never run against a real push is unverified, no matter how carefully-reasoned it looked when written — treat "first real push" as a real test of the CI config itself, not just of the code.
**Category:** Bug Fix

### [Learning Entry - 2026-09-03T00:00:00Z]

**Context:** Release readiness — checking release readiness holistically before the first tag.
**Mistake:** `evals/pyproject.toml`'s PyPI distribution name was `evals`, not `kelvran-evals` — the name `RELEASE.md`'s own, already-written Publish Targets table had committed to.
**Root Cause:** The distribution-name field and the release-planning doc were written in two different passes with no cross-check between them.
**Fix:** Corrected the distribution name only; the importable module (`evals`) and installed CLI command (`evals`) were left unchanged, matching the standard "distribution name differs from import name" PyPI pattern.
**Prevention Rule:** When a release doc commits to a specific external-facing name (a package name, an org, a URL), grep the actual config file that will produce that artifact before tagging, don't assume they already agree.
**Category:** Bug Fix

### [Learning Entry - 2026-09-03T00:00:00Z]

**Context:** Distributed (Redis-backed) rate limiting — grounding research for the RFC.
**Mistake:** A grounding fork's research report argued against using GCRA partly on the claim that GCRA implementations can't expose "remaining tokens." Direct verification against the real, installed `go-redis/redis_rate` library's `Result` struct showed this was wrong — `Result.Remaining` reports exactly that.
**Root Cause:** The fork's claim was plausible-sounding but not checked against the actual library before being written into the RFC's Alternatives Considered section.
**Fix:** Corrected the RFC before any code was written, replacing the wrong argument with the real, verified disqualifying reason (an `int`-vs-`float64` type mismatch with the rest of the token-bucket math).
**Prevention Rule:** A subagent's plausible-but-wrong claim must never ship into a durable spec document unchecked — verify against the primary source (the actual library/API) directly. See §4.
**Category:** Anti-Pattern

### [Learning Entry - 2026-09-03T00:00:00Z]

**Context:** Cache L2 (normalized-match) — initial implementation of `dataplane.normalizeMessages`.
**Mistake:** The RFC's own initial function-signature sketch (`cache.NormalizeMessages(messages []adapter.Message)`) would have violated `internal/cache/key.go`'s own explicit, pre-existing architectural rule that `cache` never imports `internal/adapter`.
**Root Cause:** The RFC's sketch was written against the feature's logical shape without re-checking the target package's own existing import-boundary rule first.
**Fix:** Moved the actual normalization logic into `dataplane` (which already depends on `adapter`), keeping only the primitive-input `NormalizedKey` inside `cache`.
**Prevention Rule:** Before writing a new function signature into an existing package, re-read that package's own doc comment for import-boundary rules — an RFC's logical sketch is not authoritative over a package's already-settled dependency direction. See §6.
**Category:** Architecture

### [Learning Entry - 2026-09-03T00:00:00Z]

**Context:** Cache L2 (normalized-match) — `cache.Key`/`cache.NormalizedKey` initial implementation.
**Mistake:** `Key` and `NormalizedKey`, as first written, had byte-identical hash construction given identical remaining inputs — isolation between L1 and L2 relied entirely on using separate `cache.Cache` instances, with no structural insurance against a future refactor ever sharing one map.
**Root Cause:** The two functions' hash inputs were derived independently without a deliberate namespace/layer tag distinguishing them.
**Fix:** Added a `layer=l1`/`layer=l2` tag to each function's hash input. Caught by `TestKeyAndNormalizedKeyNeverCollide` failing for the right reason.
**Prevention Rule:** Two key-fabrication functions meant to address logically distinct namespaces should be structurally distinguishable in their hash input, not just conventionally kept apart by which caller happens to use which.
**Category:** Architecture

### [Learning Entry - 2026-09-03T00:00:00Z]

**Context:** Kelvran's first release (`gateway/v0.1.0`) — grounding research before tagging.
**Mistake:** `gateway/go.mod`'s module path was `github.com/kelvran/gateway`, which does not correctly resolve via `go get` — `go.mod` lives one level below the repo root (the repo is `kelvran/gateway`, the module is at `gateway/go.mod`), so Go's resolution treated the module path as having no subdirectory, and `go get` returned a synthesized empty stub instead of the real dependency graph.
**Root Cause:** The module path was chosen to match the repo name rather than the actual filesystem location of `go.mod` relative to the repo root.
**Fix:** Changed the path to `github.com/kelvran/gateway/gateway`, matching Go's own subdirectory-module convention. Independently reproduced the bug live (fetched the real, broken `.mod` file from `proxy.golang.org`) before trusting the fix, then re-verified live after the fix and the real tag push.
**Prevention Rule:** Before tagging a first release, actually run `go get <module>@<branch>` against the real, already-public repo and inspect what comes back — don't infer resolution correctness from the module path looking reasonable.
**Category:** Bug Fix

### [Learning Entry - 2026-09-03T00:00:00Z]

**Context:** Guardrails v1 — the real end-to-end HTTP integration test's status-code assertion.
**Mistake:** `ErrGuardrailBlocked`, left unmapped in `cmd/gateway/main.go`'s `writeErrorResponse`, would have defaulted to `http.StatusBadGateway` (502) instead of the correct `400` — a content-policy rejection misclassified as an upstream failure. The integration test's own first, looser assertion (`>= 400`) would have passed either way, masking the bug.
**Root Cause:** A new sentinel error was added without updating every switch statement that classifies errors by status code, and the test written alongside it wasn't precise enough to catch the omission.
**Fix:** Mapped `ErrGuardrailBlocked` to `400` explicitly; tightened the integration test to assert the exact status code, not a range.
**Prevention Rule:** A new sentinel error needs its mapping added everywhere existing sentinels are switched on (status codes, `Outcome` enums, etc.) in the same pass it's introduced — and the test proving it should assert the exact expected value, never a loose bound. See §7.
**Category:** Bug Fix

### [Learning Entry - 2026-09-04T00:00:00Z]

**Context:** `evals` Rollout Scheduler pass — appending to `DECISIONS.md`.
**Mistake:** A new `DECISIONS.md` entry was inserted before the immediately-preceding entry, breaking that file's own append-only chronological convention.
**Root Cause:** The file's header rule (never edit past entries, always append at the true end) was not re-checked immediately before the edit.
**Fix:** Moved the new entry to the true end before treating the edit as done.
**Prevention Rule:** Before appending to any append-only file (`DECISIONS.md`, `docs/agents/LOGS.md`, this file's own Evolution Log), re-read the file's real current tail directly (e.g. `tail -c 300`), not from memory of what was there a few tool calls ago.
**Category:** Anti-Pattern

### [Learning Entry - 2026-09-04T00:00:00Z]

**Context:** `evals` LLM-judge wiring pass — appending to `DECISIONS.md` again.
**Mistake:** None this time — logged here because this is the deliberate, successful *avoidance* of the immediately-preceding entry's mistake, on the very next append.
**Root Cause:** N/A.
**Fix:** N/A — re-checked `DECISIONS.md`'s real tail via a direct `tail -c` read immediately before appending, specifically because the prior pass's own log entry recorded having broken this file's ordering once already. Confirmed the new entry landed after the true last line.
**Prevention Rule:** Same as the immediately-preceding entry — recorded again to show the rule was actually followed the very next time it applied, not just written down and forgotten.
**Category:** Best Practice

### [Learning Entry - 2026-09-04T00:00:00Z]

**Context:** Real judge-call cost tracking on `Score.cost_usd` — updating `_AnthropicCallModel.__call__` to read `response.usage`.
**Mistake:** The pre-existing `_FakeMessage` test fixtures in `test_providers.py` had no `.usage` attribute at all — all 3 pre-existing provider tests would have failed with `AttributeError` the moment the updated `__call__` tried to read `response.usage.input_tokens`.
**Root Cause:** A production code change started reading a new field on an object whose test fixtures were written before that field mattered, and the fixtures weren't audited for the new read before the change shipped.
**Fix:** Added a `_FakeUsage` class and gave every `_FakeMessage` a default `usage` attribute, in the same pass, before any test could break.
**Prevention Rule:** When production code starts reading a field on an object it previously ignored, audit every test fixture standing in for that object in the same pass — don't wait for the test run to discover the gap.
**Category:** Bug Fix

### [Learning Entry - 2026-09-04T00:00:00Z]

**Context:** `evals report --scores` cost aggregation — migrating `Score.cost_usd` from `float` to `Decimal`.
**Mistake:** 3 pre-existing test assertions compared a real `Decimal` result directly against a bare float literal (`7.00`, `0.0`, `0.0035`), which only happened to pass because those specific values were coincidentally exact in binary floating point.
**Root Cause:** `Decimal(...) == <float>` is not reliably safe in general (confirmed empirically both directions before writing any fix: `Decimal('0.0001234') == 0.0001234` is `False`) — these three assertions were quietly relying on binary-exactness that doesn't hold for most real values.
**Fix:** Compared against `Decimal(...)` literals in every case, instead of bare floats. Caught immediately by running the suite right after the type migration, not assumed safe.
**Prevention Rule:** Never compare a `Decimal` result against a bare float literal in a test — always wrap the expected value in `Decimal(...)` too, even when the current value happens to look "round." See §4.
**Category:** Bug Fix

### [Learning Entry - 2026-09-04T00:00:00Z] `[doc-staleness]`

**Context:** Guardrails v1 — closing the security-doc gap the feature itself was meant to close.
**Mistake:** `THREAT_MODEL.md` and `SECURITY.md` already asserted a "PII/content guardrail pre- and post-call" mitigation existed, in the present tense, while zero guardrail code existed anywhere in the repo.
**Root Cause:** The threat model was written describing the *intended* v1 architecture before any code existed, and nothing re-checked it once Guardrails itself was still unbuilt for several passes.
**Fix:** Building Guardrails v1 made the claim true; no separate doc-only fix was needed this time (the code caught up to the doc), but the gap itself is the same class of issue as the other 5 instances below.
**Prevention Rule:** See §2/§3.
**Category:** Anti-Pattern

### [Learning Entry - 2026-09-04T00:00:00Z] `[doc-staleness]`

**Context:** `evals` `Score` model pass — a doc-honesty cleanup performed before the feature work.
**Mistake:** `evals/ARCHITECTURE.md` claimed a `.importlinter` file exists (the Python analogue of `go-arch-lint`) — it doesn't, and nothing references it in CI or `pyproject.toml`. Found as "a third, previously-unflagged instance" while independently re-verifying two other claims from a prior pass's research.
**Root Cause:** Same as every other instance in this group — an enforcement mechanism was named in a doc as if real, describing the target architecture rather than the current one.
**Fix:** Corrected `evals/ARCHITECTURE.md` to state the mechanism doesn't exist yet.
**Prevention Rule:** See §2/§3.
**Category:** Anti-Pattern

### [Learning Entry - 2026-09-04T00:00:00Z] `[doc-staleness]`

**Context:** `evals` `Score` model pass — the same doc-honesty cleanup, gateway side.
**Mistake:** `gateway/ARCHITECTURE.md`'s Package Layout described `/internal/router` as a real, separate package with load-balancing/fallback-chain/circuit-breaker behavior — none of which existed; real routing was ~15 lines of inline round-robin in `dataplane.go`, and `go-arch-lint` (named as the dependency-rule enforcement mechanism) also didn't exist.
**Root Cause:** Same as every other instance — design-time architecture description never corrected once the simpler, real implementation shipped instead.
**Fix:** Rewrote the Package Layout entry to name exactly what's real (`nextDeployment`, round-robin + single fallback) vs. target-shape-not-built-yet.
**Prevention Rule:** See §2/§3. (This exact gap needed re-fixing again 2 days later — see the `[doc-staleness]` entry below for `docs/users/USER_GUIDE.md`'s separate description of the same feature.)
**Category:** Anti-Pattern

### [Learning Entry - 2026-09-04T00:00:00Z] `[doc-staleness]`

**Context:** `THREAT_MODEL.md` Gateway/MCP-A2A doc-honesty fix — found by a sanity-check fork before committing to the router RFC.
**Mistake:** `THREAT_MODEL.md`'s Gateway DoS row claimed circuit-breaker/backoff-jitter/retry-budget/concurrency-cap mechanisms with zero code anywhere in `gateway/internal/`; its Spoofing/Elevation-of-Privilege rows and the entire Cross-Component MCP/A2A table described mitigations for a subsystem (`gateway/internal/mcp`) that doesn't exist and is explicitly out of v1 scope.
**Root Cause:** Same as every other instance — threat-model rows were written against the intended full-scope design, not re-checked against the narrower real/deferred scope.
**Fix:** Corrected to name real, narrower backstops (per-key rate limiting/budget caps) vs. moot-until-MCP/A2A-exists, mirroring the pattern already used for Guardrails.
**Prevention Rule:** See §2/§3.
**Category:** Anti-Pattern

### [Learning Entry - 2026-09-04T00:00:00Z] `[doc-staleness]`

**Context:** `docs/users/USER_GUIDE.md`'s Routing & Failover section, found during the repo-wide doc-honesty sweep — the *second* time the router doc gap needed fixing.
**Mistake:** Even after `gateway/ARCHITECTURE.md`'s router entry was corrected (see the earlier `[doc-staleness]` entry above), `docs/users/USER_GUIDE.md` §4 still separately said routing was "(Not implemented yet.)" — now false for weighted routing, which shipped the same day.
**Root Cause:** Fixing one document that describes a feature does not fix every other document that also describes it — this is the clearest single proof of that root cause in this project's history so far (same underlying feature, two separate stale descriptions, corrected on two separate days).
**Fix:** Rewrote §4 (and, once looked at closely, §5/§7/§8, plus `README.md`'s/`USER_GUIDE.md`'s own top-level status banners and 6 other files — see the next entry) to match real, shipped behavior.
**Prevention Rule:** See §2/§3 — specifically the "grep the whole repo for the feature's keyword across every `*.md`, not just the one architecture doc" rule.
**Category:** Anti-Pattern

### [Learning Entry - 2026-09-04T00:00:00Z] `[doc-staleness]`

**Context:** Repo-wide "pre-scaffolding" doc-honesty sweep.
**Mistake:** The exact same "pre-scaffolding / no code exists / nothing has shipped" project-status claim, sometimes verbatim, was found live in `README.md`, `docs/users/USER_GUIDE.md`, `RELEASE_NOTES.md`, `UPGRADE.md`, `DEPRECATED.md`, `CONTRIBUTING.md`, `docs/operations/DEPLOY.md`, `AGENTS.md`, and `docs/agents/AGENTS_LEARNING.md` (this file, ironically) — all false given `gateway/v0.1.0`/`evals/v0.1.0` were tagged, released, and live 2 days earlier, and dozens of real features had shipped since.
**Root Cause:** Every one of these files was written once, during pre-scaffolding, with an honest "this doesn't exist yet" framing at the time — and never revisited as a category once the premise (no code exists) stopped being true, even though each individual doc-fix pass before this one kept fixing narrower, single-subsystem instances of the exact same underlying problem without ever asking "is this same class of claim wrong somewhere else too, at a larger scale?"
**Fix:** Corrected all 9 files in one pass; added a real, sourced `RELEASE_NOTES.md` v0.1.0 entry rather than just flipping a boolean-ish claim.
**Prevention Rule:** See §2/§3. This is the instance that crossed this pattern's 3+ recurrence threshold and triggered its own promotion into §1/§2/§3/§5 above.
**Category:** Anti-Pattern

### [Learning Entry - 2026-09-04T00:00:00Z]

**Context:** This very pass — populating this file, immediately after shipping the repo-wide doc-honesty sweep.
**Mistake:** While editing `STATUS.md`'s "Current Phase" section (replacing the router-pass paragraph with the doc-sweep paragraph), the `## Current Phase` heading line itself was accidentally dropped — the edit's old-text block had included the heading, the new-text block didn't, and nothing rendered the diff in a way that made the missing heading obvious at edit time.
**Root Cause:** An `Edit`-style exact-string replacement that spans a section heading plus its body will silently drop the heading if the replacement text isn't checked to include everything the original span included, not just the parts that changed.
**Fix:** Caught by re-grepping `STATUS.md`'s own heading list (`grep -n "^## "`) immediately after the edit and noticing `## Current Phase` was missing from it; restored the heading in the same pass.
**Prevention Rule:** After any edit that replaces a multi-paragraph block spanning a heading, re-grep the file's heading list (or otherwise re-render structure) before moving on — don't assume a successful string-replacement tool call means the surrounding structure survived intact.
**Category:** Bug Fix
