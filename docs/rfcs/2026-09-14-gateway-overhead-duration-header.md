# RFC: `X-Kelvran-Overhead-Duration-Ms` response header

- **Status**: accepted
- **Date**: 2026-09-14
- **Author(s)**: gateway maintainers (via `docs/upgrade-research/load-testing-capacity-planning-2026-09-14.md` Finding 4, extending `docs/upgrade-research/gateway-load-testing-methodology-2026-09-12.md`'s own citation of LiteLLM's `x-litellm-overhead-duration-ms`)

## Summary

Add a response header on the buffered (non-streaming) `/v1/chat/completions` path reporting how much of the request's total wall-clock time was Kelvran's own added latency, isolated from the real upstream provider round-trip — mirroring LiteLLM's own shipped `x-litellm-overhead-duration-ms` header.

## Motivation

Load-testing research (both the 2026-09-12 and 2026-09-14 passes) confirmed this is a real, standing gap: without isolating gateway-added latency from total end-to-end latency, a load test's own numbers conflate Kelvran's real overhead with the (mocked, in a real load test) upstream provider's own latency, making before/after comparisons across a Kelvran change meaningless. LiteLLM ships exactly this signal today; Kelvran had no equivalent.

## Detailed Design

`dataplane.WithOverheadTracker(ctx) (context.Context, *time.Duration)` returns a context carrying a pointer `runMissPath` writes the cumulative real-upstream-call time into internally (wrapping the deployment-selection-through-fallback-resolution block — the same span `attemptFallbackChain`'s own multi-hop retries live inside, so a multi-hop fallback's added latency is correctly counted as upstream time, not gateway overhead). A cache hit never calls `runMissPath` at all, so the pointer stays at its zero value — correctly reporting 100% of that request's latency as gateway overhead, since no real upstream call happened.

`cmd/gateway/main.go`'s `chatCompletionsHandler` creates the tracker before calling `HandleChatCompletion`, and after it returns, computes `overhead := totalDuration - *upstreamDuration` and sets `X-Kelvran-Overhead-Duration-Ms` before encoding the response body — mirroring the existing `start := time.Now()` total-duration measurement already used for `finalize`'s own `duration` parameter, not a second, independent timer.

Deliberately a context-value pointer, not a widened `HandleChatCompletion` return signature: the latter would touch all ~26 existing call sites (mostly tests) for one optional, additive header, versus zero signature changes with this approach — the same tradeoff `telemetry.AgentRunIDFromContext`'s own Baggage-based mechanism already made for the opposite (input) direction.

**Explicitly out of scope: the streaming path.** `handleStreamingChatCompletion` sets response headers *before* the pipeline runs (`Content-Type`/`Cache-Control`/`Connection`, required to be set before the first SSE byte) — there is no point after which a post-hoc-computed header could still be added to that same response. A real fix would need either an SSE-trailer-shaped mechanism or a fundamentally different measurement point (e.g. time-to-first-chunk instead of total-request overhead), neither of which this RFC attempts; disclosed as a real, known gap rather than silently applying the header inconsistently.

## Drawbacks

Buffered-path only — an operator load-testing the streaming path gets no equivalent signal from this header. A context-value pointer is a slightly unusual pattern versus an explicit return value, though it directly mirrors an existing precedent in this codebase (`AgentRunIDFromContext`).

## Alternatives Considered

Widening `HandleChatCompletion`'s return signature: rejected for the call-site churn named above. Attaching the value to `adapter.ChatResponse` itself: rejected — that type is Kelvran's own client-facing wire format; leaking an internal performance metric into it would mean every client SDK's response model needs to tolerate an unexpected extra field.

## Unresolved Questions

Whether a future streaming-specific overhead signal (e.g. time-to-first-chunk minus time-to-first-upstream-byte) is worth building — deferred until a real caller asks for streaming-path overhead visibility specifically.
