> **For agentic executors:** Task 1 (`Span` model + `pyproject.toml` dependency) is independent and must land first. Task 2 (`tracing.py`) depends on Task 1's `Span` model existing. Task 3 (`results_store.py` extension) is independent of 2 and can land anytime before Task 4. Task 4 (`run_suite` wiring) depends on 1 and 2. Task 5 (CLI wiring) depends on 1, 2, 3, and 4. Task 6 is docs/changelog/wrap-up.

---

**Goal:** A real `Span` model + JSONL persistence, wrapping every real `run_in_sandbox()` call with a self-contained OTel Python SDK span (no exporter, no collector), closing `THREAT_MODEL.md`'s "full audit logging tied to the trace" gap entirely inside `evals` — not blocked on `api/otel`.

**Architecture:** New `Span`/`SpanStatus` in `evals/evals/models.py`. New `evals/evals/tracing.py`: a locally-held `TracerProvider` + `_CapturingSpanProcessor` + `start_sandbox_span`/`finish_sandbox_span`. `evals/evals/results_store.py` gains `append_spans`/`load_spans`, thin wrappers over the already-generic `_append_models`/`_load_models`. `evals/evals/rollout/scheduler.py`'s `run_suite` gains an opt-in `span_sink: list[Span] | None = None` keyword param. `evals/evals/cli.py`'s `rollout_cmd` gains a required `--traces <path>` option. `evals/pyproject.toml` gains `opentelemetry-sdk`.

**Spec:** `docs/rfcs/2026-09-04-evals-trace-span-model.md` — the exact `Span` field table, the attribute-inclusion table, and every rejected alternative live there; this plan implements them verbatim.

**Global Constraints** (inherited from the spec):
- `Span` only — no `Trace` wrapper in this pass.
- No `gen_ai.*` attributes anywhere on the sandbox span.
- No `container.id`/`process.pid` — not obtainable without a separate `sandbox.py` change, out of scope here.
- `span_sink=None` (the default) must reproduce `run_suite`'s exact current behavior — zero OTel SDK calls, zero behavior change.
- No span is ever created for a cache hit or an early-stop skip — only for a real `run_in_sandbox()` attempt (success or exception).
- Never call `trace.set_tracer_provider()` globally — `tracing.py` holds its own local `TracerProvider`.
- `--traces` is required on `rollout_cmd`, mirroring `--results`/`--scores`'s existing required convention. `run_cmd` is untouched.

---

## Task 1 — `Span` model + dependency

**Files:**
- Modify: `evals/evals/models.py`
- Modify: `evals/pyproject.toml`
- Modify: `evals/tests/test_models.py`

