package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToolDefMarshalJSONIncludesCacheControl proves ToolDef's hand-written
// MarshalJSON (needed because ToolDef's wire shape nests name/description/
// parameters under a "function" key, per its own doc comment) correctly
// carries a set CacheControl through as a sibling key of "type"/"function"
// -- not nested inside "function" -- per
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md's
// tool-definition-level addendum. Unlike Message/ContentPart's plain
// struct-tag CacheControl field, ToolDef's custom (Un)MarshalJSON pair
// has no default encoding/json behavior to fall back on, so this is the
// one load-bearing proof that the field isn't silently dropped or
// misplaced on the wire.
func TestToolDefMarshalJSONIncludesCacheControl(t *testing.T) {
	td := ToolDef{
		Name:           "get_weather",
		Description:    "Get the weather",
		ParametersJSON: `{"type":"object"}`,
		CacheControl:   &CacheControl{TTL: "1h", Key: "session-123"},
	}

	b, err := json.Marshal(td)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var tree map[string]any
	if err := json.Unmarshal(b, &tree); err != nil {
		t.Fatalf("unmarshaling marshaled output: %v\n%s", err, b)
	}
	cc, ok := tree["cache_control"].(map[string]any)
	if !ok {
		t.Fatalf("marshaled ToolDef has no top-level cache_control key: %s", b)
	}
	if cc["ttl"] != "1h" || cc["key"] != "session-123" {
		t.Errorf("cache_control = %+v, want {ttl:1h key:session-123}", cc)
	}
	if fn, ok := tree["function"].(map[string]any); ok {
		if _, nested := fn["cache_control"]; nested {
			t.Errorf("cache_control must be a sibling of function, not nested inside it: %s", b)
		}
	}
}

// TestToolDefMarshalJSONOmitsCacheControlWhenUnset proves the unset
// (nil, default) case never emits a cache_control field at all, matching
// this schema's existing optional-field convention.
func TestToolDefMarshalJSONOmitsCacheControlWhenUnset(t *testing.T) {
	td := ToolDef{Name: "get_weather", Description: "Get the weather", ParametersJSON: `{"type":"object"}`}

	b, err := json.Marshal(td)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), "cache_control") {
		t.Errorf("marshaled ToolDef contains cache_control despite CacheControl being unset: %s", b)
	}
}

// TestToolDefUnmarshalJSONReadsCacheControl proves the reverse direction:
// a client-supplied ToolDef JSON document with a top-level cache_control
// key unmarshals into ToolDef.CacheControl correctly -- the real path a
// caller's inbound HTTP request body takes, since the canonical schema
// doubles as Kelvran's own client-facing wire format.
func TestToolDefUnmarshalJSONReadsCacheControl(t *testing.T) {
	wire := `{
		"type": "function",
		"function": {
			"name": "get_weather",
			"description": "Get the weather",
			"parameters": {"type": "object"}
		},
		"cache_control": {"ttl": "1h", "key": "session-123"}
	}`

	var td ToolDef
	if err := json.Unmarshal([]byte(wire), &td); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if td.Name != "get_weather" {
		t.Errorf("Name = %q, want get_weather", td.Name)
	}
	if td.CacheControl == nil {
		t.Fatalf("CacheControl = nil, want {ttl:1h key:session-123}")
	}
	if td.CacheControl.TTL != "1h" || td.CacheControl.Key != "session-123" {
		t.Errorf("CacheControl = %+v, want {TTL:1h Key:session-123}", td.CacheControl)
	}
}

// TestToolDefUnmarshalJSONWithoutCacheControlLeavesItNil proves a wire
// document with no cache_control key at all leaves ToolDef.CacheControl
// nil, not a zero-value non-nil struct -- matching Message/ContentPart's
// own nil-is-the-default convention.
func TestToolDefUnmarshalJSONWithoutCacheControlLeavesItNil(t *testing.T) {
	wire := `{
		"type": "function",
		"function": {"name": "get_weather"}
	}`

	var td ToolDef
	if err := json.Unmarshal([]byte(wire), &td); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if td.CacheControl != nil {
		t.Errorf("CacheControl = %+v, want nil", td.CacheControl)
	}
}

