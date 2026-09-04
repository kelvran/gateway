- **Status**: accepted
- **Date**: 2026-09-04
- **Author(s)**: project founder + Claude Code

## Summary

Implement a real `gateway/internal/adapter/bedrock` adapter (buffered/non-streaming only this pass), targeting Amazon Bedrock Runtime's provider-agnostic **Converse** API. Unlike Gemini's adapter (which closed a gap `PRD.md`'s v1 scope line already named), this is a deliberate **scope expansion** the user explicitly requested — `PRD.md`'s v1 line names only OpenAI, Anthropic, Gemini, and OpenAI-compatible self-hosted runtimes; Bedrock is not on that list. `bedrock.go` is currently a full, honest stub (`ToProvider`/`FromProvider` both unconditionally error), per `docs/rfcs/2026-09-02-initial-code-scaffolding.md`'s original scoping.

This is the first adapter requiring a genuine `Deployment`/config-schema change — every prior adapter (OpenAI, Anthropic, Gemini, openaicompat) fit Kelvran's existing "one bearer-token-shaped secret per deployment" model; Bedrock's real authentication is AWS SigV4 request signing, which needs a fundamentally different credential shape (access key ID + secret access key + optional session token + region), not a header value.

## Motivation

Confirmed directly against the live tree: `bedrock.go` (39 lines) is the same stub shape `gemini.go` was before its own RFC. Grounded via a 4-angle dynamic-workflow research pass (API surface, auth/transport, streaming format, tool-use/stop-reasons) plus independent re-verification against real, current AWS SDK source — which caught a genuinely important correction the initial research got wrong.

**The corrected finding**: the research's synthesis stage recommended signing with AWS service name `"bedrock"`. Independently fetching and reading `aws-sdk-go-v2`'s real, current `service/bedrockruntime/endpoints.go` directly (not a docs page — the actual Go source the AWS SDK ships) shows the endpoint-resolution code's `SigningName` fallback is **`"amazonbedrockfrontendservice"`**, applied whenever the resolved endpoint carries no explicit override — and grepping the entire endpoint-resolution ruleset table in that same file for `SigningName:` found **zero** explicit per-region overrides anywhere, meaning this fallback is what standard Bedrock Runtime calls are signed with universally, not a rare edge case. Signing with the wrong service name produces a real, hard-to-diagnose `SignatureDoesNotMatch`/`InvalidSignatureException` from AWS with no local feedback (no live AWS account exists in this environment to test against) — getting this exactly right from source, not from a plausible-sounding guess, matters more here than for any wire-format field name.

The research also confirmed two other load-bearing facts by independently re-checking primary sources rather than trusting the first pass: (1) Bedrock's short-term "API keys" feature is itself just a pre-signed SigV4 credential wrapper (AWS's own security blog: the key value base64-decodes to a blob containing `X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=...`) — confirming SigV4/IAM is the real underlying mechanism, not an alternative Kelvran could sidestep; (2) `ConverseStream`'s real wire format is AWS's binary `application/vnd.amazon.eventstream` framing (length-prefixed, CRC-checksummed), not text-based SSE — confirmed via AWS's Ruby SDK response shape (`resp.stream #=> Enumerator` over typed binary events) and AWS's own docs for the structurally identical Transcribe streaming protocol, which explicitly describes "binary-encoded event stream messages, with three headers and a body."

## Detailed Design

### Scope: buffered Converse only — `ConverseStream` explicitly deferred

`gateway/internal/streaming/types.go`'s `StreamingAdapter` doc comment already states Bedrock "remains stubbed for both non-streaming and streaming" — this RFC closes only the non-streaming half. `StreamDecoder.Decode(raw streaming.SSEEvent)` is fundamentally text/line-shaped (built around `streaming.Reader`'s SSE parser); retrofitting it for AWS's binary, length-prefixed, CRC-checksummed event-stream framing would require either a parallel decoder path or a real dependency on `aws-sdk-go-v2/aws/protocol/eventstream` — genuine, separate work, correctly deferred to a follow-on RFC once the auth path is proven here. `bedrock.Adapter` does not implement `streaming.StreamingAdapter` this pass — a request routed to it for streaming continues returning the existing typed `dataplane.ErrStreamingNotSupported`, unchanged behavior.

