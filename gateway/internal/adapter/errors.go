package adapter

import "errors"

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