// TestChatRequestDisableCacheControlAutoPopulateIsWireUnreachable is the
// load-bearing proof for docs/rfcs/2026-09-07-gateway-cache-control-
// auto-populate.md's json:"-" choice on ChatRequest.
// DisableCacheControlAutoPopulate: a client-supplied request body can
// never set this field via unmarshal (it expresses an operator's
// deployment-level policy, never a per-call client choice), and marshaling
// a ChatRequest that has it set (as dataplane does, per-call, before
// ToProvider) never leaks it back onto any wire representation either.
func TestChatRequestDisableCacheControlAutoPopulateIsWireUnreachable(t *testing.T) {
	// Direction 1: unmarshal. A malicious or merely curious client trying
	// to set the field directly via the JSON body must have zero effect.
	wire := `{"model":"gpt-4o","messages":[],"disable_cache_control_auto_populate":true,"DisableCacheControlAutoPopulate":true}`
	var req ChatRequest
	if err := json.Unmarshal([]byte(wire), &req); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if req.DisableCacheControlAutoPopulate {
		t.Error("DisableCacheControlAutoPopulate = true after unmarshaling a wire body that tried to set it -- it must be wire-unreachable")
	}

	// Direction 2: marshal. dataplane sets this field internally
	// (per-call, before ToProvider) -- it must never leak back onto any
	// JSON representation of the request (e.g. a future logging/tracing
	// path that marshals ChatRequest).
	req2 := ChatRequest{Model: "gpt-4o", DisableCacheControlAutoPopulate: true}
	b, err := json.Marshal(req2)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), "isable") || strings.Contains(string(b), "auto_populate") {
		t.Errorf("marshaled ChatRequest leaks DisableCacheControlAutoPopulate onto the wire: %s", b)
	}
}

// TestEmbeddingRequestUnmarshalJSONAcceptsABareString is the regression
// proof for a real gap an audit found: EmbeddingRequest.Input's own doc
// comment already claimed OpenAI's real "input: string | string[]"
// contract, but before UnmarshalJSON existed, a bare JSON string was
// rejected outright with a raw "cannot unmarshal string into []string"
// error from the default encoding/json behavior, never reaching this
// package's translation logic at all.
func TestEmbeddingRequestUnmarshalJSONAcceptsABareString(t *testing.T) {
	var req EmbeddingRequest
	wire := `{"model":"text-embedding-3-small","input":"the quick brown fox"}`
	if err := json.Unmarshal([]byte(wire), &req); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if req.Model != "text-embedding-3-small" {
		t.Errorf("Model = %q, want text-embedding-3-small", req.Model)
	}
	if len(req.Input) != 1 || req.Input[0] != "the quick brown fox" {
		t.Errorf("Input = %v, want a single-element slice [\"the quick brown fox\"]", req.Input)
	}
}

// TestEmbeddingRequestUnmarshalJSONAcceptsAnArrayOfStrings proves the
// pre-existing, more common shape still works unchanged.
func TestEmbeddingRequestUnmarshalJSONAcceptsAnArrayOfStrings(t *testing.T) {
	var req EmbeddingRequest
	wire := `{"model":"text-embedding-3-small","input":["hello","world"],"dimensions":256}`
	if err := json.Unmarshal([]byte(wire), &req); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(req.Input) != 2 || req.Input[0] != "hello" || req.Input[1] != "world" {
		t.Errorf("Input = %v, want [hello world]", req.Input)
	}
	if req.Dimensions != 256 {
		t.Errorf("Dimensions = %d, want 256", req.Dimensions)
	}
}

// TestEmbeddingRequestUnmarshalJSONRejectsANonStringArrayEntry proves an
// array containing a non-string entry is a real, reported error, not
// silently coerced (e.g. a number stringified) or a panic.
func TestEmbeddingRequestUnmarshalJSONRejectsANonStringArrayEntry(t *testing.T) {
	var req EmbeddingRequest
	wire := `{"model":"text-embedding-3-small","input":["hello", 42]}`
	if err := json.Unmarshal([]byte(wire), &req); err == nil {
		t.Fatal("Unmarshal with a non-string array entry: got nil error, want a real error")
	}
}

// TestEmbeddingRequestUnmarshalJSONRejectsANumberInput mirrors the
// array-entry proof for the top-level input field itself.
func TestEmbeddingRequestUnmarshalJSONRejectsANumberInput(t *testing.T) {
	var req EmbeddingRequest
	wire := `{"model":"text-embedding-3-small","input":42}`
	if err := json.Unmarshal([]byte(wire), &req); err == nil {
		t.Fatal("Unmarshal with input=42: got nil error, want a real error")
	}
}