### The real config-schema change

Confirmed by reading `gateway/internal/gateway/controlplane/config.go`'s `DeploymentConfig` and its `Load()` parsing/validation directly: every deployment today unconditionally requires `api_key_env` (`Load` errors if empty, regardless of provider). Bedrock has no single bearer secret — it needs `access_key_id_env`/`secret_access_key_env` (env var **names**, never raw values, matching `api_key_env`'s existing convention exactly) and an optional `session_token_env` (only needed for temporary/STS-issued credentials), plus a plain, non-secret `region` string field (SigV4 signing takes region as an explicit parameter — it cannot be reliably parsed back out of an arbitrary `base_url`).

```go
// DeploymentConfig gains (controlplane/config.go):
AccessKeyIDEnv     string // required if Provider == "bedrock"
SecretAccessKeyEnv string // required if Provider == "bedrock"
SessionTokenEnv    string // optional; only for temporary/STS credentials
Region             string // required if Provider == "bedrock"; not a secret
```

`Load()`'s validation becomes provider-conditional: `api_key_env` required unless `provider == "bedrock"`; `access_key_id_env`/`secret_access_key_env`/`region` required when it is. `dataplane.Deployment` gains matching resolved-value fields (`AccessKeyID`, `SecretAccessKey`, `SessionToken`, `Region`), populated in `cmd/gateway/main.go`'s `buildPipeline` exactly like `APIKey` already is (`os.Getenv(d.AccessKeyIDEnv)` etc., with the same "warn if unset, don't fail startup" posture `APIKey` resolution already has).

