import re

from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider as SDKTracerProvider

from evals import tracing

_HEX16 = re.compile(r"^[0-9a-f]{16}$")
_HEX32 = re.compile(r"^[0-9a-f]{32}$")


def test_success_round_trip_produces_ok_status_with_real_ids():
    span = tracing.start_sandbox_span(image="alpine:3.20", command=["echo", "hi"])
    result = tracing.finish_sandbox_span(
        span,
        run_id="run-001",
        image="alpine:3.20",
        command=["echo", "hi"],
        exit_code=0,
        error=None,
    )
    assert result.status == "OK"
    assert result.process_exit_code == 0
    assert result.error is None
    assert _HEX16.match(result.span_id)
    assert _HEX32.match(result.trace_id)
    assert result.end_time_unix_nano > result.start_time_unix_nano


def test_error_round_trip_produces_error_status_with_no_exit_code():
    span = tracing.start_sandbox_span(image="alpine:3.20", command=["false"])
    result = tracing.finish_sandbox_span(
        span,
        run_id="run-002",
        image="alpine:3.20",
        command=["false"],
        exit_code=None,
        error="docker binary not found",
    )
    assert result.status == "ERROR"
    assert result.process_exit_code is None
    assert result.error == "docker binary not found"


def test_two_sequential_spans_get_distinct_real_ids():
    span_a = tracing.start_sandbox_span(image="alpine:3.20", command=["echo", "a"])
    result_a = tracing.finish_sandbox_span(
        span_a,
        run_id="run-a",
        image="alpine:3.20",
        command=["echo", "a"],
        exit_code=0,
        error=None,
    )
    span_b = tracing.start_sandbox_span(image="alpine:3.20", command=["echo", "b"])
    result_b = tracing.finish_sandbox_span(
        span_b,
        run_id="run-b",
        image="alpine:3.20",
        command=["echo", "b"],
        exit_code=0,
        error=None,
    )
    assert result_a.span_id != result_b.span_id
    assert result_a.trace_id != result_b.trace_id


def test_module_holds_its_own_tracer_provider_never_the_global_singleton():
    # Importing/using evals.tracing must never call trace.set_tracer_provider()
    # -- the global provider stays the SDK's default no-op ProxyTracerProvider.
    assert not isinstance(trace.get_tracer_provider(), SDKTracerProvider)


def test_tracer_is_a_module_level_singleton():
    span_a = tracing.start_sandbox_span(image="alpine:3.20", command=["echo", "a"])
    tracing.finish_sandbox_span(
        span_a,
        run_id="run-a",
        image="alpine:3.20",
        command=["echo", "a"],
        exit_code=0,
        error=None,
    )
    processor_after_first = tracing._processor
    span_b = tracing.start_sandbox_span(image="alpine:3.20", command=["echo", "b"])
    tracing.finish_sandbox_span(
        span_b,
        run_id="run-b",
        image="alpine:3.20",
        command=["echo", "b"],
        exit_code=0,
        error=None,
    )
    assert tracing._processor is processor_after_first
