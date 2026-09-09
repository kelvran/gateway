# RFC: Post-call Guardrails must scan tool-call arguments, not just Content

## Status

Accepted, implemented 2026-09-09.

## Context

A `docs/upgrade-research/gateway-next-upgrade-2026-09-09.md` deep-research pass, cross-referencing 2026 production gateway practice against Kelvran's own real code, found a genuine asymmetry in already-shipped Guardrails coverage rather than a new-feature gap: `serializeMessages` (used by the pre-call check, `dataplane.go`'s `runMissPath`) does a full `json.Marshal` of `[]adapter.Message`, which covers `adapter.ToolCall.ArgumentsJSON` via that type's own custom `MarshalJSON`. `serializeResponse` (used by the post-call check — both the buffered path and the streaming audit-only path, `finishStreamedResponse`, which reuses the exact same function) only ever read `Message.Content` per choice, never `Message.ToolCalls[].ArgumentsJSON`.

A model that emits PII, a leaked secret, or attacker-exfiltrated data inside a tool call's arguments — for example, a hijacked `send_email(body=...)` call following a successful prompt injection — produces a response whose `Message.Content` is typically empty. Guardrails' post-call regex/checksum detectors (credit card, SSN, IBAN, secret-key, email, prompt-injection) never see that payload at all under the pre-fix behavior. This lands within OWASP LLM06:2025 Excessive Agency's framing, though Kelvran's own exposure is narrower than that framing's general tool-execution guidance implies: Kelvran's adapter layer is a pure wire-format pass-through for tool calls, it never executes one itself. Kelvran's own responsibility is the narrower, concrete gap this RFC closes: scan everything the post-call path lets through, symmetrically with what the pre-call path already covers.

## Design

`serializeResponse` (`gateway/internal/gateway/dataplane/dataplane.go`) is extended to append each choice's `ToolCalls[].ArgumentsJSON` alongside `Content`, in the same newline-joined, non-JSON-re-encoded scanning style the function already used. No signature change; no new detectors. Since both the buffered post-call check and the streaming audit-only post-call check call this exact function, the fix closes the gap on both paths in one change — the streaming path's own separate, intentional "audit-only, never withheld" limitation (content is already flushed to the client before a complete response exists to check) is unrelated and untouched by this fix.

## Alternatives considered

**Adding a dedicated, structured JSON re-encoding of the response for the post-call check, matching the request-side's full `json.Marshal`** — rejected as unnecessary: the guardrail detectors are text-pattern/checksum scanners, not JSON-structure-aware, so a plain string concatenation of every scannable field (as the pre-existing design already does for `Content`) is sufficient and keeps `serializeResponse`'s existing "text, not structure" scope intact.

## Verification

`gateway/internal/gateway/dataplane/guardrail_test.go`: new `fakeOpenAIResponseWithToolCallArguments` helper builds a response with empty `Content` and the trigger buried in a tool call's `Function.Arguments`. `TestHandleChatCompletionPostCallScansToolCallArguments` proves the buffered path now blocks (`ErrGuardrailBlocked`, upstream called exactly once — post-call, not pre-call). New `sseStreamWithToolCallArguments` builds the streaming-path equivalent (a tool-call delta with no content delta at all); `TestHandleChatCompletionStreamPostCallAuditLogsToolCallArguments` builds a `Pipeline` directly with a buffer-backed logger (the only way to observe an audit-only verdict, which never changes response behavior) and proves the `guardrail_blocked_postcall_streaming_audit_only` log line now fires with a nonzero `finding_count` for a trigger hidden only in tool-call arguments. Both new tests pass; full `go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./... && go-arch-lint check && gofmt -l . && go mod tidy` clean except the same pre-existing, already-documented rootless-Docker `TestIntegrationTwoGatewayInstancesShareOneRedisRateLimit` environmental failure.
