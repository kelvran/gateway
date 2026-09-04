"""Self-contained OTel Python SDK span capture for real sandbox executions.

No OTLP exporter is wired -- see docs/rfcs/2026-09-04-evals-trace-span-
model.md for why: the OTel SDK is used purely as a real, spec-compliant
trace_id/span_id + span-lifecycle generator (for possible future
correlation with gateway's own real OTel spans, per api/otel's still-
undecided transport), never as a live export pipeline.

A locally-held `TracerProvider` is used rather than the process-wide
`trace.set_tracer_provider()` singleton -- the latter is `Once()`-guarded
and process-global (confirmed via source inspection of the installed SDK):
the first caller wins, every later call silently no-ops. That is wrong for
a library invoked repeatedly across CLI processes and, especially, across
an in-process pytest suite. `get_tracer()` is called directly on this
module's own `TracerProvider` instance instead.

Attribute names are hardcoded string literals rather than imported from
`opentelemetry.semantic_conventions` (a 0.65b0 beta package) -- confirmed
via inspection that `process.command_args`/`process.exit.code` are real
but "_incubating"-only attributes (no stable `process` module exists in
this SDK version at all), while `container.image.name` is stable.
`gen_ai.operation.name` is deliberately never used here -- confirmed, via
the OTel semantic-conventions registry, to be a namespace for LLM/model-
inference operations only, not a container-sandbox execution.
"""

from __future__ import annotations

from opentelemetry.sdk.trace import ReadableSpan, SpanProcessor, TracerProvider
from opentelemetry.sdk.trace import Span as OTelSpan
from opentelemetry.trace import Status, StatusCode

from evals.models import Span


class _CapturingSpanProcessor(SpanProcessor):
    """Captures the most recently ended span.

    Safe only because `evals`' Rollout Scheduler starts and ends spans
    strictly sequentially -- never concurrently -- matching the scheduler's
    own existing single-worker design (see evals/ARCHITECTURE.md's Tech
    Stack: "asyncio at v1... concurrency is a later upgrade").
    """

    def __init__(self) -> None:
        self.last_span: ReadableSpan | None = None

    def on_end(self, span: ReadableSpan) -> None:
        self.last_span = span


_provider = TracerProvider()
_processor = _CapturingSpanProcessor()
_provider.add_span_processor(_processor)
_tracer = _provider.get_tracer("kelvran.evals")


def start_sandbox_span(*, image: str, command: list[str]) -> OTelSpan:
    """Start one real OTel span for a single `run_in_sandbox()` attempt.

    Returns the live OTel `Span` (not `evals.models.Span`) -- the caller
    must eventually pass it to `finish_sandbox_span` to close it out and
    get the persisted pydantic model back.
    """
    span = _tracer.start_span("sandbox.exec")
    span.set_attribute("process.command_args", command)
    span.set_attribute("container.image.name", image)
    return span


def finish_sandbox_span(
    otel_span: OTelSpan,
    *,
    run_id: str,
    image: str,
    command: list[str],
    exit_code: int | None,
    container_id: str | None,
    error: str | None,
) -> Span:
    """End `otel_span` and return the finished `evals.models.Span`.

    Exactly one of `exit_code`/`error` is meaningful: `error` set means
    `run_in_sandbox()` itself raised (nothing ever ran, no exit code
    exists); otherwise `exit_code` is the real result. `container_id` is
    `None` only if the container never reached the point of being
    created -- never a fabricated placeholder.
    """
    if error is not None:
        otel_span.set_status(Status(StatusCode.ERROR, description=error))
    else:
        otel_span.set_attribute("process.exit.code", exit_code)
        otel_span.set_status(Status(StatusCode.OK))
    if container_id is not None:
        otel_span.set_attribute("container.id", container_id)
    otel_span.end()

    finished = _processor.last_span
    assert finished is not None, "on_end fires synchronously inside span.end()"
    ctx = finished.get_span_context()
    assert ctx is not None

    return Span(
        span_id=format(ctx.span_id, "016x"),
        trace_id=format(ctx.trace_id, "032x"),
        parent_span_id=None,
        run_id=run_id,
        name=finished.name,
        start_time_unix_nano=finished.start_time,
        end_time_unix_nano=finished.end_time,
        status=finished.status.status_code.name,
        process_command_args=command,
        process_exit_code=exit_code,
        container_image_name=image,
        container_id=container_id,
        error=error,
    )
