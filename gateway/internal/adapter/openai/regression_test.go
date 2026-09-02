package openai

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/kelvran/gateway/internal/adapter"
)

// This file holds REGRESSION tests, distinct from openai_test.go's
// TestRoundTrip. TestRoundTrip proves internal consistency (canonical ->
// native -> canonical is lossless); it would still pass even if a future
// change silently renamed a native JSON tag, since both directions of the
// round trip would drift together. These tests instead pin the adapter's
// output against real, checked-in wire-format JSON fixtures
// (testdata/*.json), so an accidental change to the wire format itself
// (e.g. a JSON tag typo, a dropped field) is caught immediately, per
// docs/testing/TESTING.md §4's emphasis on testing against fixtures that
// "behave like the real thing rather than a hand-rolled stub."

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

// TestRegressionToProviderMatchesOpenAIWireFormat loads a canonical
// request fixture with a system message, a tool call, and multi-turn
// history, runs it through ToProvider, and asserts the produced native
// request byte-for-field matches the checked-in golden OpenAI wire-format
// JSON. If a future change to ToProvider silently drifts the wire shape
// (a renamed JSON tag, a dropped field), this test fails even though
// TestRoundTrip might not.
func TestRegressionToProviderMatchesOpenAIWireFormat(t *testing.T) {
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

	wantJSON := mustReadTestdata(t, "request_openai_native.golden.json")
	assertJSONEqual(t, gotJSON, wantJSON)
}

// TestRegressionFromProviderMatchesCanonicalWireFormat loads a real
// OpenAI-native response fixture (the wire format actually returned by
// OpenAI's Chat Completions API), runs it through FromProvider, and
// asserts the produced canonical response byte-for-field matches the
// checked-in golden canonical JSON.
func TestRegressionFromProviderMatchesCanonicalWireFormat(t *testing.T) {
	nativeJSON := mustReadTestdata(t, "response_openai_native.json")

	var native Response
	if err := json.Unmarshal(nativeJSON, &native); err != nil {
		t.Fatalf("unmarshaling response_openai_native.json: %v", err)
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
