- **Status**: accepted
- **Date**: 2026-09-03
- **Author(s)**: project founder + Claude Code

## Summary

Give `api/` its first real protobuf contract: `api/gatewayevents/v1/gatewayevents.proto`, one message type (`GatewayDecisionEvent`) capturing the structured outcome of every `gateway` request — a real replacement for today's free-text error string, joinable to the existing OTel span by trace/span ID. `gateway` gets a real producer (encodes the message and adds it as a new field on the existing structured JSON log line); `evals` gets its first real consumer (`ingestion/`'s v1: decode a checked-in fixture via the generated Python bindings and assert field equality — the golden-fixture round-trip test `docs/testing/TESTING.md` §5 already promised). `buf breaking` becomes real in CI for the first time. **`api/otel/` is explicitly not built in this pass** — see "Why not `api/otel/` too" below; this is a reaffirmation of an already-made decision, not a re-litigation.

## Motivation

`api/` has been a placeholder since the initial scaffolding — `api/README.md` says as much: no `.proto` files exist, and the two subdirectories it names (`otel/`, `gatewayevents/`) are aspirational. Nothing forced this until now because neither deployable had a real reason to cross the language boundary. That's no longer true for one specific gap: `internal/gateway/dataplane.Pipeline.finalize` today calls `span.RecordError(err)`/`span.SetStatus(codes.Error, err.Error())` on any rejection (auth failure, model-not-allowed, rate-limited, budget-exceeded, no-deployment, upstream error) — a free-text string, not a queryable category. Anyone wanting to answer "how many requests failed because of budget vs. rate-limit this week" has to regex-parse error text, which is exactly the kind of "cost/usage/decision event schema" `api/README.md` already named as `gatewayevents`' job.

Grounded via a dedicated research pass (5 parallel angles + synthesis) before writing this RFC — see "Research trail" below.

## Detailed Design

### Why not `api/otel/` too — reaffirmed, not reopened

The OTel tracing RFC (`docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md`) already considered and explicitly rejected building `api/otel/`'s protobuf contract in the same pass as gateway's real span emission, for a reason that still holds verbatim: *"OTLP already IS a real, standard wire format for exporting spans to any backend. The protobuf contract's actual job (per `DESIGN.md`) is the `gatewayevents` cost/usage schema specifically."* This RFC's own research (independently, via a competitor survey of LiteLLM, Kong, Portkey, and TensorZero) confirms the same conclusion from a different angle: none of them invent a bespoke protobuf schema to hand span data to a downstream analytics consumer — it's either "subscribe to the same OTel/OTLP pipeline" (LiteLLM, Kong) or "skip OTel, share a database directly" (TensorZero, Portkey-internally) — the latter being a pattern `api/README.md` and `docs/decisions/0003-go-python-split.md` already rule out for Kelvran ("No shared database, no shared library"). If `evals` ever needs to consume gateway's spans, the mechanism is a real OTel Collector fan-out (gateway → Collector → a real backend + evals), not a parallel `api/otel/` proto — and that's its own future RFC, triggered by a real consumer need, not built speculatively here. `api/otel/` stays exactly as placeholder as `api/README.md` already describes it.

### `gatewayevents` v1 scope: one message, derived from data `finalize` already has

The research surfaced a real trap worth naming explicitly: it's easy to sketch a rich, multi-message schema (rejection reason, rate-limit fail-open flag, fallback-routing detail, budget-spend-at-decision-time) that looks complete but outruns what `gateway`'s code can actually, honestly populate today without new plumbing. Checked directly against `dataplane.go`:

- **Structured outcome — real, zero new plumbing.** `finalize` already receives `err` (the final `HandleChatCompletion` error, or nil). Every distinct rejection is already a distinct sentinel error (`ErrRateLimited`, `ErrBudgetExceeded`, `ErrModelNotAllowed`) or a wrapped verify/no-deployment/upstream error — `errors.Is` against the existing sentinels, checked once inside `finalize`, derives a real `Outcome` enum with no changes to `HandleChatCompletion`'s control flow at all.
- **Rate-limit fail-open flag — real, but NOT free.** `checkRateLimit`'s fail-open path returns `true` (allowed) when the Redis backend errors — from `finalize`'s vantage point that's indistinguishable from an ordinary successful rate-limit check; the signal never reaches `err`. Capturing it honestly needs either a new out-parameter threaded from `checkRateLimit` up through both call sites, or a second, decision-point-scoped event emitted directly from `checkRateLimit` itself. Real, but a second slice of plumbing this RFC doesn't build (see Unresolved Questions).
- **Fallback-routing detail — same problem, worse.** `finalize` only ever sees the *last* `dep` tried; whether a fallback occurred, from which deployment, and why, is discarded once a retry succeeds. Recovering this needs `HandleChatCompletion`'s fallback loop itself restructured to carry that state forward.
- **Budget spend-at-decision-time — not obtainable at all today.** `budget.Tracker` has no exported "current spend" getter; only the cap (`vk.BudgetUSD`) is in scope at the call site.