**Steps:**
- [ ] `cd evals && uv add opentelemetry-sdk` (pulls in `opentelemetry-api` transitively) — confirm the resolved version is `>=1.40` (the grounding research verified against 1.44.0; do not pin below what's actually resolved).
- [ ] Add `SpanStatus = Literal["UNSET", "OK", "ERROR"]` alongside `RunStatus`/`ScorerType`.
- [ ] Add `Span(BaseModel)` with `model_config = ConfigDict(frozen=True)`: `span_id: str`, `trace_id: str`, `parent_span_id: str | None = None`, `run_id: str`, `name: str`, `start_time_unix_nano: int`, `end_time_unix_nano: int`, `status: SpanStatus`, `process_command_args: list[str]`, `process_exit_code: int | None = None`, `container_image_name: str`, `error: str | None = None`.
- [ ] Docstring names: why no `Trace` wrapper yet, why no `gen_ai.*`/`container.id`/`process.pid`, pointer to the RFC — not a re-argued justification (mirroring `Run`/`Score`'s own docstring style).
- [ ] Tests in `test_models.py`: construction with all fields; construction with only required fields (defaults apply — `parent_span_id=None`, `process_exit_code=None`, `error=None`); frozen-instance mutation rejection; invalid `status` value rejection.

**Verify:** `cd evals && uv run pytest tests/test_models.py -v`

## Task 2 — `evals/evals/tracing.py`

**Files:**
- Create: `evals/evals/tracing.py`
- Create: `evals/tests/test_tracing.py`

**Steps:**
- [ ] Module docstring: states the no-exporter, locally-held-`TracerProvider` design and why (never `trace.set_tracer_provider()` — see RFC), pointer to the RFC.
- [ ] `_CapturingSpanProcessor(SpanProcessor)`: `__init__` sets `self.last_span: ReadableSpan | None = None`; overrides only `on_end(self, span: ReadableSpan) -> None` to set `self.last_span = span`. Safe only because spans are started/ended strictly sequentially — documented in the class docstring.
- [ ] Module-level singletons: `_provider = TracerProvider()`; `_processor = _CapturingSpanProcessor()`; `_provider.add_span_processor(_processor)`; `_tracer = _provider.get_tracer("kelvran.evals")`.
- [ ] `start_sandbox_span(*, image: str, command: list[str])`: calls `_tracer.start_span("sandbox.exec")`, sets `span.set_attribute("process.command_args", command)` and `span.set_attribute("container.image.name", image)`, returns the live `Span` object (the OTel one, not the pydantic one — name the type collision explicitly in a comment since both this module and `evals.models` define a symbol named `Span`; import `evals.models` as a qualified reference or alias the OTel type on import to avoid ambiguity).
- [ ] `finish_sandbox_span(otel_span, *, run_id: str, image: str, command: list[str], exit_code: int | None, error: str | None) -> models.Span`: if `error is not None`, call `otel_span.record_exception`-equivalent via `otel_span.set_status(Status(StatusCode.ERROR, description=error))`; else `otel_span.set_attribute("process.exit.code", exit_code)` and `otel_span.set_status(Status(StatusCode.OK))`. Call `otel_span.end()`. Read `_processor.last_span` (assert non-`None` — `on_end` fires synchronously inside `.end()`). Build and return the pydantic `Span`: `span_id=format(ctx.span_id, "016x")`, `trace_id=format(ctx.trace_id, "032x")` (from `finished.get_span_context()`), `parent_span_id=None`, `run_id=run_id`, `name=finished.name`, `start_time_unix_nano=finished.start_time`, `end_time_unix_nano=finished.end_time`, `status=finished.status.status_code.name`, `process_command_args=command`, `process_exit_code=exit_code`, `container_image_name=image`, `error=error`.
- [ ] Tests in `test_tracing.py`: a successful `start_sandbox_span`→`finish_sandbox_span(error=None, exit_code=0)` round trip produces a `Span` with `status="OK"`, real non-empty hex `span_id`/`trace_id` (regex-checked length: 16/32 hex chars), `end_time_unix_nano > start_time_unix_nano`; an error round trip (`error="boom"`) produces `status="ERROR"`, `process_exit_code=None`; two sequential spans produce two **different** `span_id`s but (since each call makes an independent root span, not a child) also two different `trace_id`s — assert both are real and distinct, not merely non-empty; confirm `_tracer`/`_provider` are module-level singletons (same object across two calls) via `is` identity, proving no global `trace.set_tracer_provider()` call ever happens (a real check that this module doesn't accidentally pollute `opentelemetry.trace`'s global state — assert `trace.get_tracer_provider()` is still the SDK's default `ProxyTracerProvider`, i.e. unaffected by importing/using this module).

**Verify:** `cd evals && uv run pytest tests/test_tracing.py -v`

## Task 3 — `results_store.py` extension

**Files:**
- Modify: `evals/evals/results_store.py`
- Modify: `evals/tests/test_results_store.py`

**Steps:**
- [ ] Add `append_spans(spans: list[Span], path: Path) -> None` and `load_spans(path: Path) -> list[Span]`, thin wrappers over `_append_models`/`_load_models`, identically shaped to `append_scores`/`load_scores`.
- [ ] `test_results_store.py`: a parallel `Span` round-trip test (`_make_span` fixture mirroring the existing `_make_run`/`_make_score` pattern): append 2 `Span`s, `load_spans` returns them equal; `load_spans` on a nonexistent path returns `[]`; appending twice accumulates.

**Verify:** `cd evals && uv run pytest tests/test_results_store.py -v`

## Task 4 — Wire tracing into `run_suite`

**Files:**
- Modify: `evals/evals/rollout/scheduler.py`
- Modify: `evals/tests/test_scheduler.py`

**Steps:**
- [ ] Import `evals.tracing` and `Span` from `evals.models`.
- [ ] `run_suite` signature gains `span_sink: list[Span] | None = None`, keyword-only, alongside `cached_runs`/`early_stop`.
- [ ] In the per-case loop's real-execution branch (the one making the actual `await run_in_sandbox(...)` call — never the cache-hit branch, never the early-stop-skip branch): hoist `run_id = uuid.uuid4().hex` once, before the `try`, and reuse it as `Run(id=run_id, ...)` at both existing construction sites (success and exception) instead of each calling `uuid.uuid4().hex` independently. Immediately before the `try`, if `span_sink is not None`, call `otel_span = tracing.start_sandbox_span(image=harness_config["image"], command=harness_config["command"])`, else `otel_span = None`.
- [ ] In the exception branch: if `otel_span is not None`, `span_sink.append(tracing.finish_sandbox_span(otel_span, run_id=run_id, image=harness_config["image"], command=harness_config["command"], exit_code=None, error=str(exc)))` — placed after constructing `run` but before `continue`.
- [ ] In the success branch: if `otel_span is not None`, `span_sink.append(tracing.finish_sandbox_span(otel_span, run_id=run_id, image=harness_config["image"], command=harness_config["command"], exit_code=result.exit_code, error=None))` — placed right after constructing `run`.
- [ ] Confirm (by reading, not assuming) the cache-hit branch and the early-stop-skip branch are untouched — neither calls `tracing.start_sandbox_span` at all.
- [ ] Tests in `test_scheduler.py`: `span_sink=None` (omitted) reproduces every existing assertion unmodified (a real regression check, not just "should still pass"); `span_sink=[]` on a real-Docker-gated success case (`RUN_DOCKER_TESTS=1`) produces exactly one `Span` with `run_id` matching the returned `Run.id` and `status="OK"`; a sandbox-launch exception (missing-binary path, already exercised by an existing test) with `span_sink=[]` produces exactly one `Span` with `status="ERROR"` and a non-`None` `error`; a cache hit with `span_sink=[]` appends **zero** spans; an early-stop skip with `span_sink=[]` appends **zero** spans.

**Verify:** `cd evals && uv run pytest tests/test_scheduler.py -v`; separately `RUN_DOCKER_TESTS=1 uv run pytest tests/test_scheduler.py -v` against a real local Docker daemon.

## Task 5 — CLI wiring: `--traces`

**Files:**
- Modify: `evals/evals/cli.py`
- Modify: `evals/tests/test_cli_integration.py`

**Steps:**
- [ ] Import `append_spans` from `evals.results_store`; `Span` from `evals.models`.
- [ ] `rollout_cmd`: add a required `--traces` `click.option` (`click.Path(dir_okay=False, path_type=Path)`, help text mirroring `--results`'s style), placed right after `--results`.
- [ ] In `_run_and_score`/the surrounding body: construct `span_sink: list[Span] = []` before calling `run_suite(...)`, pass `span_sink=span_sink` alongside the existing `cached_runs=`/`early_stop=` kwargs (both the early-stop and non-early-stop call sites), and call `append_spans(span_sink, traces_path)` once after `append_runs(runs, results_path)`.
- [ ] Update every existing `rollout` invocation in `test_cli_integration.py` to add `--traces <tmp_path>` — a mechanical addition, no assertion changes for existing behavior. `run_cmd`'s tests are untouched — `evals run` never gains a `--traces` option.
- [ ] New tests in `test_cli_integration.py`: a real (Docker-gated, `RUN_DOCKER_TESTS=1`) `rollout` invocation persists a `--traces` JSONL file with exactly one `Span` per real (non-cached, non-skipped) `Run`, each `Span.run_id` matching a real persisted `Run.id`; a `--use-cache` re-run's cache-hit cases produce **zero** new spans in the traces file for those cases (only the genuinely-executed ones do).

**Verify:** `cd evals && uv run pytest tests/test_cli_integration.py -v`; separately `RUN_DOCKER_TESTS=1 uv run pytest tests/test_cli_integration.py tests/test_scheduler.py tests/test_sandbox_integration.py -v` against a real local Docker daemon.

## Task 6 — Docs, changelog, wrap-up

**Files:**
- Modify: `evals/ARCHITECTURE.md`
- Modify: `THREAT_MODEL.md`
- Modify: `evals/changelog/unreleased.md`
- Modify: `DECISIONS.md`
- Modify: `docs/agents/LOGS.md`
- Modify: `STATUS.md`

**Steps:**
- [ ] `evals/ARCHITECTURE.md`'s Data Model sketch: mark `Span` as real (v1-narrowed shape — no `Trace` wrapper, no `gen_ai.*`, real `process.*`/`container.*` fields only), pointer to the RFC. `Trace` remains the only unbuilt entity, with an explicit one-line reason (no multi-step harness yet).
- [ ] `evals/ARCHITECTURE.md`'s Tech Stack table's `Tracing` row: currently reads "OTel Python SDK, GenAI semantic conventions, consumed from `api/otel`" — **factually wrong** for what's now real (self-contained sandbox-span capture, zero consumption from `api/otel`, zero `gen_ai.*` attributes). Correct it to describe the real shape.
- [ ] `evals/ARCHITECTURE.md`'s Package Layout: add a one-line entry for the new `tracing.py`.
- [ ] `THREAT_MODEL.md`'s Evals Elevation-of-Privilege row: currently says "full audit logging tied to the trace needs the `Trace`/`Span` model, itself blocked on `api/otel`'s still-undecided transport." Correct to state `Span`-level audit logging for real sandbox executions is now real and self-contained — `api/otel`'s transport question was never actually a blocker for this specific gap, a doc-vs-code framing correction, not a status change on the still-real prerequisites for the *other* three EoP items in that row (cross-sandbox isolation, package-registry-proxy hardening, scoped per-tool credentials — all unchanged, still blocked on their own separate prerequisites).
- [ ] `evals/changelog/unreleased.md`: new `## Added` entry (the `Span` model, `tracing.py`, the required `--traces` flag, the new `opentelemetry-sdk` dependency), at the same detail level as the prior evals entries this session.
- [ ] `DECISIONS.md`: one new line at the true chronological end (re-check `tail` immediately before appending) naming the self-contained-vs-`api/otel` scope call, the `gen_ai.*`-rejection finding, and the locally-held-`TracerProvider` choice.
- [ ] `docs/agents/LOGS.md`: new entry (Files touched / Intent-summary / Decisions made / Verification performed / Bugs found / Next steps).
- [ ] `STATUS.md`: update Current Phase / Last Completed Task / Next Action / Verification State.
- [ ] Full `make verify` from repo root — must pass clean before commit (`gateway` untouched by this pass).

**Verify:** `make verify` (root) passes end-to-end; `git diff` reviewed in full before committing.

## Scope Gate

This is architecturally scoped (a new data model + a new module + a new runtime dependency + a breaking CLI change), correctly warranting a plan file, not a one-line `DECISIONS.md` entry.
