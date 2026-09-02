> **For agentic executors:** work through this task-by-task. Task 1 is foundational and must land before Tasks 2/3/4. Task 2 (config) is independent of Task 3 (dataplane) and can run in parallel with it. Task 4 depends on both 2 and 3. Task 5 is last.

---

**Goal:** Real OTel spans for the gateway's dataplane, with `agent_run_id` propagated in via W3C Baggage and GenAI semantic-convention attributes plus Kelvran-custom ones.

**Architecture:** A new `gateway/internal/telemetry` package (TracerProvider setup, context extraction, the shared "finish a span" helper) wired into `dataplane.HandleChatCompletion`/`HandleChatCompletionStream` and `cmd/gateway`'s HTTP handlers.

**Tech Stack:** `go.opentelemetry.io/otel` + `sdk` + `exporters/stdout/stdouttrace` + `exporters/otlp/otlptrace/otlptracehttp` — the first external Go dependencies this module has ever had, pre-approved by `gateway/ARCHITECTURE.md`'s Tech Stack table.

**Spec:** `docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md` — exact attribute names, the `ChatCompletionResult` shape, and the propagation design live there; this plan implements them verbatim.

**Global Constraints** (inherited from the spec + `AGENTS.md`):
- Never add prompt/completion content to a span, not even behind a flag — this pass builds no content-capture mechanism at all.
- `internal/telemetry` must not import `internal/identity` or `internal/adapter` — it takes primitive values only (mirrors `internal/budget`'s existing leaf-package discipline).
- `agent_run_id` is never fabricated: absent Baggage means an absent attribute, not an empty string written anyway.
- Existing `buildPipeline`-based test helpers (`cmd/gateway/integration_test.go`) must keep working unmodified — `telemetry.Init` is called from `run()`, not from `buildPipeline`.

---

## Task 1 — `internal/telemetry` package (foundational)

**Files:**
- Modify: `gateway/go.mod`, `gateway/go.sum` (via `go get` — first external dependencies)
- Create: `gateway/internal/telemetry/telemetry.go` (`Config`, `Init`, `ExtractContext`, `AgentRunIDFromContext`, `Tracer`, attribute-key constants)
- Create: `gateway/internal/telemetry/result.go` (`ChatCompletionResult`, `RecordChatCompletionResult`)
- Create: `gateway/internal/telemetry/telemetry_test.go`, `result_test.go`

**Steps:**
- [ ] `go get go.opentelemetry.io/otel go.opentelemetry.io/otel/sdk go.opentelemetry.io/otel/exporters/stdout/stdouttrace go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp` from `gateway/`.
- [ ] `Init(ctx, cfg)`: build a `sdktrace.TracerProvider` with `WithResource` (`service.name = "kelvran-gateway"`) and the exporter selected by `cfg.Exporter` (`""`/`"stdout"` → `stdouttrace.New()`; `"otlp"` → `otlptracehttp.New(ctx, otlptracehttp.WithEndpoint(cfg.OTLPEndpoint))`; `"none"` → no TracerProvider installed at all, leaving the SDK's default no-op). Call `otel.SetTracerProvider` and `otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))` (propagator set unconditionally — extraction should work regardless of exporter choice). Return the provider's `Shutdown` method (or a no-op func for `"none"`).
- [ ] `ExtractContext(ctx, r)`: `otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(r.Header))`.
- [ ] `AgentRunIDFromContext(ctx)`: `baggage.FromContext(ctx).Member("agent_run_id").Value()`.
- [ ] Define the `gen_ai.*`/`kelvran.*` attribute-key constants exactly as listed in the RFC.
- [ ] `ChatCompletionResult` struct + `RecordChatCompletionResult(span, r)`: sets every attribute from `r`'s fields (skipping empty-string/zero fields where the semconv attribute is conditionally-required, e.g. don't set `gen_ai.response.finish_reasons` if `r.FinishReasons` is empty); if `r.Err != nil`, calls `span.RecordError(r.Err)` and `span.SetStatus(codes.Error, r.Err.Error())`.
- [ ] Tests: `Init` with each of the three exporter values constructs without error and returns a working shutdown func (call it, assert no error); `ExtractContext` round-trip (inject a `baggage` header via `propagation.Baggage{}.Inject` into a real `*http.Request`, extract, confirm `AgentRunIDFromContext` returns the injected value); `AgentRunIDFromContext` returns `""` on a context with no baggage at all; `RecordChatCompletionResult` against a `tracetest.NewSpanRecorder()`-backed span — assert the exact attribute set for (a) a full success result and (b) an error result (confirm `RecordError`/`SetStatus` fired, confirm `kelvran.virtual_key.id` is absent when `VirtualKeyID == ""`).

**Verify:** `cd gateway && go build ./internal/telemetry/... && go test ./internal/telemetry/...`

## Task 2 — `internal/gateway/controlplane`: `telemetry:` config section

**Files:**
- Modify: `gateway/internal/gateway/controlplane/config.go`
- Modify: `gateway/internal/gateway/controlplane/config_test.go`, `config_fuzz_test.go`
- Modify: `gateway/config.example.yaml`

**Steps:**
- [ ] Add `TelemetryConfig{Exporter, OTLPEndpoint string}` and `Config.Telemetry TelemetryConfig` (optional — no validation requiring it to be present, since `""` is a meaningful default handled by `telemetry.Init`, not an error).
- [ ] Parse `telemetry:` as a simple nested mapping (`exporter`, `otlp_endpoint` — both scalars, no list support needed, so no `parseYAMLMini` changes required, mirroring Task 4 of the virtual-keys plan's own finding that the existing parser already covers this shape).
- [ ] Add a `telemetry:` block to `config.example.yaml` with a short comment pointing at `docs/operations/TELEMETRY.md` and the RFC.
- [ ] Tests: loading a config with an explicit `telemetry:` block round-trips correctly; loading a config with NO `telemetry:` block at all still succeeds with `Config.Telemetry` zero-valued (proving this section is genuinely optional, not silently required).

**Verify:** `cd gateway && go test ./internal/gateway/controlplane/...`

## Task 3 — Dataplane wiring (depends on Task 1)

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go`
- Modify: `gateway/internal/gateway/dataplane/streaming.go`
- Modify: `gateway/internal/gateway/dataplane/dataplane_test.go`, `streaming_test.go`

**Steps:**
- [ ] `HandleChatCompletion`: start a span (`telemetry.Tracer.Start(ctx, "chat "+req.Model, ...)`) as the very first line, replacing `ctx` with the span-carrying one for the rest of the call. Extend the existing deferred finalize closure to build a `telemetry.ChatCompletionResult` from `vk`/`resp`/`cacheHit`/`cost`/`err` (cost is already computed inside `logRequest` today — hoist that computation one level so both `logRequest` and the telemetry call can use it, rather than computing it twice), call `telemetry.RecordChatCompletionResult`, then `span.End()`, then `logRequest` (unchanged otherwise).
- [ ] Fix the fallback-path gap: reassign `dep = fallbackDep` on the fallback branch (currently read but never reassigned) so `ChatCompletionResult.DeploymentName`/`Provider` reflect whichever deployment actually served the response.
- [ ] `HandleChatCompletionStream`: identical treatment. `FinishReasons`/token counts come from the already-accumulated `resp` (from `streamAccumulator.build` or the cache-hit path's reconstructed response) exactly like the buffered path — no new accumulation logic needed.
- [ ] Tests: wrap each new test in a `tracetest.NewSpanRecorder()` + `otel.SetTracerProvider` (saved/restored via `t.Cleanup`, since this is process-global state and tests in this package run sequentially, never `t.Parallel()`). Assert: a successful buffered request produces one ended span named `"chat gpt-4o"` with the right `gen_ai.*`/`kelvran.*` attributes; an auth failure still produces a span, with `kelvran.virtual_key.id` absent and the span's error recorded; a cache-hit request's span has `kelvran.cache.hit = true`; an `agent_run_id` present on the inbound context (set directly via `baggage.ContextWithBaggage` in the test, bypassing HTTP) appears as `kelvran.agent_run_id` on the span; the streaming path's mirror of at least the success and cache-hit cases.

**Verify:** `cd gateway && go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./...`

## Task 4 — `cmd/gateway` wiring (depends on Tasks 2 and 3)

**Files:**
- Modify: `gateway/cmd/gateway/main.go`
- Modify: `gateway/cmd/gateway/integration_test.go`

**Steps:**
- [ ] `run()`: call `telemetry.Init(ctx, telemetry.Config{Exporter: cfg.Telemetry.Exporter, OTLPEndpoint: cfg.Telemetry.OTLPEndpoint})` before `buildPipeline`; `defer shutdown(context.Background())` (best-effort — see the RFC's Drawbacks on the lack of real graceful shutdown).
- [ ] `chatCompletionsHandler` and `handleStreamingChatCompletion`: replace `r.Context()` with `telemetry.ExtractContext(r.Context(), r)` before calling into the pipeline.
- [ ] New integration test: send a real HTTP request carrying a `baggage: agent_run_id=<value>` header against a server built with a `tracetest.NewSpanRecorder()` installed as the global TracerProvider (installed once for this test, saved/restored) — assert the recorded span's `kelvran.agent_run_id` attribute matches. This is the one integration test that proves the FULL real path (HTTP header → context → dataplane → span), not just the dataplane-unit-level proof from Task 3.

**Verify:** `cd gateway && go build ./... && go vet ./... && go test ./... -v -race && golangci-lint run ./...`

## Task 5 — Docs, Changelog, Wrap-Up

**Files:**
- Modify: `gateway/ARCHITECTURE.md` (mark `/internal/telemetry` ACTIVE in the package layout; note the OTel dependency in the Tech Stack row is now real, not aspirational)
- Modify: `docs/operations/TELEMETRY.md` (mark the exporter/agent_run_id/Baggage sections as real, not designed-only; note `api/otel/`'s protobuf contract remains explicitly deferred — see the RFC's Motivation)
- Modify: `gateway/changelog/unreleased.md` (Added entry)
- Modify: `DECISIONS.md` (one line: first external Go dependency, why; agent_run_id via Baggage; api/ contract deliberately still deferred)
- Modify: `docs/agents/LOGS.md` (new append-only entry)
- Modify: `STATUS.md` (Current Phase, Verification State, Next Action)

**Verify:** re-run Task 4's full verify command once more after doc edits; cross-reference grep for every new doc's referenced paths.