v1 ships only the first bullet. The other three are real, worth building eventually, and explicitly **not** built now — inventing fields for a producer that doesn't honestly exist yet is the same YAGNI violation the OTel RFC already flagged once for `api/otel/` itself.

```protobuf
// api/gatewayevents/v1/gatewayevents.proto
syntax = "proto3";
package gatewayevents.v1;

option go_package = "github.com/kelvran/gateway/api/gatewayevents/v1;gatewayeventsv1";

import "google/protobuf/timestamp.proto";

// One per HandleChatCompletion/HandleChatCompletionStream call. Joins to
// the OTel span via trace_id/span_id — never re-encodes gen_ai.*/cost/
// cache_hit, all of which are already real OTel span attributes per
// docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md.
message GatewayDecisionEvent {
  string trace_id = 1;
  string span_id = 2;
  google.protobuf.Timestamp occurred_at = 3;
  string virtual_key_id = 4;   // "" if auth itself failed
  string requested_model = 5;

  // Enum values are prefixed with OUTCOME_ per buf's STANDARD lint
  // ruleset (ENUM_VALUE_PREFIX) — a real, empirically-hit requirement,
  // not a style preference invented for this RFC.
  enum Outcome {
    OUTCOME_UNSPECIFIED = 0;
    OUTCOME_OK = 1;
    OUTCOME_AUTH_FAILED = 2;
    OUTCOME_MODEL_NOT_ALLOWED = 3;
    OUTCOME_RATE_LIMITED = 4;
    OUTCOME_BUDGET_EXCEEDED = 5;
    OUTCOME_NO_DEPLOYMENT = 6;
    OUTCOME_UPSTREAM_ERROR = 7;
  }
  Outcome outcome = 6;
}
```

### Producer: `finalize` encodes and logs it — no new transport invented

`evals`' `ingestion/` package (per its own `ARCHITECTURE.md` line: "consumes `api/gatewayevents` + `api/otel` only; production-trace sampling") implies a real production-trace pipeline eventually — but the actual transport (a queue, a periodic file export, a push endpoint) is genuinely undecided, and the research pass's own evals-ingestion-scope angle correctly flagged deciding it now as premature. This RFC ships a real producer without inventing that transport: `finalize` builds a `gatewayeventsv1.GatewayDecisionEvent`, encodes it via `protojson.Marshal` (the standard library-adjacent JSON representation of a protobuf message, shipped in the same `google.golang.org/protobuf` module the generated code already depends on — no new dependency), and adds it as a new `gatewayevents_v1` field on the same structured JSON log line `logRequest` already emits per request, right alongside the existing `cost_usd` field. This mirrors `cost_usd`'s own precedent exactly: a real, encoded, inspectable value on an already-existing, already-tested log line — not a new sink, queue, or file format. Whatever eventually ships gateway's structured logs to a durable store (already assumed to exist for production observability, unbuilt here) is the same mechanism that would carry this field forward to a real `evals` ingestion pipeline later.

### Consumer: `evals/ingestion/` v1 — the golden-fixture round-trip test, for real

`docs/testing/TESTING.md` §5 already promised this: *"a golden-fixture round-trip test, where `evals` decodes and `gateway` encodes the same checked-in set of `api/gatewayevents` and `api/otel` messages, confirming both languages' generated bindings agree on the wire format."* This RFC makes it real for `gatewayevents` (the `api/otel` half stays deferred alongside `api/otel/` itself): a checked-in fixture — one `GatewayDecisionEvent`, protojson-encoded by `gateway`'s real generated bindings — lives in `evals/tests/testdata/`, and a new `evals/ingestion/` module decodes it via the generated Python bindings and asserts field-for-field equality against the known input values, wired as a real pytest case (same pattern as the existing `test_llm_judge_prompt_golden.py`). Explicitly deferred: live production-trace sampling, any transport decision, and consuming a *running* gateway's output at all — this is a decode-and-verify proof that both languages' generated bindings genuinely agree on the wire format, which is the entire, real, non-speculative claim this pass makes.

### Tooling: `buf` v2, committed generated code, drift-checked in CI