// TestEmbeddingRequestUnmarshalJSONWithNoInputLeavesItNil proves omitting
// input entirely leaves Input nil, matching the plain struct-tag default
// this custom UnmarshalJSON replaced -- callers (e.g. cmd/gateway's own
// "input is required and must be non-empty" check) rely on this.
func TestEmbeddingRequestUnmarshalJSONWithNoInputLeavesItNil(t *testing.T) {
	var req EmbeddingRequest
	wire := `{"model":"text-embedding-3-small"}`
	if err := json.Unmarshal([]byte(wire), &req); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if req.Input != nil {
		t.Errorf("Input = %v, want nil", req.Input)
	}
}

// TestChatRequestThinkingIsUnreachableFromJSON pins the json:"-" tag on
// ChatRequest.Thinking: the field is set only by the Anthropic Messages
// ingress (docs/rfcs/2026-10-09-gateway-anthropic-messages-ingress.md §3),
// never by a /v1/chat/completions body, which unmarshals straight into
// this type -- so a client posting a "thinking" object there must get the
// same silent unknown-field drop it gets today, and marshaling a request
// that carries one must not leak it onto the OpenAI-shaped wire either.
func TestChatRequestThinkingIsUnreachableFromJSON(t *testing.T) {
	var req ChatRequest
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive","budget_tokens":1024}}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if req.Thinking != nil {
		t.Errorf("Thinking = %+v after unmarshaling a body with a thinking object, want nil (json:\"-\")", req.Thinking)
	}

	out, err := json.Marshal(ChatRequest{Model: "m", Thinking: &ThinkingConfig{Type: "adaptive", BudgetTokens: 1024}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "thinking") {
		t.Errorf("Marshal leaked the thinking field onto the wire: %s", out)
	}
}

// TestChatRequestStopDecodesStringOrArray pins the wire contract for the
// canonical StopSequences field (item 11 slice S5): OpenAI's `stop` is a
// string or an array of strings, both must decode (a bare string was an
// IGNORED unknown field before this slice, so a 400 on it now would be a
// regression), null and absence mean none, a non-string value is a real
// decode error, and the field always marshals as an array -- the
// canonical form every provider adapter and the idempotency fingerprint
// read.
func TestChatRequestStopDecodesStringOrArray(t *testing.T) {
	decode := func(t *testing.T, body string) ChatRequest {
		t.Helper()
		var req ChatRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("Unmarshal(%s): %v", body, err)
		}
		return req
	}
	if got := decode(t, `{"model":"m","messages":[],"stop":"END"}`).StopSequences; len(got) != 1 || got[0] != "END" {
		t.Errorf("stop string: got %#v, want [END]", got)
	}
	if got := decode(t, `{"model":"m","messages":[],"stop":["a","b"]}`).StopSequences; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("stop array: got %#v, want [a b]", got)
	}
	if got := decode(t, `{"model":"m","messages":[],"stop":null}`).StopSequences; got != nil {
		t.Errorf("stop null: got %#v, want nil", got)
	}
	if got := decode(t, `{"model":"m","messages":[]}`).StopSequences; got != nil {
		t.Errorf("stop absent: got %#v, want nil", got)
	}
	var bad ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[],"stop":5}`), &bad); err == nil {
		t.Error("stop number: Unmarshal succeeded, want a decode error")
	}

	out, err := json.Marshal(ChatRequest{Model: "m", StopSequences: StopSequences{"END"}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), `"stop":["END"]`) {
		t.Errorf("Marshal = %s, want the array form \"stop\":[\"END\"]", out)
	}
	out, err = json.Marshal(ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "stop") {
		t.Errorf("Marshal of a request without stop sequences emitted a stop key: %s", out)
	}
}

// TestChatRequestTopPDecodesAndOmits pins the wire contract for the
// canonical TopP field (item 11 slice S5): OpenAI's `top_p` decodes to a
// pointer so 0 and absent stay distinct, and an unset field marshals no
// key -- byte-identical to every request built before the field existed.
func TestChatRequestTopPDecodesAndOmits(t *testing.T) {
	var req ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[],"top_p":0.9}`), &req); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if req.TopP == nil || *req.TopP != 0.9 {
		t.Fatalf("TopP = %v, want 0.9", req.TopP)
	}
	var zero ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[],"top_p":0}`), &zero); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if zero.TopP == nil || *zero.TopP != 0 {
		t.Errorf("TopP = %v for an explicit 0, want a pointer to 0 (distinct from absent)", zero.TopP)
	}
	out, err := json.Marshal(ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "top_p") {
		t.Errorf("Marshal of a request without top_p emitted the key: %s", out)
	}
}
