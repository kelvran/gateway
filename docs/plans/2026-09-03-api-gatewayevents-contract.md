> **For agentic executors:** Task 1 (the proto + buf tooling) must land before Task 2 (Go codegen + producer), which must land before Task 3 (Python codegen + consumer). Task 4 is CI/make wiring; Task 5 is last.

---

**Goal:** `api/gatewayevents/v1/gatewayevents.proto` is real, `buf breaking` runs in CI for the first time, `gateway` produces a real `GatewayDecisionEvent` on every request, and `evals` has a real (if minimal) consumer proving the generated Go and Python bindings agree on the wire format.

**Architecture:** One buf module at `api/` (v2 config) covering `otel/` (still empty — deliberately not built this pass) and `gatewayevents/` (real). `dataplane.Pipeline.finalize` derives a structured `Outcome` from the existing `err` via `errors.Is`, builds a `GatewayDecisionEvent`, and adds its `protojson`-encoded form as a new field on the existing structured JSON log line. `evals/ingestion/` decodes a checked-in fixture via generated Python bindings and asserts equality — the golden-fixture round-trip test `docs/testing/TESTING.md` §5 already promised.

**Tech Stack:** `buf` v2 CLI (new dev/CI tool, not a Go/Python dependency); `google.golang.org/protobuf` generated code + `protojson` (already a transitive dependency of `protoc-gen-go`'s runtime, no new `go.mod` entry beyond what codegen requires); Python `protobuf` runtime via `grpcio-tools` (new `evals` dev-dependency) — not `betterproto` (real, verified ownership churn).

**Spec:** `docs/rfcs/2026-09-03-api-gatewayevents-contract.md` — the exact proto schema, the `api/otel/`-stays-deferred reasoning, and the commit-generated-code-vs-generate-in-CI tradeoff live there; this plan implements them verbatim.

**Global Constraints** (inherited from the spec + `AGENTS.md`):
- `api/otel/` stays empty — do not create any file under it in this pass.
- `Outcome` must be derivable from `finalize`'s existing `err` parameter alone — no changes to `HandleChatCompletion`'s or `HandleChatCompletionStream`'s control flow, no new out-parameters threaded through `checkRateLimit` or the fallback retry logic.
- `gatewayevents_v1`'s encoded form is a new field on the *existing* structured JSON log line (`logRequest`) — no new log sink, file, queue, or transport of any kind.
- Generated Go and Python code is committed to the repo, not gitignored.
- Any change to `api/` requires declaring breaking-or-not per `buf breaking`'s categories, per `AGENTS.md`'s existing "ask first before changing `api/`" rule — this plan is the first-ever `api/` change, so there is nothing to break yet, but `buf breaking` must still run successfully (0 violations) as part of this plan's own verification.

---

## Task 1 — `api/` buf module + the real `.proto`

**Files:**
- Create: `api/buf.yaml`
- Create: `api/buf.gen.yaml`
- Create: `api/gatewayevents/v1/gatewayevents.proto`
- Modify: `api/README.md` (update "No `.proto` files exist yet" — it's no longer true for `gatewayevents/`; `otel/` stays as-is)

**Steps:**
- [ ] Install `buf` locally if not already present (`brew install buf`); confirm version.
- [ ] `api/buf.yaml`: `version: v2`, `modules: [{path: .}]`, real `lint`/`breaking` sections (start with buf's `STANDARD` lint ruleset and `FILE` breaking category — the plan's own Unresolved Question in the spec flags confirming this category matches the "v1 frozen, cut v2" workflow the RFC assumes; if `FILE` doesn't, adjust to `PACKAGE` and note why in the RFC's Unresolved Questions, don't silently pick one).
- [ ] Write `gatewayevents.proto` exactly as specified in the RFC (one message, `GatewayDecisionEvent`, the `Outcome` enum with the 7 named values + `OUTCOME_UNSPECIFIED`).
- [x] `buf lint` runs clean (0 issues). `buf breaking --against '.git#branch=main'` cannot run meaningfully *before* this pass's first commit — `main` has zero `.proto` files, so buf reports "had no .proto files" rather than "0 violations" (a real empirical finding, not the "trivially clean" outcome originally assumed here). Verified the mechanism itself works correctly instead, via `buf breaking --against <a local directory>` with a deliberately-broken copy (confirmed: 0 violations against an identical baseline, a real violation correctly flagged against a field-renumbered one). `buf breaking` against `main` becomes meaningful starting with the *next* commit that touches `api/`; this commit's own CI run compares the pushed commit against itself (trivially 0 violations) since `main` already includes it by the time CI checks it out.
- [ ] Update `api/README.md`'s "No `.proto` files exist yet" paragraph to describe the real `gatewayevents/v1/` state accurately, while leaving `otel/`'s description as still-a-placeholder — don't silently imply both are now real.

**Verify:** `cd api && buf lint && buf breaking --against '.git#branch=main'`

## Task 2 — Go codegen + `finalize`'s real producer (depends on Task 1)

**Files:**
- Create: `api/buf.gen.yaml`'s Go output directory (generated `.pb.go` file, committed) — exact path decided during implementation, likely `api/gatewayevents/v1/gatewayeventsv1/gatewayevents.pb.go` or a Go-module-relative path `gateway`'s own build can import cleanly; confirm the real generated import path resolves before treating this as done.
- Modify: `gateway/internal/gateway/dataplane/dataplane.go` (`finalize`: derive `Outcome` from `err`, build and `protojson.Marshal` the event, add the new field to `logRequest`'s JSON output)
- Modify: `gateway/internal/gateway/dataplane/dataplane_test.go` / a new small test file (assert the right `Outcome` for each of the existing sentinel-error rejection paths, and `OK` for a successful request — reusing this package's existing test-pipeline helpers, not new scaffolding)
- Modify: `gateway/go.mod`/`go.sum` (via `go get` for the generated code's own runtime dependency, `google.golang.org/protobuf`, if not already present — check first, since OTel may have already pulled part of this in transitively)

**Steps:**
- [ ] `go install google.golang.org/protobuf/cmd/protoc-gen-go@latest`; confirm `protoc-gen-go` is on `PATH`.
- [ ] `buf generate` from `api/`; confirm the generated `.pb.go` file's package/import path is something `gateway/go.mod`'s module can actually import (this may require `api/buf.gen.yaml`'s Go plugin `opt: module=github.com/kelvran/gateway` or an explicit `go_package` option in the `.proto` file itself — get this right empirically, don't guess the path and assume it compiles).
- [ ] Write the `Outcome`-deriving logic in `finalize`: a small, explicit `switch`/`errors.Is` chain (not a generic string-matching heuristic) mapping each of `ErrRateLimited`, `ErrBudgetExceeded`, `ErrModelNotAllowed`, `identity`'s verify error, "no deployment configured," and a final catch-all `UPSTREAM_ERROR` to the right enum value, plus `OK` for `err == nil`.
- [ ] Build the `GatewayDecisionEvent`, populate `trace_id`/`span_id` from `span.SpanContext()` (already available in `finalize`'s scope), `occurred_at` from the real clock, `virtual_key_id`/`requested_model` from existing parameters.
- [ ] `protojson.Marshal` it; add the result as a new `"gatewayevents_v1"` field to `logRequest`'s existing JSON log line — confirm the existing JSON structure's other fields are completely unchanged (a diff-the-log-output test, not just "it still compiles").
- [ ] Tests: for each of the 6 rejection sentinel paths plus the success path, assert the logged `gatewayevents_v1` field decodes back (via the same generated Go bindings, proving round-trip within one language first) to a `GatewayDecisionEvent` with the correct `Outcome` and the correct `virtual_key_id`/`requested_model`.

**Verify:** `cd gateway && go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./...`

## Task 3 — Python codegen + `evals/ingestion/`'s golden-fixture consumer (depends on Task 1)

**Files:**
- Modify: `evals/pyproject.toml` (add `grpcio-tools` as a dev-dependency for `protoc`-based codegen; confirm it doesn't conflict with the existing `hatchling`/`uv` setup)
- Create: generated Python bindings output directory (`evals/contracts/gatewayevents/v1/` or similar — confirm exact path against `evals/ARCHITECTURE.md`'s own already-named `contracts/` line)
- Create: `evals/ingestion/__init__.py`, `evals/ingestion/decode.py` (or similar minimal module — decode-only, no sampling/transport logic)
- Create: `evals/tests/testdata/gatewayevents_v1_sample.json` (the checked-in fixture — the SAME protojson output Task 2's Go-side test already proves is correct, copied verbatim, not independently re-authored, so both languages are provably decoding the identical bytes)
- Create: `evals/tests/test_ingestion_golden_roundtrip.py`

**Steps:**
- [ ] Add Python codegen to `api/buf.gen.yaml` (a `protoc`-family plugin producing plain Python protobuf runtime code — confirm the exact buf plugin name/invocation works locally before assuming the RFC's tooling choice is actually wired correctly, not just theoretically sound).
- [ ] `buf generate` produces real Python bindings; confirm `evals`' `uv`-managed environment can import them (add `protobuf` itself, not just `grpcio-tools`, as a real runtime dependency in `pyproject.toml` if the generated code needs it directly — check, don't assume `grpcio-tools` alone suffices at runtime).
- [ ] Copy the exact fixture Task 2's own Go-side round-trip test produces (run the gateway test, capture its `gatewayevents_v1` field's real output) into `evals/tests/testdata/` — this is the deliberate cross-language proof point, not a hand-written Python-side guess at what the JSON should look like.
- [ ] `evals/ingestion/decode.py`: one function, `decode_gateway_decision_event(raw: str) -> GatewayDecisionEvent`, wrapping `google.protobuf.json_format.Parse` against the generated message class — no sampling, no transport, no batching logic of any kind (explicitly out of scope per the RFC).
- [ ] `test_ingestion_golden_roundtrip.py`: loads the fixture, decodes it, asserts every field (`trace_id`, `span_id`, `virtual_key_id`, `requested_model`, `outcome`) matches the known input values — the actual golden-fixture round-trip proof `docs/testing/TESTING.md` §5 promised, now real for `gatewayevents` specifically.

**Verify:** `cd evals && uv sync && uv run pytest tests/test_ingestion_golden_roundtrip.py -v`

## Task 4 — CI + `make` wiring (depends on Tasks 1-3)

**Files:**
- Modify: `Makefile` (`gen-proto`, `check-proto` targets; wire `check-proto` into `verify`; wire `buf lint`/`buf breaking` into `lint`; update `help` text, remove the "Not yet real: buf breaking" line)
- Modify: `.github/workflows/ci.yml` (install `buf` via `bufbuild/buf-setup-action`; add a `check-proto` step before build/test in both the `gateway` and `evals` jobs, or a new shared `proto` job both depend on — decide which during implementation based on what's actually simplest to wire correctly, not assumed in advance)

**Steps:**
- [ ] `make gen-proto`: `cd api && buf generate`.
- [ ] `make check-proto`: `make gen-proto` then `git diff --exit-code` scoped to the generated-code paths only (not the whole repo — a stray unrelated uncommitted change elsewhere shouldn't fail this check).
- [ ] Wire `check-proto` into `verify`; wire `buf lint`/`buf breaking --against '.git#branch=main'` into `lint`.
- [ ] `.github/workflows/ci.yml`: add the `buf-setup-action` step; add the new checks to the existing job(s) — confirm this doesn't require restructuring the existing `gateway`/`evals` job split in a way that breaks anything already working.
- [ ] Push to a scratch branch or dry-run locally with `act`/manual review before trusting this against the real remote — per this project's own established "don't trust a CI change until you've watched a real run" discipline (see `docs/agents/LOGS.md`'s release-readiness entry for why).

**Verify:** `make verify` passes locally end-to-end; push and watch the real CI run to completion (`gh run watch`), don't just trust a green checkmark.

## Task 5 — Docs, Changelog, Wrap-Up

**Files:**
- Modify: `api/README.md` (already partially updated in Task 1 — final pass once everything is real)
- Modify: `gateway/ARCHITECTURE.md`, `evals/ARCHITECTURE.md`, root `ARCHITECTURE.md` (mark `gatewayevents` as real, `otel` as still-deferred — don't let any of the three docs imply both are done)
- Modify: `gateway/changelog/unreleased.md`, `evals/changelog/unreleased.md` (Added entries)
- Modify: `DECISIONS.md` (one line: `gatewayevents` real, `api/otel` reaffirmed-deferred, commit-generated-code-not-generate-in-CI divergence from the grounding research and why)
- Modify: `docs/agents/LOGS.md` (new append-only entry)
- Modify: `STATUS.md` (Current Phase, Verification State, Last Completed Task, Next Action)
- Modify: `docs/testing/TESTING.md` §5 (the golden-fixture round-trip test is no longer aspirational for `gatewayevents` — update the wording to say so, while keeping `api/otel`'s half of that sentence honestly marked as still not real)

**Verify:** re-run Task 4's full verify command once more after doc edits; cross-reference grep for every new doc's referenced paths; confirm no doc claims `api/otel/` is real anywhere.