- **`buf.yaml` v2** at `api/` root (`modules: [{path: .}]`) — the current, actively-maintained config version; v1 remains documented but not the path for a new module. Verified directly against buf's own current docs (`buf.build/docs`), not assumed from training data.
- **One buf module covers both `otel/` and `gatewayevents/`** subdirectories of `api/`, matching `api/README.md`'s own framing of `api/` as one shared surface with two named schemas — even though only `gatewayevents/` has real files yet.
- **`gateway/internal/cache/grpc/cache.proto`** (named in `gateway/ARCHITECTURE.md`'s Cache Subsystem as a dormant, not-yet-created extraction seam) is explicitly a *different* concern — Go-to-Go only, never consumed by Python — and must live in its own separate buf module if/when it's ever created, never folded into `api/`'s workspace; confirmed the file doesn't exist yet, so there's nothing to accidentally conflict with today.
- **Go codegen**: plain `protoc-gen-go` via a `local` plugin in `buf.gen.yaml` — no gRPC service plugin, since `gatewayevents` is message-only (an event schema, not an RPC service). Verified installable and working locally (`go install google.golang.org/protobuf/cmd/protoc-gen-go@latest`).
- **Python codegen**: plain `protobuf` runtime via `grpcio-tools`'s `protoc`, added as an `evals` dev-dependency — not `betterproto`. Checked directly: `betterproto`'s own README (fetched via `gh api`, not assumed) states *"Betterproto has moved to a new repository... This new version is a major redesign... still under active development... breaking changes may occur"* — real ownership churn, too risky for this contract's codegen path.
- **No Buf Schema Registry (BSR).** `buf generate` and `buf breaking --against '.git#branch=main'` both work fully offline — verified directly against `buf.build/docs`' own reference. BSR's value (remote plugin pinning, external SDK distribution, cross-repo dependency management) doesn't apply to a single-repo, no-external-consumer, pre-1.0 project.
- **Generated code is committed, not generated-in-CI-only** — a deliberate divergence from this RFC's own research, which flagged "generate in CI, don't commit" as its own inferred judgment call, not a verified finding. The call here: committing keeps `go build ./...` and `pip install -e .`/`pytest` self-contained from a fresh clone with zero `buf` toolchain required, matching this project's own established bias toward minimal build-time dependencies (the hand-rolled YAML config parser exists for exactly this reason). The risk the research worried about — generated code silently drifting from its `.proto` source — is addressed differently: CI re-runs `buf generate` and fails the build if the working tree comes out dirty (`git diff --exit-code`), which requires the exact same `buf`-in-CI investment either way (CI already needs `buf` installed to run `buf breaking`), so committing adds no net new CI cost while removing a runtime dependency for every contributor who just wants to build.

### `buf breaking` and versioning

Package/directory-based versioning, per buf's own convention (and Google's AIP-215): `package gatewayevents.v1;`, directory `api/gatewayevents/v1/`. No `kelvran.` org prefix — buf's STANDARD lint ruleset enforces `PACKAGE_DIRECTORY_MATCH` (the package path must equal the file's directory path exactly), and `api/README.md` already fixed `gatewayevents/`/`otel/` as top-level directories, predating this RFC; matching that meant dropping the prefix rather than restructuring directories to add a `kelvran/` nesting level. `buf breaking`'s comparison scope is the package/directory path, so `v1` becomes frozen by construction the moment it ships — an incompatible change means cutting a sibling `v2/` that coexists until both `gateway` and `evals` regenerate against it, then retiring `v1`, which is a literal implementation of `RELEASE.md`'s existing step 3 ("never let one deployable release against a contract version the other hasn't caught up to"). The package-version bump **is** the "contract's own version identifier" `RELEASE.md` step 2 already commits to — no separate `api/VERSION` file, no independent semver tag on `api/`, and no new `api/CHANGELOG.md`: `api/` isn't independently tagged/released the way `gateway`/`evals` are, and the required prose record for a breaking change already has a home — `UPGRADE.md`'s existing table (confirmed it exists, with exactly a `Version | Breaking Change | Migration Steps` header, currently empty) — with the Version column holding the package version (e.g. `gatewayevents v2`), not a deployable version.

### CI and `make` wiring

`Makefile` gains a `gen-proto` target (`buf generate` from `api/`) and a `check-proto` target (`gen-proto` then `git diff --exit-code` on the generated paths); `lint` gains `buf lint` + `buf breaking --against '.git#branch=main'` (a no-op — nothing to break — until `v2` ever exists); `verify`/CI gain `check-proto`. `.github/workflows/ci.yml` installs `buf` (the official `bufbuild/buf-setup-action`) alongside the existing `setup-go`/`setup-uv` steps.

## Drawbacks

