package adapter

import (
	"errors"
	"fmt"
)

// ErrProviderContentPolicyBlocked is a sentinel error an adapter's
// FromProvider (or a streaming decoder's Decode) wraps when it detects
// that an upstream provider rejected the entire request for
// content-policy/safety reasons through a channel OTHER than an ordinary
// non-2xx HTTP response.
//
// Every provider this codebase supports signals an ordinary per-response
// content-policy rejection via a non-2xx HTTP status with recognizable
// body text — gateway/internal/gateway/dataplane/fallback.go's
// classifyFallbackError already classifies that shape as
// FallbackClassContentPolicy by pattern-matching a wrapped
// *UpstreamHTTPError's Body. That inspection can never fire for a
// FromProvider-local error, since no HTTP error occurred at all — the
// call succeeded with a 2xx status.
//
// Gemini is the first (and, as of this writing, only) real instance:
// confirmed against Google's live generateContent API reference, the
// API "Returns no candidates at all only if there was something wrong
// with the prompt" — a 200 OK response with an empty candidates array
// and promptFeedback.blockReason set. Without this sentinel, that
// condition surfaced as an ordinary, unclassifiable Go error, so a
// deployment's own configured content_policy fallback chain (built
// specifically to route around content-policy rejections) never fired
// for a Gemini prompt-level safety block, even though the semantic
// condition is identical to what a 4xx content-policy rejection from
// any other provider already routes around correctly. Wrapping this
// sentinel lets classifyFallbackError recognize the condition via
// errors.Is instead of errors.As-ing for *UpstreamHTTPError.
var ErrProviderContentPolicyBlocked = errors.New("adapter: upstream blocked the request for content-policy/safety reasons")

// ErrStructuredOutputUnsupported is a sentinel error dataplane.Pipeline
// wraps when a request sets ChatRequest.ResponseFormat but the
// deployment it would be sent to cannot enforce it (per
// adapter.SupportsStructuredOutput) -- and, for the first-pick call
// sites (dataplane.go's runMissPath, streaming.go's
// HandleChatCompletionStream), rerouteToCapableDeploymentIfNeeded's own
// best-effort search already failed to find a capable deployment
// elsewhere in the same pool.
//
// Closes a real gap: bedrock.additionalModelRequestFieldsFor's own
// documented behavior for a non-whitelisted model is to silently omit
// output_config.format entirely -- no error, no signal -- so a request
// reaching that deployment would otherwise proceed upstream with
// ResponseFormat silently dropped. AWS's own real behavior for a
// genuinely unsupported Converse request is a loud ValidationException,
// confirmed against AWS's official API reference -- Kelvran's prior
// silent-strip was strictly MORE silent than AWS itself would be.
//
// This sentinel only ever fires on the first-pick path when no capable
// deployment exists anywhere in the pool; attemptFallbackChain's own
// capabilityOK gate (fallback.go) already prevents the identical silent
// strip during a fallback hop by skipping an incapable target outright,
// and stays untouched by this sentinel's introduction.
var ErrStructuredOutputUnsupported = errors.New("adapter: response_format is not supported by this model and no capable deployment was found")

// ErrBedrockURLContentUnsupported is a sentinel error the Bedrock adapter
// wraps when a request includes a URL-based image or document
// ContentPart. This is a permanent AWS API constraint, not a Kelvran
// gap: confirmed against AWS's own current API reference, Bedrock's
// ImageSource/DocumentSource union types have exactly bytes/s3Location
// (plus content/text for documents) as valid members -- no generic-URL
// member exists for any model. Every other adapter (openai, anthropic,
// gemini, openaicompat) passes ContentPart.URL straight through to the
// provider verbatim; only Bedrock's own API has no equivalent to pass
// it to. Exported as a sentinel (rather than a bare fmt.Errorf, this
// error's prior shape) so a caller can errors.Is-detect this specific
// condition, matching ErrProviderContentPolicyBlocked's own convention.
var ErrBedrockURLContentUnsupported = errors.New("adapter: bedrock does not support URL-based image/document content; provide inline base64 data instead")

// UpstreamStreamError wraps a mid-stream, in-band error signal from an
// upstream provider on an ALREADY-2xx streaming connection -- an OpenAI/
// openaicompat native `data: {"error":{...}}` frame, an Anthropic
// `error` SSE event, or a Bedrock ConverseStream `:exception-type` (or
// generic RPC-level "error" message-type) frame. Every one of these
// embeds the raw, provider-authored error text verbatim (a Bedrock AWS
// exception payload can carry the operator's real AWS account ID and
// IAM role ARN; a self-hosted openaicompat backend's error.message can
// carry a stack trace or internal hostname) -- the exact same
// information-disclosure risk dataplane.UpstreamHTTPError already
// redacts for a non-2xx HTTP response (see that type's own doc comment
// for the full writeup of that risk). UpstreamHTTPError itself cannot
// cover this case: dataplane's HTTP upstream callers only construct one
// when the upstream HTTP status is >=300, but every path that
// constructs an UpstreamStreamError starts from an already-2xx
// streaming connection that then carries a provider-authored error
// INSIDE the body once decoding begins -- confirmed unreachable by
// UpstreamHTTPError's own errors.As check in cmd/gateway's
// writeErrorResponse before this type existed, so that check silently
// fell through to the unredacted default (err.Error()) for exactly this
// shape.
//
// Error()'s own string is SERVER-SIDE ONLY, mirroring
// UpstreamHTTPError.Error()'s identical convention -- it is what reaches
// dataplane.go's logRequest/logEmbeddingsRequest structured error log
// line via err.Error(), so an operator debugging a real upstream fault
// loses nothing to this fix. ClientSafeMessage is the ONLY text about
// this failure that cmd/gateway's writeErrorResponse may put in a
// client-facing HTTP error body.
type UpstreamStreamError struct {
	// Provider names which adapter produced this (e.g. "openai",
	// "openaicompat", "anthropic", "bedrock") -- safe to disclose, purely
	// diagnostic, matching the same disclosure level as
	// UpstreamHTTPError's status code.
	Provider string
	// Raw is the full, unredacted upstream-authored error text (and, for
	// Bedrock, the sentinel category text too -- see Error() below).
	// Never put in a client-facing response; see ClientSafeMessage.
	Raw string
	// Cause, if non-nil, is a typed sentinel (e.g.
	// bedrock.ErrBedrockThrottled) this error wraps -- returned by
	// Unwrap so a caller can still errors.Is-detect the specific
	// category through this wrapper, exactly as it could before this
	// type existed.
	Cause error
}

// Error implements the error interface. See the type's own doc comment
// for why this string is server-side only.
func (e *UpstreamStreamError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s", e.Cause.Error(), e.Raw)
	}
	return fmt.Sprintf("%s: %s", e.Provider, e.Raw)
}

// Unwrap exposes Cause so errors.Is still finds a wrapped typed
// sentinel through this error.
func (e *UpstreamStreamError) Unwrap() error {
	return e.Cause
}

// ClientSafeMessage mirrors dataplane.UpstreamHTTPError.ClientSafeMessage
// -- the only text about this failure safe to hand to a tenant. Never
// includes Raw.
func (e *UpstreamStreamError) ClientSafeMessage() string {
	return fmt.Sprintf("%s: upstream provider returned a mid-stream error", e.Provider)
}
