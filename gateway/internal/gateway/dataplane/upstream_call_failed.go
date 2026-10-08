package dataplane

import (
	"errors"
	"fmt"
)

// ErrUpstreamCallFailed is the sentinel every "the upstream call itself
// failed" error from HandleChatCompletion, HandleChatCompletionStream and
// HandleEmbeddings wraps (errors.Is), whatever the underlying cause: a
// provider's non-2xx status (*UpstreamHTTPError, still reachable by
// errors.As through this wrapper), a mid-stream decode error, or a transport
// failure such as a refused dial or a timeout.
//
// It exists so cmd/gateway can tell "an upstream call failed" apart from
// every other error without matching message text, and in particular so a
// TRANSPORT failure's message can be redacted before it reaches a tenant:
// until 2026-10-08 a refused dial or a timeout was returned verbatim --
// `... calling upstream "http://10.0.0.5:8000": Post "...": dial tcp
// 10.0.0.5:8000: connect: connection refused` -- which named the
// deployment's internal BaseURL and host:port, exactly what
// UpstreamHTTPError.ClientSafeMessage exists to keep from tenants. The full
// text still reaches the operator in the chat_completion / embeddings log
// line. Found while writing docs/operations/FAILURE-MODES.md.
var ErrUpstreamCallFailed = errors.New("dataplane: upstream call failed")

// UpstreamCallFailedError wraps the cause of a failed upstream call for one
// request. Its Error text is byte-identical to the fmt.Errorf wraps it
// replaced, so log lines and existing assertions are unchanged; Is reports
// ErrUpstreamCallFailed and Unwrap keeps the typed cause reachable.
type UpstreamCallFailedError struct {
	model     string
	streaming bool
	err       error
}

func (e *UpstreamCallFailedError) Error() string {
	if e.streaming {
		return fmt.Sprintf("dataplane: streaming upstream call failed for model %q: %v", e.model, e.err)
	}
	return fmt.Sprintf("dataplane: upstream call failed for model %q: %v", e.model, e.err)
}

func (e *UpstreamCallFailedError) Unwrap() error { return e.err }

// Model is the client-supplied model name the failed call was for -- safe to
// echo back to the client, which chose it.
func (e *UpstreamCallFailedError) Model() string { return e.model }

func (e *UpstreamCallFailedError) Is(target error) bool { return target == ErrUpstreamCallFailed }

// WrapUpstreamCallFailed marks err as the failure of the upstream call for
// model (streaming selects the streaming message prefix). A nil err returns
// nil.
func WrapUpstreamCallFailed(model string, streaming bool, err error) error {
	if err == nil {
		return nil
	}
	return &UpstreamCallFailedError{model: model, streaming: streaming, err: err}
}