- Fifth external tool this project now depends on for a full `make verify` run (`buf`, alongside `golangci-lint`, `ruff`, `go`, `uv`) — mitigated: `buf` is only needed for `make gen-proto`/`check-proto`/the new lint/breaking steps, never for a plain `go build`/`go test` against already-committed generated code.
- Committing generated code means two extra files (one Go, one Python) whose byte-for-byte content is fully determined by the `.proto` source and toolchain version — a real, if small, review-noise cost on every future schema change, accepted for the self-contained-build benefit described above.
- `gatewayevents_v1`'s JSON-log-field transport is explicitly a stopgap, not a real production-events pipeline — genuinely useful for proving the contract works end-to-end, not yet useful for `evals` to actually sample production traffic from. That gap is named, not hidden.

## Alternatives Considered

1. **Build `api/otel/` in the same pass** — rejected; reaffirms the OTel RFC's own prior, still-correct decision (see "Why not `api/otel/` too" above), not a new argument.
2. **A richer first-pass `gatewayevents` schema** (rate-limit fail-open, fallback routing, budget-spend-at-decision-time) — rejected for v1: none of those are honestly obtainable from `finalize` without real code restructuring beyond "add one emission call," and inventing the fields ahead of a real producer is the exact trap this RFC's own research flagged.
3. **`betterproto` for Python codegen** — rejected: real, verified ownership churn (moved to an actively-breaking-changes fork).
4. **Buf Schema Registry (BSR) from day one** — rejected: no capability it adds (remote plugin pinning, external SDK distribution, cross-repo deps) applies to this project's current single-repo, no-external-consumer shape.
5. **Generate-in-CI, don't commit generated code** — this RFC's own grounding research recommended this, flagged as its own inferred judgment call rather than a verified finding; overridden here for the reasons in "Tooling" above (self-contained builds beat avoiding review noise, and the drift risk is addressed by a CI diff check instead).
6. **A real transport (queue/file-export) for `gatewayevents` now** — rejected: the research pass explicitly found this transport question unresolved and out of scope for a first real pass; building one now would be exactly the "producer race ahead of a real consumer" problem this RFC is trying to avoid in the other direction.

## Unresolved Questions

- Whether an OTel Collector exists or is planned anywhere in Kelvran's deployment topology (checked against `docs/operations/DEPLOY.md`? — not yet, this RFC's research didn't verify it) — relevant if/when `api/otel/`'s deferred consumption path is ever revisited.
- The real transport for `gatewayevents` at production scale (queue vs. periodic file export vs. push endpoint) — deliberately undecided here.
- Whether/when to build the rate-limit-fail-open and fallback-routing slices this RFC explicitly deferred — real, useful, not yet built.
- Producer-side latency cost of adding `protojson.Marshal` + a new JSON field to every `finalize` call — unmeasured; expected to be small (this is comparable-sized JSON work to what `logRequest` already does per request) but not benchmarked.
- Whether `buf breaking`'s `FILE` category (the one actually configured in `api/buf.yaml`) enforces the "`v1` frozen by construction, cut `v2` for anything incompatible" workflow this RFC assumes — **confirmed empirically during implementation**: a deliberate field renumbering, compared via `buf breaking --against <a local directory>`, was correctly flagged ("Previously present field... was deleted"). Not yet confirmed specifically via `.git#branch=main` against a real prior commit, since this RFC's own commit is the first one to ever add a `.proto` file here — see the next item.
- **`buf breaking --against '.git#branch=main'` cannot run meaningfully before this RFC's own first commit** — confirmed empirically, not assumed: run locally pre-commit, it errors with `Module "path: "api"" had no .proto files` rather than reporting "0 violations," since `main` genuinely has zero `.proto` files at that point. This is expected, one-time, and self-resolving: starting with this RFC's own commit, `main` has the schema, and `buf breaking` becomes meaningful for every subsequent `api/`-touching change. This pass's own CI run compares the just-pushed commit against itself (trivially 0 violations, since `main` already includes it by the time CI checks it out) — not a special case handled in code, just what the comparison naturally resolves to.

## Research Trail

Grounded via a dynamic-workflow research pass (5 parallel angles: `api/otel` purpose given gateway's real existing OTLP pipeline, `gatewayevents` concrete scope grounded in `dataplane.go`'s actual code, buf/codegen tooling, contract versioning, and `evals/ingestion/`'s minimal real first-pass scope — plus a synthesis merging all five). The synthesis's own flagged "real disagreement between reports" (whether `api/otel/` should be a bespoke proto at all) was resolved by directly re-reading `docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md`'s own prior text rather than trusting either side's framing — confirming the "no bespoke `api/otel/` proto" position was correct, and already this project's own standing decision. The `buf.yaml` v2 config shape and the "works fully offline, no BSR needed" claim were independently re-verified via `mcp__context7` against buf's real current docs before being written into this RFC; the `betterproto` churn claim was independently re-verified via `gh api` against that project's actual README.
