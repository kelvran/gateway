package bedrock

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// This file holds REGRESSION tests, distinct from bedrock_test.go's
// TestRoundTrip. TestRoundTrip proves internal consistency (canonical ->
// native -> canonical is lossless); it would still pass even if a future
// change silently renamed a native JSON tag, since both directions of the
// round trip would drift together. These tests instead pin the adapter's
// output against real, checked-in wire-format JSON fixtures
// (testdata/*.json), so an accidental change to the wire format itself
// (e.g. a JSON tag typo) is caught immediately, per docs/testing/TESTING.md
// §4's emphasis on testing against fixtures that "behave like the real
// thing rather than a hand-rolled stub." Every fixture's shape was
// confirmed directly against AWS's live API reference before being
// checked in -- see docs/rfcs/2026-09-04-bedrock-adapter.md.

// mustReadTestdata reads a fixture file, failing the test on error.
func mustReadTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading testdata/%s: %v", name, err)
	}
	return data
}

// assertJSONEqual compares two JSON documents field-for-field (by
// unmarshaling both into generic map[string]any / []any trees and
// deep-comparing), rather than byte-for-byte, so the test is robust to
// key-ordering differences between what encoding/json produces and how
// the golden fixture happens to be formatted on disk.
func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()

	var gotTree, wantTree any
	if err := json.Unmarshal(got, &gotTree); err != nil {
		t.Fatalf("unmarshaling actual JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal(want, &wantTree); err != nil {
		t.Fatalf("unmarshaling golden JSON: %v\n%s", err, want)
	}

	if !reflect.DeepEqual(gotTree, wantTree) {
		gotPretty, _ := json.MarshalIndent(gotTree, "", "  ")
		wantPretty, _ := json.MarshalIndent(wantTree, "", "  ")
		t.Errorf("wire format mismatch:\n--- got ---\n%s\n--- want (golden fixture) ---\n%s", gotPretty, wantPretty)
	}
}

// TestRegressionToProviderMatchesBedrockWireFormat loads a canonical
// request fixture with a system message, a tool call, and multi-turn
// history (including a role:"tool" result message), runs it through
// ToProvider, and asserts the produced native request byte-for-field
// matches the checked-in golden Bedrock Converse wire-format JSON --
// system pulled into system[], tool calls/results converted to
// toolUse/toolResult content blocks, per every documented normalization
// hazard.
func TestRegressionToProviderMatchesBedrockWireFormat(t *testing.T) {
	canonicalJSON := mustReadTestdata(t, "request_canonical.json")

	var req adapter.ChatRequest
	if err := json.Unmarshal(canonicalJSON, &req); err != nil {
		t.Fatalf("unmarshaling request_canonical.json: %v", err)
	}

	a := New()
	native, err := a.ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}

	gotJSON, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("marshaling ToProvider output: %v", err)
	}

	wantJSON := mustReadTestdata(t, "request_bedrock_native.golden.json")
	assertJSONEqual(t, gotJSON, wantJSON)
}

// TestRegressionToProviderNilResponseFormatMatchesExistingGoldenFixture
// is the backward-compatibility half of the structured-output schema-
// bearing proof below: a request with ResponseFormat left nil (the
// default, and every request built before this field existed) must
// produce output BYTE-IDENTICAL to the pre-existing golden fixture --
// TestRegressionToProviderMatchesBedrockWireFormat above already proves
// this implicitly (its fixture never set response_format), but this test
// names the guarantee explicitly, mirroring the other four adapter
// packages' own identically-named test.
func TestRegressionToProviderNilResponseFormatMatchesExistingGoldenFixture(t *testing.T) {
	canonicalJSON := mustReadTestdata(t, "request_canonical.json")

	var req adapter.ChatRequest
	if err := json.Unmarshal(canonicalJSON, &req); err != nil {
		t.Fatalf("unmarshaling request_canonical.json: %v", err)
	}
	if req.ResponseFormat != nil {
		t.Fatal("setup: request_canonical.json fixture must not itself set response_format")
	}

	native, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	gotJSON, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("marshaling ToProvider output: %v", err)
	}

	wantJSON := mustReadTestdata(t, "request_bedrock_native.golden.json")
	assertJSONEqual(t, gotJSON, wantJSON)
}

