- **Status**: accepted
- **Date**: 2026-09-04
- **Author(s)**: project founder + Claude Code

## Summary

Build a `Span` model (deliberately not the full `Trace{spans:[Span]}` the architecture sketch describes) and JSONL persistence for `evals/`, wrapping every real `run_in_sandbox()` call in the Rollout Scheduler with a real OpenTelemetry Python SDK span — self-contained, no OTLP exporter, no collector, no dependency on `api/otel`'s still-undecided transport. This resolves the actual cited blocker (`THREAT_MODEL.md`'s Evals Elevation-of-Privilege row: "full audit logging tied to the trace needs the `Trace`/`Span` model, itself blocked on `api/otel`'s still-undecided transport") entirely inside `evals`, per the approved backlog plan's own explicit Phase 3a design (`/Users/sairamugge/.claude/plans/snuggly-exploring-finch.md`): reject both a new `api/otel` proto contract and live gateway-OTLP consumption as premature — there is no real consumer of either yet — and instead give every real sandbox execution a durable, spec-compliant audit record on its own.

## Motivation

Confirmed directly against the live tree, not assumed: `evals/ARCHITECTURE.md`'s Data Model sketch names `Trace{trace_id, run_id, spans:[Span]}` / `Span{span_id, parent_span_id, gen_ai.operation.name, input, output, start_time, end_time, attributes}` as the last two unbuilt entities — `EvalCase`, `Run`, and `Score` are all real. `evals/evals/models.py`'s own `Run` docstring names why it was never built: "`api/otel`'s transport is still undecided, so a stub field here would be dead code with no consumer." `api/README.md` confirms `otel/` is "still a placeholder, deliberately" — a custom protobuf schema there would duplicate a wire format gateway's own OTLP export already provides, and `evals` has no live-consumption mechanism for it (`api/README.md`: "If `evals` ever needs to consume gateway's spans, the mechanism is a real OTel Collector fan-out, not this directory").

That framing is correct for *gateway's* spans — but conflates two unrelated capabilities under one "blocked on `api/otel`" umbrella. `THREAT_MODEL.md`'s Elevation-of-Privilege row is about *evals' own* sandbox executions having no audit trail, not about consuming gateway's telemetry. Nothing about that gap requires `api/otel`'s transport question to be resolved first: `evals`' Rollout Scheduler already makes exactly one `run_in_sandbox()` call per `Run`, entirely within one Python process, with zero cross-service transport involved at all. Grounded via a 3-angle dynamic-workflow research pass (task `w4vjp2i9u`, run `wf_439be6c2-d5b`) plus independent re-verification against the installed `opentelemetry-sdk`/`api` 1.44.0 source: the OTel Python SDK's `TracerProvider`/`Tracer`/`SpanProcessor` API works correctly with zero exporters wired, producing real, spec-compliant 128-bit trace IDs and 64-bit span IDs via `RandomIdGenerator` regardless. The one concrete reason to use the real SDK rather than hand-rolled UUIDs, as this codebase's own approved plan already named: real trace_id/span_id format compatibility for a possible future correlation with gateway's genuine OTel spans, without a migration.

The research also caught a real design mistake already sitting in `ARCHITECTURE.md`'s own sketch: `gen_ai.operation.name` is confirmed, via direct inspection of the OTel semantic-conventions registry (`GenAiOperationNameValues`: `chat`, `generate_content`, `text_completion`, `embeddings`, `retrieval`, `create_agent`, `invoke_agent`, `execute_tool`, `invoke_workflow`), to be a namespace for LLM/model-inference operations — none of which describe "ran an arbitrary command inside a Docker container." Applying it to a sandbox-execution span would be a semantic misuse, corrected in this design rather than carried forward silently.

## Detailed Design

### Scope: `Span` only, no `Trace` wrapper — same discipline already applied to `Run`/`Score`

