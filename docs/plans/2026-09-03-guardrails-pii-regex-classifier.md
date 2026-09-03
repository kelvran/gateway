> **For agentic executors:** Task 1 (guardrail package) is independent and must land first — everything else depends on its types. Task 2 (cache-key/L3 provenance extension) and Task 3 (gatewayevents Outcome addition) are independent of each other and of Task 4, but both must land before Task 4 (dataplane/streaming wiring), which needs `guardrailPolicyVersion` threading and `OUTCOME_GUARDRAIL_BLOCKED` to exist. Task 5 is controlplane config + `cmd/gateway` wiring. Task 6 is docs/changelog/wrap-up.

---

**Goal:** A real, working pre-call and post-call content guardrail — pure-Go regex/checksum PII+secrets detection plus a prompt-injection heuristic, category-tiered fail-open/fail-closed policy, wired exactly where `gateway/ARCHITECTURE.md`'s Request Lifecycle already documents it — closing the real gap where `THREAT_MODEL.md`/`SECURITY.md` currently claim a mitigation that doesn't exist in code.

**Architecture:** New `gateway/internal/guardrail` package (`Engine`/`Detector`/`Policy`/`Verdict`/`Finding` types + 8 detector files). `dataplane.Config` gains a required `Guardrails *guardrail.Engine` field, mirroring `CacheL2`/`CacheL3`/`Limiter`/`Budget`. Pre-call runs after L1/L2/L3 all miss, before the router; post-call runs after upstream succeeds, before cache write-back on the buffered path, and audit-only (no enforcement) on the streaming path. Cache-hit safety: `cache.Key`/`cache.NormalizedKey` gain a `guardrailPolicyVersion` hash input (L1/L2); `LexicalCandidate`/`LexicalCache.Put` gain an explicit `GuardrailPolicyVersion` field, checked as a new sibling gate in `checkLexicalCache` (L3).

**Spec:** `docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md` — the exact detector list, fail-open/closed policy table, pipeline integration points (with real line-number citations), and the cache-key-vs-stored-field design correction all live there; this plan implements them verbatim.

**Global Constraints** (inherited from the spec):
- Zero new `go.mod` entries — every detector is pure `regexp`/`unicode`/stdlib arithmetic (Luhn, ISO 7064 mod-97). No ML, no NER, no ONNX/GGML, no upstream API call.
- No free-text NER, no ML-grade toxicity/moderation model, no CSAM detection (not applicable — text-only v1), no CBRN classification. Do not scope-creep into any of these "while you're at it."
- `guardrail` must never import `adapter`, `cache`, or any provider-specific package — text in, `Verdict` out.
- The guardrail-policy-version check for L3 is a NEW, SEPARATE inline check in `checkLexicalCache` — never folded into `freshnessRiskModel`, which stays scoped to its own documented checklist.
- `Config.Guardrails` is hard-required (`NewPipeline` validation error if nil) — never optional the way `UpstreamStream` is.
- Post-call on the streaming path is audit-only — never attempt to block/withhold already-flushed chunks. Do not add a buffering layer to `streamDeployment`'s write loop.

---

## Task 1 — `guardrail` package: types, engine, detectors

