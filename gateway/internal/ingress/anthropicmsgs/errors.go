package anthropicmsgs

import "errors"

// Format is adapter.Passthrough.Format for every request this package parses.
const Format = "anthropic-messages"

// Request-shape errors Parse returns (wrapped with the offending pointer
// where one exists); the handler maps each to Anthropic's 400
// invalid_request_error. The adapter's outbound max_tokens default does not
// apply inbound: Anthropic requires the field, so a body without it is a
// client error here too.
var (
	ErrInvalidBody      = errors.New("anthropicmsgs: request body is not a JSON object")
	ErrMissingModel     = errors.New("anthropicmsgs: model is required")
	ErrMissingMaxTokens = errors.New("anthropicmsgs: max_tokens is required")
	ErrMissingMessages  = errors.New("anthropicmsgs: messages is required")
	ErrInvalidField     = errors.New("anthropicmsgs: invalid field")
)