// TestRegressionToProviderResponseFormatMatchesBedrockWireFormat is the
// schema-bearing round-trip proof for structured output: a canonical
// request carrying ResponseFormat, sent against a Bedrock model ON the
// structured-output whitelist, must translate onto Converse's real
// additionalModelRequestFields.output_config.format.{type,schema} escape
// hatch, live-verified against a real Converse API call.
func TestRegressionToProviderResponseFormatMatchesBedrockWireFormat(t *testing.T) {
	canonicalJSON := mustReadTestdata(t, "request_canonical.json")

	var req adapter.ChatRequest
	if err := json.Unmarshal(canonicalJSON, &req); err != nil {
		t.Fatalf("unmarshaling request_canonical.json: %v", err)
	}
	// The fixture's own model (Claude 3.5 Sonnet) is NOT on the
	// structured-output whitelist -- overridden here to a whitelisted
	// Haiku-4.5-style Bedrock model ID so this test actually exercises the
	// populated-field path, not the named-scope-limit omission path
	// TestToProviderResponseFormatOmitsAdditionalModelRequestFieldsOnUnsupportedModel
	// (bedrock_test.go) already covers.
	req.Model = "global.anthropic.claude-haiku-4-5-20251001-v1:0"
	req.ResponseFormat = &adapter.ResponseFormat{
		Type: "json_schema",
		JSONSchema: &adapter.JSONSchema{
			Name:   "weather_response",
			Schema: json.RawMessage(`{"type":"object","properties":{"temp_f":{"type":"number"}},"required":["temp_f"]}`),
		},
	}

	native, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	gotJSON, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("marshaling ToProvider output: %v", err)
	}

	wantJSON := mustReadTestdata(t, "request_bedrock_native.golden.json")
	var wantTree map[string]any
	if err := json.Unmarshal(wantJSON, &wantTree); err != nil {
		t.Fatalf("unmarshaling golden fixture: %v", err)
	}
	wantTree["additionalModelRequestFields"] = map[string]any{
		"output_config": map[string]any{
			"format": map[string]any{
				"type": "json_schema",
				"schema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"temp_f": map[string]any{"type": "number"}},
					"required":   []any{"temp_f"},
				},
			},
		},
	}
	wantJSONWithFields, err := json.Marshal(wantTree)
	if err != nil {
		t.Fatalf("marshaling augmented golden fixture: %v", err)
	}
	assertJSONEqual(t, gotJSON, wantJSONWithFields)
}

// TestRegressionFromProviderMatchesCanonicalWireFormat loads a real
// Bedrock-native response fixture, runs it through FromProvider, and
// asserts the produced canonical response byte-for-field matches the
// checked-in golden canonical JSON -- including the inputTokens/
// outputTokens -> prompt_tokens/completion_tokens/total_tokens
// field-name remapping and the honest empty id/model (Converse has no
// native response-ID field).
func TestRegressionFromProviderMatchesCanonicalWireFormat(t *testing.T) {
	nativeJSON := mustReadTestdata(t, "response_bedrock_native.json")

	var native Response
	if err := json.Unmarshal(nativeJSON, &native); err != nil {
		t.Fatalf("unmarshaling response_bedrock_native.json: %v", err)
	}

	a := New()
	canonical, err := a.FromProvider(&native)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}

	gotJSON, err := json.Marshal(canonical)
	if err != nil {
		t.Fatalf("marshaling FromProvider output: %v", err)
	}

	wantJSON := mustReadTestdata(t, "response_canonical.golden.json")
	assertJSONEqual(t, gotJSON, wantJSON)
}

// TestRegressionFromProviderMatchesCanonicalWireFormatWithCacheTokens is
// the cache-token-cost-accounting counterpart to the test above: a real
// Bedrock-native response fixture carrying cacheReadInputTokens/
// cacheWriteInputTokens must fold them into prompt_tokens/total_tokens
// (Converse's own totalTokens is inputTokens+outputTokens ONLY) and
// surface them as cache_creation_tokens on the canonical side.
func TestRegressionFromProviderMatchesCanonicalWireFormatWithCacheTokens(t *testing.T) {
	nativeJSON := mustReadTestdata(t, "response_bedrock_native_cached.json")

	var native Response
	if err := json.Unmarshal(nativeJSON, &native); err != nil {
		t.Fatalf("unmarshaling response_bedrock_native_cached.json: %v", err)
	}

	a := New()
	canonical, err := a.FromProvider(&native)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}

	gotJSON, err := json.Marshal(canonical)
	if err != nil {
		t.Fatalf("marshaling FromProvider output: %v", err)
	}

	wantJSON := mustReadTestdata(t, "response_canonical_cached.golden.json")
	assertJSONEqual(t, gotJSON, wantJSON)
}
