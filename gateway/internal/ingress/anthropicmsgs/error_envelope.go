package anthropicmsgs

import "encoding/json"

// ParseErrorEnvelope recognises Anthropic's error object -- {"type":"error",
// "error":{"type":<non-empty>,"message":<non-empty>}} with any other members
// -- and returns its type and message. It is the gate of RFC-1 §9's verbatim
// exception (item 11 slice S11b): an anthropic deployment's 400 or 422 is
// relayed to the client as received only when the body is this shape, so
// Claude Code's recovery can match Anthropic's own wording; a body of any
// other shape (a Bedrock message, an OpenAI envelope, HTML) stays redacted.
func ParseErrorEnvelope(body []byte) (errType, message string, ok bool) {
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Type != "error" || env.Error.Type == "" || env.Error.Message == "" {
		return "", "", false
	}
	return env.Error.Type, env.Error.Message, true
}
