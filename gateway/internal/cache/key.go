package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Key fabricates the L1 exact-match cache key from the four fields that
// determine whether two requests are byte-for-byte equivalent for caching
// purposes: model, the serialized message history, temperature, and
// max_tokens. This is a real key fabricator, not a placeholder — callers
// (the dataplane pipeline) are expected to serialize a request's messages
// deterministically (e.g. canonical JSON) before calling Key, so the same
// logical request always produces the same key.
//
// This function deliberately takes primitive/serialized inputs rather
// than a canonical adapter.ChatRequest, so this package never needs to
// import internal/adapter — keeping cache decoupled from the request
// schema entirely, per its documented extractability goal
// (docs/decisions/0002-cache-embedded-in-gateway.md).
func Key(model string, serializedMessages string, temperature *float64, maxTokens *int) string {
	h := sha256.New()
	fmt.Fprintf(h, "model=%s\x00messages=%s\x00temperature=", model, serializedMessages)
	if temperature != nil {
		fmt.Fprintf(h, "%v", *temperature)
	}
	fmt.Fprint(h, "\x00max_tokens=")
	if maxTokens != nil {
		fmt.Fprintf(h, "%v", *maxTokens)
	}
	return hex.EncodeToString(h.Sum(nil))
}