**Files:**
- Create: `gateway/internal/guardrail/types.go` (`Category`, `Action`, `Finding`, `Verdict`, `Detector` interface)
- Create: `gateway/internal/guardrail/policy.go` (`Policy` struct, `DefaultPolicy()` returning the spec's fail-open/closed table)
- Create: `gateway/internal/guardrail/engine.go` (`Engine`, `NewEngine`, `Version`, `Check`)
- Create: `gateway/internal/guardrail/{email,phone,ssn,iban,creditcard,ipaddress,secretkey,promptinjection}.go` — one `Detector` implementation each
- Create matching `_test.go` for every file above

**Steps:**
- [ ] `types.go`: `Category` string-const type (`credential`/`financial_id`/`government_id`/`contact_info`/`network_id`/`prompt_injection`), `Action` int-const type (`ActionWarn`/`ActionBlock`), `Finding{Category, Detector string, Start, End int}`, `Verdict{Blocked bool, Findings []Finding, DetectorError error}`, `Detector` interface (`Name() string; Category() Category; Detect(ctx, text string) ([]Finding, error)`).
- [ ] `policy.go`: `Policy{Actions map[Category]Action; ErrorActions map[Category]Action}`; `DefaultPolicy()` returns exactly the spec's table (Block: `credential`/`financial_id`/`government_id`; Warn: `contact_info`/`network_id`/`prompt_injection`, both axes).
- [ ] `email.go`: RFC-5322-shaped regex, category `contact_info`.
- [ ] `phone.go`: NANP + generic E.164 regional regex set, category `contact_info`.
- [ ] `ssn.go`: US SSN pattern regex, category `government_id`. Deliberately US-only in v1 — say so in the doc comment, don't silently imply broader coverage.
- [ ] `iban.go`: pattern regex + real ISO 7064 mod-97 checksum implementation (not a stub) — a candidate string that matches the pattern but fails the checksum must NOT be reported as a finding (test this explicitly).
- [ ] `creditcard.go`: pattern regex + real Luhn checksum implementation — same false-positive-rejection requirement as IBAN, test explicitly with a pattern-matching-but-checksum-failing number.
- [ ] `ipaddress.go`: IPv4 + IPv6 pattern regex (use `net.ParseIP` for validation after a coarse regex pre-filter, not a hand-rolled IP-format regex alone — Go's stdlib already does this correctly).
- [ ] `secretkey.go`: the exact prefix list from the spec (`sk-`, `sk-ant-`, `sk-proj-`, `ghp_`/`gho_`/`ghu_`/`ghs_`/`ghr_`, `AKIA`/`ASIA`, `sk_live_`/`pk_live_`/`sk_test_`) plus the generic high-entropy assignment pattern with a real Shannon-entropy calculation (≥3.5 threshold) — implement entropy calculation as a small, tested, standalone function, not inlined.
- [ ] `promptinjection.go`: verb×preposition combinatoric fuzzy match (Kong/LiteLLM-style: "ignore/disregard/skip/forget" × "prior/previous/preceding instructions") plus hidden-Unicode detection (zero-width `U+200B`–`U+200D`/`U+FEFF`, bidi-control `U+202A`–`U+202E`, Unicode tag chars `U+E0020`–`U+E007F`) as two separate checks inside one `Detect` — a message can trigger either or both, both are still `Category() == CategoryPromptInjection`.
- [ ] `engine.go`: `Engine{detectors []Detector; policy Policy; version string; logger *slog.Logger}`; `NewEngine(detectors, policy, version, logger) *Engine`; `Version() string`; `Check(ctx, text string) Verdict` — runs every detector, collects findings by category, applies `policy.Actions`/`policy.ErrorActions` to decide `Blocked`, logs every Block-tier hit and every detector error at Warn+ level (never silent).
- [ ] `DefaultDetectors()` helper in `engine.go` or a small `detectors.go` — returns the 8 detectors above as `[]Detector`, for `cmd/gateway` to consume without hand-listing them.
- [ ] Tests per detector: at least one true-positive, one true-negative (adjacent-but-non-matching text), and for checksum-backed detectors (IBAN, credit card) one pattern-matches-but-checksum-fails case proving it's correctly NOT reported. `engine_test.go`: a Block-tier finding sets `Verdict.Blocked = true`; a Warn-tier-only finding does not; a detector returning an error on a Block-tier category sets `Blocked = true` (per `ErrorActions`); the same on a Warn-tier category does not.

**Verify:** `cd gateway && go build ./internal/guardrail/... && go test ./internal/guardrail/... -race`

## Task 2 — Cache-key / L3 provenance extension for guardrail-policy versioning

**Files:**
- Modify: `gateway/internal/cache/key.go`, `gateway/internal/cache/key_test.go`
- Modify: `gateway/internal/cache/lexical.go`, `gateway/internal/cache/lexical_test.go`
- Modify: `gateway/internal/cache/inprocess/lexical.go`, `gateway/internal/cache/inprocess/lexical_test.go`

**Steps:**
- [ ] `key.go`: add a `guardrailPolicyVersion string` parameter to both `Key(...)` and `NormalizedKey(...)`, hashed in as one more `\x00`-tagged field (`\x00policy=%s`) alongside the existing `tenant`/`model`/`messages`/`temperature`/`max_tokens` fields — placement in the format string doesn't matter for correctness, but keep it adjacent to `model=` for readability, matching how the RFC frames "one more isolation dimension."
- [ ] `key_test.go`: a test proving two otherwise-identical `Key`/`NormalizedKey` calls with different `guardrailPolicyVersion` values produce different hashes (mirroring the existing `TestKeyAndNormalizedKeyNeverCollide`-style precedent).
- [ ] `lexical.go`: add `GuardrailPolicyVersion string` to `LexicalCandidate`; add a `guardrailPolicyVersion string` parameter to `LexicalCache.Put(...)`.
- [ ] `inprocess/lexical.go`: thread the new `Put` parameter through to the stored `lexicalEntry` and back out via `Search`'s returned `LexicalCandidate.GuardrailPolicyVersion` — mirrors exactly how `ModelID` is already threaded.
- [ ] Tests: a `Put` with policy version A, `Search`ed after a `Put` with policy version B for the same tenant/signature, must NOT be conflated in the returned candidate's `GuardrailPolicyVersion` field (i.e., prove the field round-trips correctly per-entry) — this is a data-plumbing test, the actual "does a mismatch block the hit" check belongs in Task 4's dataplane tests, not here.

**Verify:** `cd gateway && go build ./internal/cache/... && go test ./internal/cache/... -race`

## Task 3 — `gatewayevents/v1`: `OUTCOME_GUARDRAIL_BLOCKED`

**Files:**
- Modify: `api/gatewayevents/v1/gatewayevents.proto`
- Regenerate: `gateway/api/gatewayevents/v1/gatewayevents.pb.go`, `evals/evals/contracts/gatewayevents/v1/gatewayevents_pb2.py`

**Steps:**
- [ ] Add `OUTCOME_GUARDRAIL_BLOCKED = 8;` to the `Outcome` enum (next free value after the existing `OUTCOME_UPSTREAM_ERROR = 7`) — purely additive, same non-breaking category as the prior `gatewayevents-decision-enrichment` field additions.
- [ ] `make gen-proto` (remember: `export PATH="$(go env GOPATH)/bin:$PATH"` first if `protoc-gen-go` isn't found).
- [ ] `cd api && buf lint && buf breaking --against '../.git#branch=main,subdir=api'` — confirm clean, 0 violations, same as every prior proto change this session.

**Verify:** `buf lint` and `buf breaking` both clean; `git diff` on the two generated files shows only the expected new-enum-value diff.

## Task 4 — `dataplane`/`streaming` wiring: pre-call, post-call, `ErrGuardrailBlocked`

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go`
- Modify: `gateway/internal/gateway/dataplane/streaming.go`
- Modify: `gateway/internal/gateway/dataplane/dataplane_test.go`, `streaming_test.go`, `gatewayevents_test.go`, `cache_l2_test.go`, `lexical_cache_test.go` (every test helper that constructs a `Config{}` literal needs a `Guardrails` value, mirroring exactly how `CacheL3: inprocess.NewLexicalCache(0)` was added to every helper during the Cache L3-lite pass)
- Create: `gateway/internal/gateway/dataplane/guardrail_test.go` (new load-bearing full-pipeline tests)

**Steps:**
- [ ] Add `ErrGuardrailBlocked = errors.New("dataplane: request blocked by guardrail policy")` sentinel, alongside the existing `ErrRateLimited`/`ErrBudgetExceeded`/etc.
- [ ] Add `case cfg.Guardrails == nil: return nil, fmt.Errorf("dataplane: Config.Guardrails is required")` to `NewPipeline`'s validation switch; add `Guardrails *guardrail.Engine` to `Config`; add `guardrails *guardrail.Engine` to `Pipeline`; add `guardrails: cfg.Guardrails,` to the `&Pipeline{...}` literal.
- [ ] Update `outcomeFor` to classify `errors.Is(err, ErrGuardrailBlocked)` → `gatewayeventsv1.GatewayDecisionEvent_OUTCOME_GUARDRAIL_BLOCKED`, in the same `switch` as every other sentinel.
- [ ] `HandleChatCompletion`: after the L3 lexical-cache-check block, before `var found bool`/`nextDeployment`, insert:
  ```go
  if verdict := p.guardrails.Check(ctx, serializeMessages(req.Messages)); verdict.Blocked {
      p.logger.Warn("guardrail_blocked_precall", "key_id", vk.ID, "findings", len(verdict.Findings))
      err = ErrGuardrailBlocked
      return
  }
  ```
  Update `l1Key`/`l2Key` construction (a few lines earlier, unchanged position) to pass `p.guardrails.Version()` as the new `cache.Key`/`cache.NormalizedKey` parameter. Update the `l3Signature`-adjacent `checkLexicalCache` call path (Task 4's own sub-step below) to pass the version through for the L3 read-side check.
- [ ] `checkLexicalCache`: add the new sibling check — `if c.GuardrailPolicyVersion != p.guardrails.Version() { continue }` — placed alongside the existing `fingerprintsEqual`/`freshnessRiskModel` checks in the same loop, same "any mismatch skips this candidate" discipline.
- [ ] `writeCache`: thread `p.guardrails.Version()` through to the new `LexicalCache.Put` parameter (Task 2's addition).
- [ ] `HandleChatCompletion`, post-call: after the unified upstream-error check closes, before cache write-back, insert:
  ```go
  if postVerdict := p.guardrails.Check(ctx, serializeResponse(resp)); postVerdict.Blocked {
      p.logger.Warn("guardrail_blocked_postcall", "key_id", vk.ID, "findings", len(postVerdict.Findings))
      err = ErrGuardrailBlocked
      return
  }
  ```
  (New tiny helper `serializeResponse(resp adapter.ChatResponse) string` — extracts the response's text content for scanning; keep it minimal, mirroring `serializeMessages`' own scope.)
- [ ] Mirror all of the above in `streaming.go`'s `HandleChatCompletionStream` for the pre-call check and cache-key/L3-version threading — identical shape, same insertion point (after the L3 check block, before `nextDeployment`).
- [ ] `streaming.go`'s `streamDeployment`: after `resp := acc.build(usage)`, before `sw.WriteDone()`, insert the audit-only post-call check:
  ```go
  if postVerdict := p.guardrails.Check(ctx, serializeResponse(resp)); postVerdict.Blocked {
      p.logger.Warn("guardrail_blocked_postcall_streaming_audit_only", "deployment", dep.Name, "findings", len(postVerdict.Findings))
      // Audit-only: content has already been flushed to the client and
      // cannot be withheld — see the RFC's named residual-risk section.
  }
  ```
  Do NOT set `err` here and do NOT block `sw.WriteDone()` — this is the decisive audit-only design from the RFC, not a bug to "fix" by trying to block.
- [ ] Update every existing test helper's `Config{}` literal to supply `Guardrails: guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", discardLogger())` (or an equivalent minimal test engine) — mirrors the Cache L3-lite pass's own "add the new required field to every helper" step exactly.
- [ ] New tests in `guardrail_test.go`:
  - A request whose content matches a `Block`-tier pattern (e.g. a message containing a syntactically valid, Luhn-valid credit card number) is rejected pre-call, never reaches the mock upstream.
  - A request whose content matches only a `Warn`-tier pattern (e.g. a message containing an email address) is NOT blocked — it proceeds normally, and the mock upstream IS called (proves Warn tier doesn't block).
  - A response whose content matches a `Block`-tier pattern is rejected post-call on the buffered path — the mock upstream IS called (it's a post-call check), but the response is never written to cache and the caller gets `ErrGuardrailBlocked`.
  - Streaming: a response matching a `Block`-tier pattern still has ALL its chunks delivered to the client (proving audit-only, not enforcement) — assert the full body was written, not truncated.
  - A cache entry written under policy version "v1" is a genuine miss when the engine's version changes to "v2" for the same request content, on all of L1 and L2 (two separate tests or one parameterized test) — and separately for L3, proving the `GuardrailPolicyVersion` mismatch check works end-to-end, not just as a data-plumbing round-trip.
  - `TestOutcomeForClassifiesEverySentinelError` (existing test) gets `ErrGuardrailBlocked` added to its table.

**Verify:** `cd gateway && go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./...` — all packages `ok`, `0 issues`, race-clean.

## Task 5 — `controlplane` config + `cmd/gateway` wiring

**Files:**
- Modify: `gateway/internal/gateway/controlplane/config.go`, `config_test.go`
- Modify: `gateway/cmd/gateway/main.go`
- Modify: `gateway/config.example.yaml`

**Steps:**
- [ ] Add `GuardrailsConfig{PolicyVersion string; CategoryOverrides map[string]string}` and `Guardrails GuardrailsConfig` on `Config`, doc-commented "Optional — zero value runs all v1 default detectors under the RFC's default policy."
- [ ] Add the `if guardrailsRaw, ok := getMap(root, "guardrails"); ok { ... }` block in `Load()`, alongside the existing optional-section blocks, before `price_table` parsing.
- [ ] Test mirroring `TestLoadCacheSectionParsesL1AndNestedL2`: a config with a `guardrails:` section parses `policy_version`/`category_overrides` correctly; a config WITHOUT one defaults to the zero value (genuinely optional, confirmed via a real parse, not assumed).
- [ ] `cmd/gateway/main.go`: construct `guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), cfg.Guardrails.PolicyVersion, logger)` (fall back to a sensible default version string like `"v1"` if `PolicyVersion` is empty) and wire it into `dataplane.Config{Guardrails: ...}`, following exactly how `CacheL2`/`CacheL3`/`Budget`/`Limiter` are already constructed there.
- [ ] Update `gateway/config.example.yaml` with a commented-out `guardrails:` section example, matching the style of the existing `cache:`/`budget:`/`rate_limit:` examples.
- [ ] A real end-to-end HTTP integration test in `cmd/gateway/integration_test.go`: a request with a Block-tier pattern (e.g. a fake-but-Luhn-valid credit card number) against the real `buildPipeline` wiring returns a 4xx, never reaches the mock upstream.

**Verify:** `cd gateway && go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./...`; root `make verify`.

## Task 6 — Docs, changelog, wrap-up

**Files:**
- Modify: `gateway/ARCHITECTURE.md` (correct line 136's "PII/NER" wording; add `guardrail` to the Dependency direction rules block)
- Modify: `gateway/changelog/unreleased.md`
- Modify: `DECISIONS.md`
- Modify: `docs/agents/LOGS.md`
- Modify: `STATUS.md`

**Steps:**
- [ ] `ARCHITECTURE.md:136`: change *"Ships with a basic PII/NER + regex classifier at v1"* to accurately name what's real (regex/checksum PII+secrets detection plus a prompt-injection heuristic — no NER), mirroring exactly how the L3-lite RFC corrected its predecessor's "real embedding-based semantic matching" framing.
- [ ] `ARCHITECTURE.md`'s Dependency direction rules block: add a `guardrail` line (never imports `adapter`/`cache`/provider-specific packages).
- [ ] `gateway/changelog/unreleased.md`: new `## Added` entry describing the guardrail subsystem, its v1 scope (what's real, what's explicitly deferred and why), the fail-open/closed policy, and the pipeline integration (including the streaming audit-only limitation) — matching the detail level of the Cache L3-lite changelog entry.
- [ ] `DECISIONS.md`: one new line at the true chronological end (re-check `tail` immediately before appending), naming the build, the narrowed v1 scope, the category-tiered fail-open/closed policy, the cache-key-vs-L3-field design correction, and the explicitly-deferred items (NER, ML moderation, real-time streaming enforcement, CSAM/CBRN not-applicable-yet).
- [ ] `docs/agents/LOGS.md`: new entry (Files touched / Intent-summary / Decisions made / Verification performed / Bugs found / Next steps).
- [ ] `STATUS.md`: update Current Phase / Last Completed Task / Next Action / Verification State / Active Blockers as needed (note: this closes THREAT_MODEL.md's Information-Disclosure/Elevation-of-Privilege guardrail-mitigation gap — worth a real callout here, not just in DECISIONS.md).
- [ ] Full `make verify` from repo root — must pass clean before commit.

**Verify:** `make verify` (root) passes end-to-end; `git diff` reviewed in full before committing.
