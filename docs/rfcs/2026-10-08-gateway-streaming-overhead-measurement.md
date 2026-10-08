# RFC: measuring the gateway's own overhead on streamed responses

**Status**: proposed, 2026-10-08. Decision gate G20 of the discoverability plan: the owner picks among the candidates below (or all three). Nothing in this RFC is implemented; the benchmark harness (`gateway/cmd/kelvran-bench`, `docs/operations/BENCHMARKS.md`) measures streams end to end today and this RFC is about attributing the gateway's share.

## Problem

`X-Kelvran-Overhead-Duration-Ms` tells a client how much of a **buffered** response's wall-clock the gateway itself spent: `cmd/gateway/main.go` sets it after the upstream call returns, as total handler time minus the time spent inside the upstream call (`dataplane.WithOverheadTracker`). A **streamed** response cannot carry that number: its headers are committed when the first chunk is flushed, long before the stream ends and the upstream time is known. So the harness can measure, for a stream, only the end-to-end time to first token (TTFT) and the inter-chunk gaps; it cannot say how many of those milliseconds were the gateway's. The recorded S2 numbers are therefore "TTFT and inter-chunk gaps as a client sees them, against a mock whose own TTFT and cadence are known", and the gateway's share is bounded above by the difference (the harness's own client-side cost and the mock's scheduling jitter sit inside it; `BENCH_BASELINE=1` in `scripts/bench-ci.sh` measures that floor by running the harness straight at the mock), which is sound only while the mock is deterministic.

## Candidates

**A. The existing header on streaming responses, meaning "gateway time before the first upstream byte".** At the moment the first chunk is flushed the gateway knows the time from request start to sending the upstream request (auth, limits, budget, cache lookup, guardrails, routing) and the time between the upstream's first byte and the first flush (decode, stamp, write). Both are gateway time; the gap in between is the provider's TTFT. Set `X-Kelvran-Overhead-Duration-Ms` to their sum on streamed responses. Pro: no new wire element, the same header a client already reads; a buffered and a streamed request then both carry "how much of this was Kelvran". Con: the meaning differs by path (whole-request share vs pre-first-token share), which the header's documentation must state; the post-first-token gateway share (per-chunk decode, stamp, write, guardrail post-call, cache write) goes unreported.

**B. An SSE comment before the sentinel**: `: kelvran-overhead-ms=<n>` written immediately before `data: [DONE]`, with the whole-stream gateway share (pre-call work plus the per-chunk work summed). The SSE specification requires clients to ignore comment lines; whether every OpenAI-compatible client in the wild actually does has not been verified. Pro: the only option that reports the whole-stream share; invisible to clients that do not look for it. Con: a non-standard channel a client has to be told about; the measurement needs per-chunk timing inside the stream loop (cheap, two monotonic reads per chunk); a stream that fails mid-way gets no comment (the in-band error frame replaces `[DONE]`).

**C. A span attribute on both paths**: `kelvran.overhead_ms` on the request span, the same value the header carries for buffered responses and the whole-stream share for streamed ones. Pro: no wire change at all, operator-side, works for every client, lands in the trace next to `gen_ai.*`; the harness can read it from a stdout-exporter run or an OTLP collector. Con: not visible to a client; needs the collector path to be part of the benchmark setup.

## Recommendation

C first (it costs one attribute and is invisible to clients), then A with the header's documentation extended to say what it means on each path. B only if a client asks for the whole-stream share on the wire. Cache replays (`writeFakeStream`) report their whole duration as overhead on every variant, since no upstream is involved.

## Measurement definitions the harness already uses

- **TTFT**: request start to the first `data:` frame whose delta carries content; the mock's configured latency is the provider's share.
- **Inter-chunk gap**: time between consecutive content frames; the mock's configured chunk interval is the provider's share.
- **Overhead (buffered)**: `X-Kelvran-Overhead-Duration-Ms` as sent.

## Open questions for the decision

1. Is a header whose meaning differs between buffered and streamed responses acceptable, or must the streamed meaning get a different header name?
2. Is an SSE comment acceptable on a wire the project advertises as OpenAI-compatible?
3. Should `kelvran.overhead_ms` also count the post-response settlement (budget and TPM reconcile), which happens after the client has its bytes?