`base_url` keeps its existing, universal meaning across every adapter — the full, literal upstream endpoint URL (e.g. `https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20241022-v2:0/converse`) — no new URL-derivation function needed (unlike Gemini's `streamUpstreamURL`), since this pass never needs a second, differently-shaped endpoint for the same deployment.

### SigV4 signing (`setUpstreamAuthHeaders`)

`setUpstreamAuthHeaders(httpReq *http.Request, dep Deployment)` gains a `body []byte` parameter — SigV4 signs over a SHA-256 hash of the actual request payload, so the signer needs the already-marshaled body, which both call sites (`NewHTTPUpstreamCaller`/`NewHTTPUpstreamStreamCaller`) already have in scope before calling it. New dependency: `github.com/aws/aws-sdk-go-v2/aws/signer/v4` (confirmed real, current signature: `Signer.SignHTTP(ctx, aws.Credentials, *http.Request, payloadHash string, service string, region string, signingTime time.Time, ...) error` — mutates the request in place, setting `Authorization`/`X-Amz-Date`/`X-Amz-Security-Token` itself; the caller does not set these manually) plus its `aws` core package for `aws.Credentials{AccessKeyID, SecretAccessKey, SessionToken}` (confirmed real field names from source).

```go
case "bedrock":
    payloadHash := sha256Hex(body)
    creds := aws.Credentials{
        AccessKeyID:     dep.AccessKeyID,
        SecretAccessKey: dep.SecretAccessKey,
        SessionToken:    dep.SessionToken,
    }
    signer := v4.NewSigner()
    if err := signer.SignHTTP(ctx, creds, httpReq, payloadHash, "amazonbedrockfrontendservice", dep.Region, time.Now()); err != nil {
        return fmt.Errorf("signing bedrock request: %w", err)
    }
```

This requires `setUpstreamAuthHeaders` to return an `error` (every prior provider's branch never could fail; Bedrock's genuinely can) — a real, minor signature widening at both call sites, propagated as a wrapped error exactly like every other failure mode in `NewHTTPUpstreamCaller`/`NewHTTPUpstreamStreamCaller`.

### Field mapping (`ToProvider`/`FromProvider`)

Confirmed against AWS's real Converse API reference (`docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html`) and cross-provider compatibility matrix:

| Canonical | Bedrock Converse native | Notes |
|---|---|---|
| `Message{role:"system"}` | top-level `system[]` (`SystemContentBlock[]`, e.g. `{text: ...}`) | Converse's `messages[].role` accepts only `"user"`/`"assistant"` — the same system-prompt-placement hazard Anthropic/Gemini already solve; hoist out, join with `"\n\n"`. |
| `Message{role:"user"/"assistant"}` | `Message{role, content: ContentBlock[]}` | Direct role mapping (no `"model"`-style rename needed, unlike Gemini). |
| `Message{role:"tool"}` | a `user`-role message carrying `{toolResult: {toolUseId, content:[{text}], status:"success"}}` | Simpler than Gemini's hazard: `toolUseId` alone correlates — no separate `name`-resolution-from-history lookup needed, since `toolResult` has no required `name` field at all. |
| `ToolCall{ID, Name, ArgumentsJSON}` | `{toolUse: {toolUseId, name, input}}` | `input` is a native JSON object (`map[string]any`), not a string — same parse-at-the-boundary pattern as Anthropic/Gemini's `Input`/`Args`. |
| `ToolDef{Name, Description, ParametersJSON}` | `toolConfig.tools[].toolSpec{name, description, inputSchema:{json: <parsed>}}` | Parsed `map[string]any`, same as Anthropic's `InputSchema`. |
| `ChatRequest.Temperature`/`MaxTokens` | `inferenceConfig.temperature`/`maxTokens` | Direct 1:1. |
| `ChatResponse.Usage` | `usage.inputTokens`/`outputTokens`/`totalTokens` | Direct 1:1, real field names confirmed. |
| `Choice.FinishReason` | `stopReason` | See mapping below. |

**`stopReason` mapping** (real enum values confirmed): `end_turn`→`"stop"`; `tool_use`→`"tool_calls"`; `max_tokens`→`"length"`; `stop_sequence`→`"stop"`; `guardrail_intervened`/`content_filtered`→`"content_filter"`; `malformed_tool_use`/`malformed_model_output`→ a real, typed error (never a fake successful `Choice` — matches `gemini.go`'s existing "never fabricate success" convention for its own 5 malformed-tool-call reasons); `model_context_window_exceeded`→`"length"` (a named, documented approximation — the canonical schema has no exact equivalent, and this is the closest honest fit, not a silent guess).

**A real, named gap**: Converse's response has no native response-ID field, unlike OpenAI/Anthropic/Gemini (which have `id`/`responseId`). `ChatResponse.ID` is left empty for Bedrock — an honest absence, never a fabricated placeholder, matching this codebase's existing "`None`/empty means genuinely not applicable" convention (`Run.cost_usd`, `Score.rubric_axis`, etc.).

### `responseUnmarshalers`

One new entry, identical shape to every other provider's:

```go
"bedrock": func(b []byte) (any, error) {
    var r bedrock.Response
    if err := json.Unmarshal(b, &r); err != nil {
        return nil, fmt.Errorf("unmarshaling bedrock response: %w", err)
    }
    return &r, nil
},
```

## Drawbacks

- **The first real `Deployment`/config-schema change any adapter has needed.** Every prior provider fit the existing `api_key_env`-shaped model; Bedrock genuinely doesn't. Accepted because there's no way to sign a SigV4 request with a single bearer-token-shaped secret — this is a real requirement of AWS's own auth model, not a design choice Kelvran can narrow around.
- **New dependency**: `aws-sdk-go-v2/aws/signer/v4` (+ its `aws` core package) — the first AWS SDK dependency this project has taken on. Deliberately the small, focused signer package, not the full `aws-sdk-go-v2` service-client SDK (confirmed: `gateway/go.mod` currently has zero `aws-sdk-go-v2` packages).
- **`setUpstreamAuthHeaders` can now fail** — every prior provider's auth-header-setting branch was infallible; Bedrock's genuinely isn't (a signing error is a real, if rare, failure mode). Both call sites' error-propagation already had the shape to accommodate this trivially.
- No streaming this pass (see Scope) — a real, named gap, not a silent omission.
- The `"amazonbedrockfrontendservice"` signing-name finding was verified only by reading SDK source, never against a live AWS account (none exists in this environment) — flagged explicitly as the one claim in this RFC not end-to-end-tested against the real service, only independently source-verified.

## Alternatives Considered

1. **Target the older per-model `InvokeModel`/`InvokeModelWithResponseStream` API instead of Converse** — rejected; Converse is AWS's own recommended, provider-agnostic surface across every model family Bedrock hosts, and `InvokeModel` would require Kelvran to hand-write a distinct request/response shape per underlying model family (Anthropic-on-Bedrock's own schema, Llama's own schema, etc.) — exactly the fragmentation Converse exists to avoid.
2. **Implement `ConverseStream` in this same pass** — rejected; the binary event-stream framing is real, separate work (a new transport-level decoder, not a `StreamDecoder` retrofit) that would roughly double this pass's scope for a capability `streaming/types.go`'s own doc comment already scopes Bedrock out of today.
3. **Repurpose `APIKeyEnv` to hold a colon-joined access-key/secret pair** — rejected; a hacky string-packing scheme that breaks the "one env var, one secret" convention every other provider follows and complicates the resolution code for no real benefit over three explicit fields.
4. **Sign with service name `"bedrock"`** — rejected once independently verified against real, current AWS SDK source: the actual, universally-applied signing name is `"amazonbedrockfrontendservice"`.

## Unresolved Questions

- `ConverseStream`/binary event-stream decoding — deferred to a follow-on RFC.
- Multi-modal content blocks (image/document/video) — no canonical `Message` field exists for non-text content yet, same gap named in the Gemini RFC.
- Native Bedrock Guardrails integration — Kelvran has its own guardrail engine; whether Bedrock's is ever worth layering on top is a separate, future question.
- Cross-region inference profiles and provisioned-throughput ARNs (vs. a plain on-demand model ID in `UpstreamModel`) — out of scope this pass.
- Prompt caching (`cachePoint`, cache-token usage fields) and `reasoningContent`/citation blocks — real Converse features, deliberately not mapped this pass.

## Research Trail

Grounded via a 4-angle dynamic-workflow research pass (API surface, auth/transport, streaming format, tool-use/stop-reasons) plus a synthesis stage that independently re-verified the two highest-risk claims. Before writing this RFC, further independently re-verified directly against real, current `aws-sdk-go-v2` source (not docs pages): fetched `service/bedrockruntime/endpoints.go` and confirmed the real SigV4 signing-name fallback (`"amazonbedrockfrontendservice"`, universally applied — zero per-region `SigningName:` overrides found anywhere in the endpoint-resolution table), `aws/signer/v4/v4.go`'s real `Signer.SignHTTP`/`NewSigner` signatures, and `aws/credentials.go`'s real `Credentials` struct field names — catching that the grounding research's own recommended service name (`"bedrock"`) was wrong. Also read `gateway/internal/gateway/controlplane/config.go`, `gateway/internal/gateway/dataplane/dataplane.go` (`setUpstreamAuthHeaders`, `NewHTTPUpstreamCaller`, `streamUpstreamURL`), `gateway/internal/gateway/dataplane/streaming.go`, `gateway/cmd/gateway/main.go`'s `buildPipeline`, `gateway/internal/adapter/gemini/{gemini,stream}.go`, and `gateway/internal/adapter/bedrock/bedrock.go` (the current stub) directly to ground every design decision in this specific codebase's real conventions.
