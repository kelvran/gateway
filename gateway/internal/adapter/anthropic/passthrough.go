package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// PassthroughRequest is ToProvider's result on the passthrough path (RFC-1
// §5, item 11 slice S11a): the client's Anthropic Messages body as received,
// with exactly two top-level rewrites -- model → the deployment's upstream id,
// stream → the pipeline's value -- and the anthropic-* request headers the
// handler collected for the upstream hop. The dataplane's HTTP callers send
// Raw as the request body unchanged (upstreamRequestBody), so unknown members
// at every depth, the client's member order and even the whitespace inside
// nested values reach Anthropic byte-for-byte; json.Marshal callers get the
// same bytes through MarshalJSON, compacted by encoding/json. Because this is
// not a *Request, setUpstreamAuthHeaders adds no thinking-binding beta on
// this path -- the client's own anthropic-beta travels in Headers instead
// (RFC-1 §5's "not applied" decision).
type PassthroughRequest struct {
	// Raw is the rewritten body.
	Raw []byte
	// Headers carries the anthropic-* request headers to forward
	// (adapter.Passthrough.ForwardHeaders); the dataplane re-checks the
	// prefix before copying them.
	Headers http.Header
}

// Body returns the bytes to send upstream.
func (r *PassthroughRequest) Body() []byte { return r.Raw }

// MarshalJSON lets generic json.Marshal callers send Raw.
func (r *PassthroughRequest) MarshalJSON() ([]byte, error) { return r.Raw, nil }

// passthroughRequestFor builds the passthrough request when the canonical
// request carries an Anthropic Messages envelope with a raw body. ok is false
// -- encode the shadow instead -- for the OpenAI route, a cache replay and any
// other envelope.
func passthroughRequestFor(req adapter.ChatRequest) (*PassthroughRequest, bool, error) {
	pt := req.Passthrough
	if pt == nil || pt.Format != adapter.IngressFormatAnthropicMessages || len(pt.RawBody) == 0 {
		return nil, false, nil
	}
	raw, err := rewriteTopLevel(pt.RawBody, req.Model, req.Stream)
	if err != nil {
		return nil, false, fmt.Errorf("anthropic: passthrough body: %w", err)
	}
	return &PassthroughRequest{Raw: raw, Headers: pt.ForwardHeaders}, true, nil
}

// rewriteTopLevel re-emits the top-level object of raw with model and stream
// set to the given values -- replaced in place when present, appended when
// absent -- and every other member copied as its original bytes in the
// original order. Only member names are re-encoded (canonically); values are
// never touched, so nested whitespace and unknown members survive. The
// ingress has already rejected duplicate members and trailing data.
func rewriteTopLevel(raw []byte, model string, stream bool) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('{') {
		return nil, errors.New("top level is not a JSON object")
	}
	modelJSON, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	streamJSON := []byte(strconv.FormatBool(stream)) // bare true/false: valid JSON, Anthropic's wire form

	var out bytes.Buffer
	out.Grow(len(raw) + 64)
	out.WriteByte('{')
	first, sawModel, sawStream := true, false, false
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("unexpected token %v where a member name was expected", keyTok)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		switch key {
		case "model":
			value, sawModel = modelJSON, true
		case "stream":
			value, sawStream = streamJSON, true
		}
		keyJSON, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(keyJSON)
		out.WriteByte(':')
		out.Write(value)
	}
	closing, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("malformed top-level object: %w", err)
	}
	if closing != json.Delim('}') {
		return nil, fmt.Errorf("malformed top-level object: unexpected %T after the last member", closing)
	}
	if !sawModel {
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.WriteString(`"model":`)
		out.Write(modelJSON)
	}
	if !sawStream {
		if !first {
			out.WriteByte(',')
		}
		out.WriteString(`"stream":`)
		out.Write(streamJSON)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}