Today's harness makes exactly one `run_in_sandbox()` call per `Run` (per `evals/ARCHITECTURE.md`'s own "sequential, no `Sandbox Pool` yet" scope boundary) — there is no multi-step, multi-span execution to group. Building `Trace{spans:[Span]}` now would be the exact "diagram-only box" pattern `ARCHITECTURE.md` already refuses to build ahead of real need (`Task/Dataset Registry`, `Sandbox Pool`, `Online Eval Service` all stay unbuilt for the same reason). `Span.run_id` joins directly to `Run.id` — the same join-key discipline `Score.eval_case_id`/`Score.run_id` already established. A future pluggable multi-step harness (the same trigger `Run.harness_config`'s docstring already names for its own currently-narrower shape) is the real trigger for introducing `Trace` later.

### SDK usage: a locally-held `TracerProvider`, never the global singleton

`trace.set_tracer_provider()` is backed by a process-wide `Once()` guard — confirmed directly in the installed 1.44.0 source (`trace/__init__.py`): the first call wins, every later call silently no-ops with a logged warning. That is the wrong primitive for a library invoked repeatedly across CLI processes and, especially, across an in-process test suite, where a second test setting a second provider would silently be ignored. Instead, `evals/evals/tracing.py` holds one module-level `TracerProvider()` instance and calls `provider.get_tracer("kelvran.evals")` directly — never touching the global `trace` module at all. No `SpanExporter` is registered; a small `_CapturingSpanProcessor(SpanProcessor)` overrides only `on_end(self, span: ReadableSpan) -> None`, confirmed via source inspection to be a plain, non-abstract hook — the documented, stable "give me the finished span" mechanism (`ReadableSpan`'s own docstring: instances are "created as a direct result of using the tracing pipeline via the Tracer," never constructed directly). Safe only because the Rollout Scheduler starts and ends spans strictly sequentially — never concurrently, matching the scheduler's own existing single-worker design.

### Attribute set: real semantic-convention names where they genuinely apply, plain custom attributes where they don't

Confirmed via direct inspection of the installed `opentelemetry-semantic-conventions` 0.65b0 package: `process.command_args`/`process.pid` and `process.exit.code` exist only in `_incubating` (no stable `process` module at all in this SDK version); `container.id`/`container.image.name` are stable, `container.command`/`container.runtime.name` remain incubating-only. Rather than depend on a beta-versioned package for a handful of string constants, this RFC hardcodes the attribute-key literals directly in `tracing.py` with a comment noting their real (if experimental) semantic-convention provenance — avoiding a beta-pinned dependency for something that's just string constants.

Only fields genuinely obtainable from today's `run_in_sandbox()`/`docker run --rm --network=none <image> <command>` invocation (`evals/evals/rollout/sandbox.py`) are populated — no fabricated placeholders, the same convention `Run.cost_usd: None` already established:

| Attribute | Source | Included? |
|---|---|---|
| `process.command_args` | `harness_config["command"]` | Yes — already known before the call |
| `container.image.name` | `harness_config["image"]` | Yes — already known before the call |
| `process.exit.code` | `SandboxResult.exit_code` | Yes, on success; absent on the sandbox-launch-exception path (nothing ever ran) |
| `container.id` | would require `docker run`'s real container ID | **No** — `sandbox.py`'s `--rm` invocation never captures it; adding that is a real, separate scope change to `sandbox.py` itself, not bundled here |
| `process.pid` | would require `sandbox.py` to surface `asyncio.subprocess.Process.pid` | **No** — not currently threaded through `SandboxResult`; a real, separate future addition, not a speculative field added now |
| `gen_ai.operation.name` | — | **No** — confirmed inapplicable per Motivation above; never used |

### `Span` model (`evals/evals/models.py`, new, alongside `EvalCase`/`Run`/`Score`)

```python
SpanStatus = Literal["UNSET", "OK", "ERROR"]


class Span(BaseModel):
    model_config = ConfigDict(frozen=True)

    span_id: str
    trace_id: str
    parent_span_id: str | None = None
    run_id: str
    name: str
    start_time_unix_nano: int
    end_time_unix_nano: int
    status: SpanStatus
    process_command_args: list[str]
    process_exit_code: int | None = None
    container_image_name: str
    error: str | None = None
```

`span_id`/`trace_id` are the real, lowercase-hex-formatted IDs read off the OTel SDK's own `SpanContext` (`format(ctx.span_id, "016x")` / `format(ctx.trace_id, "032x")`) — genuine 64-bit/128-bit values, not placeholders. `parent_span_id` stays `None` always in v1 (no nested spans yet — named explicitly, not silently omitted, mirroring `rubric_axis`'s precedent on `Score`). `status` is `"ERROR"` whenever the wrapped `run_in_sandbox()` call raised (mirrors `span.record_exception`/`set_status(ERROR)`, called before `span.end()`); `"OK"` otherwise — never `"UNSET"` in practice for a span this code always explicitly closes out, but kept in the `Literal` since the SDK's own default status is `UNSET` and a future caller could reasonably encounter it.

### `evals/evals/tracing.py` (new)

A small, self-contained module: the module-level `TracerProvider`/`_CapturingSpanProcessor`/`Tracer` singleton described above, plus two plain functions — `start_sandbox_span(*, image, command) -> Span` (the live OTel `Span` object, attributes set immediately) and `finish_sandbox_span(span, *, run_id, image, command, exit_code, error) -> Span` (sets the final attribute/status, calls `span.end()`, reads the now-finished `ReadableSpan` back out of `_CapturingSpanProcessor.last_span`, and returns the pydantic `Span` model). No async, no I/O — pure in-memory span bookkeeping.

### Wiring into `run_suite` (`evals/evals/rollout/scheduler.py`)

`run_suite` gains one new keyword-only parameter, mirroring `cached_runs`/`early_stop`'s existing opt-in-additive convention exactly: `span_sink: list[Span] | None = None`. When `None` (the default), behavior is byte-for-byte identical to before this RFC — zero OTel SDK calls happen at all. When provided, exactly one `Span` is appended to it for every real `run_in_sandbox()` attempt (both the success path and the sandbox-launch-exception path) — **never** for a cache hit or an early-stop skip, since neither represents a real execution to trace, the same "no signal, don't fabricate one" rule `Run.status="skipped"`'s docstring already states. The existing try/except control flow in `run_suite` is otherwise unchanged — `start_sandbox_span`/`finish_sandbox_span` calls are inserted immediately around the existing `await run_in_sandbox(...)` call and its two outcome branches, not a restructuring.

### CLI wiring (`evals/evals/cli.py`)

`rollout_cmd` gains a new, **required** `--traces <path>` option, placed identically to the existing required `--results`/`--scores` convention (a plain, caller-chosen append target, no default, no rotation policy) — the same deliberate breaking-CLI-change precedent `--scores` already set when the `Score` model shipped. A `span_sink: list[Span] = []` is constructed before calling `run_suite(..., span_sink=span_sink)`, and `append_spans(span_sink, traces_path)` is called once after, mirroring `append_runs`/`append_scores`'s existing "batch write once" call site exactly. `evals run` is unaffected — it never touches the Rollout Scheduler, so there is no real sandbox execution for it to trace, the same reasoning that already keeps `run_cmd` out of `Run`'s scope entirely.

### Persistence: extend the existing generic `results_store.py`, don't create a new module

`append_spans(spans: list[Span], path: Path) -> None` / `load_spans(path: Path) -> list[Span]` are added as thin wrappers over the already-generic `_append_models`/`_load_models`, identically to how `append_scores`/`load_scores` were added in the prior RFC. No new persistence mechanism.

## Drawbacks

- **New dependency**: `opentelemetry-sdk`/`opentelemetry-api` are added to `evals/pyproject.toml` — the first non-`anthropic`/`click`/`pydantic`/`protobuf` runtime dependency this project has taken on. Accepted because the alternative (hand-rolled 128-bit/64-bit ID generation) would reinvent a small but real piece of spec-compliant logic for no real benefit, and the SDK's zero-exporter mode has a genuinely small footprint (confirmed directly: `TracerProvider()` with no processors/exporters registered works standalone).
- **Breaking CLI change**: `--traces` is required on `rollout_cmd`, mirroring `--results`/`--scores`'s existing required convention — every existing invocation must add it. `evals run` is unaffected.
- `container.id`/`process.pid` are real, applicable semantic-convention attributes this RFC does **not** populate, since neither is obtainable without a separate change to `sandbox.py`'s `docker run --rm` invocation — named explicitly as a real, deliberate gap for a future pass, not a silent omission.
- A locally-held `TracerProvider` (never registered as the process-wide default) means any *other* future code in this repo that calls `trace.get_tracer_provider()` globally will never see these spans — a deliberate isolation choice (see Detailed Design), but worth naming as a real constraint on any future in-process consumer.

## Alternatives Considered

1. **Hand-rolled UUID-based IDs instead of the OTel SDK** — rejected; the approved backlog plan explicitly commits to "the OTel Python SDK," and using the real SDK costs little (confirmed via grounding) while preserving genuine future format-compatibility with gateway's real spans.
2. **Call `trace.set_tracer_provider()` globally, as most OTel tutorials show** — rejected; confirmed via source inspection that the global singleton's `Once()` guard is wrong for a library (not an application entrypoint) invoked repeatedly across CLI processes and, especially, pytest's in-process test suite.
3. **Wire a real (even if local/no-op) `SpanExporter`, e.g. `ConsoleSpanExporter` or an in-memory exporter, instead of a custom `SpanProcessor`** — rejected; confirmed the documented, minimal mechanism for "read a finished span's fields" is `SpanProcessor.on_end(span: ReadableSpan)`, with no exporter required at all. Adding one now would be speculative infrastructure with no real consumer.
4. **Build the full `Trace{spans:[Span]}` shape now** — rejected; today's harness makes exactly one span per `Run`, so a `Trace` wrapper would be an empty, unused abstraction — the same "don't build the diagram-only box" discipline already applied to `Task/Dataset Registry`/`Sandbox Pool`.
5. **Use `gen_ai.*` semantic-convention attributes on the sandbox span, matching the pre-existing (incorrect) `ARCHITECTURE.md` sketch** — rejected, decisively, per the Motivation section's grounding finding: `gen_ai.operation.name`'s real enum values are all LLM/model-inference operations, none of which describe a container-sandbox execution.
6. **Depend on the `opentelemetry-semantic-conventions` package for attribute-key string constants** — rejected for this pass; it is a `0.65b0` beta package, and the handful of string literals needed don't justify a beta-pinned dependency. Hardcoded with a sourcing comment instead.

## Unresolved Questions

- `container.id`/`process.pid` remain real, named gaps (see Drawbacks) — closing them requires a separate, scoped change to `sandbox.py`'s `docker run` invocation, not bundled here.
- Whether spans should ever be exported to a real OTLP collector (rather than only persisted as JSONL) is explicitly not decided here — this RFC's `TracerProvider` has zero exporters wired, by design, and stays that way until a real consumer exists.
- `Trace{spans:[Span]}`'s eventual shape once a multi-step harness exists is left to that future RFC.
- A query/report command over persisted `Span`s (mirroring `evals report --scores`) — no consumer needs this yet; left as a real, deliberate gap.

## Research Trail

Grounded via a 3-angle dynamic-workflow research pass (task `w4vjp2i9u`, run `wf_439be6c2-d5b`): an SDK-capture-pattern angle (correct OTel Python SDK API shape for zero-exporter span creation/capture, confirmed against installed 1.44.0 source), a semantic-conventions-fit angle (whether `gen_ai.*` applies to a sandbox-execution span — it does not — and which real `process.*`/`container.*` attributes do), and a framework-precedent angle (Inspect AI, promptfoo, DeepEval, Braintrust — none use OTel as a purely local ID/timing library; Inspect AI uses its own `SpanBeginEvent`/`SpanEndEvent`, promptfoo runs a genuine OTLP receiver for live ingestion). The synthesis independently re-installed and inspected the real `opentelemetry-sdk`/`api` 1.44.0 and `opentelemetry-semantic-conventions` 0.65b0 source directly (not just trusting the three research angles), confirming: `RandomIdGenerator` produces genuine 128-bit/64-bit IDs with zero exporters registered; `SpanProcessor.on_end` is the documented, non-abstract capture hook; `set_tracer_provider`'s `Once()` guard is real and process-wide; `GenAiOperationNameValues`' 9 real enum values are all LLM-operation-shaped; `process.*` attributes are entirely `_incubating` in this SDK version (no stable `process` module exists at all) while `container.id`/`container.image.name` are stable.
