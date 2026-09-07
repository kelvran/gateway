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
