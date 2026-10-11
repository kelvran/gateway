package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adminapi"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// wireType pairs a component schema with the Go type whose JSON tags it must
// mirror. request marks types the gateway decodes (required = what the handler
// demands) rather than encodes (required ⊇ every non-omitempty field).
type wireType struct {
	schema  string
	goType  reflect.Type
	request bool
	// stamped names response fields the pipeline sets after marshalling
	// decisions, so they are required in the document although omitempty in Go.
	stamped []string
}

var wireTypes = []wireType{
	{"ChatCompletionRequest", reflect.TypeOf(adapter.ChatRequest{}), true, nil},
	{"Message", reflect.TypeOf(adapter.Message{}), true, nil},
	{"ContentPart", reflect.TypeOf(adapter.ContentPart{}), true, nil},
	{"CacheControl", reflect.TypeOf(adapter.CacheControl{}), true, nil},
	{"ReasoningBlock", reflect.TypeOf(adapter.ReasoningBlock{}), true, nil},
	{"ResponseFormat", reflect.TypeOf(adapter.ResponseFormat{}), true, nil},
	{"JSONSchema", reflect.TypeOf(adapter.JSONSchema{}), true, nil},
	{"ChatCompletionResponse", reflect.TypeOf(adapter.ChatResponse{}), false, []string{"object", "created"}},
	{"Choice", reflect.TypeOf(adapter.Choice{}), false, nil},
	{"Usage", reflect.TypeOf(adapter.Usage{}), false, nil},
	{"InputTransformation", reflect.TypeOf(adapter.InputTransformation{}), false, nil},
	{"ChatCompletionChunk", reflect.TypeOf(streaming.ChatCompletionChunk{}), false, []string{"object", "created"}},
	{"ChunkChoice", reflect.TypeOf(streaming.ChunkChoice{}), false, nil},
	{"MessageDelta", reflect.TypeOf(streaming.MessageDelta{}), false, nil},
	{"ToolCallDelta", reflect.TypeOf(streaming.ToolCallDelta{}), false, nil},
	{"ReasoningDelta", reflect.TypeOf(streaming.ReasoningDelta{}), false, nil},
	{"EmbeddingsResponse", reflect.TypeOf(adapter.EmbeddingResponse{}), false, nil},
	{"EmbeddingData", reflect.TypeOf(adapter.EmbeddingData{}), false, nil},
	{"ModelList", reflect.TypeOf(modelsListDocument{}), false, nil},
	{"Model", reflect.TypeOf(modelEntry{}), false, nil},
	{"OpenAIError", reflect.TypeOf(apiError{}), false, nil},
	{"VirtualKeyRequest", reflect.TypeOf(adminapi.VirtualKeyRequest{}), true, nil},
	{"RotateVirtualKeyRequest", reflect.TypeOf(adminapi.RotateVirtualKeyRequest{}), true, nil},
	{"RateLimitRequest", reflect.TypeOf(adminapi.RateLimitRequest{}), true, nil},
	{"PerModelRateLimitRequest", reflect.TypeOf(adminapi.PerModelRateLimitRequest{}), true, nil},
	{"AuditEntryResponse", reflect.TypeOf(adminapi.AuditEntryResponse{}), false, nil},
	{"BackupResponse", reflect.TypeOf(adminapi.BackupResponse{}), false, nil},
	{"UpdateDeploymentWeightRequest", reflect.TypeOf(adminapi.UpdateDeploymentWeightRequest{}), true, nil},
	{"EraseCacheEntryResponse", reflect.TypeOf(adminapi.EraseCacheEntryResponse{}), false, nil},
	{"SpendRow", reflect.TypeOf(adminapi.SpendRow{}), false, nil},
	{"SpendResponse", reflect.TypeOf(adminapi.SpendResponse{}), false, nil},
	{"SpendEraseRequest", reflect.TypeOf(adminapi.SpendEraseRequest{}), true, nil},
	{"SpendEraseResponse", reflect.TypeOf(adminapi.SpendEraseResponse{}), false, nil},
	{"VirtualKeySpendResponse", reflect.TypeOf(adminapi.VirtualKeySpendResponse{}), false, nil},
	{"VirtualKeyListEntry", reflect.TypeOf(adminapi.VirtualKeyListEntry{}), false, nil},
	{"DeploymentEntry", reflect.TypeOf(adminapi.DeploymentEntry{}), false, nil},
	{"VirtualKeyInFlightResponse", reflect.TypeOf(adminapi.VirtualKeyInFlightResponse{}), false, nil},
	{"PromptRequest", reflect.TypeOf(adminapi.PromptRequest{}), true, nil},
	{"PromptResponse", reflect.TypeOf(adminapi.PromptResponse{}), false, nil},
	{"SetPromptLabelRequest", reflect.TypeOf(adminapi.SetPromptLabelRequest{}), true, nil},
	{"LabelResponse", reflect.TypeOf(adminapi.LabelResponse{}), false, nil},
}

// jsonFields lists a struct's wire members: name and whether omitempty,
// flattening anonymous embeds and skipping json:"-".
func jsonFields(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" || !f.IsExported() {
			continue
		}
		if f.Anonymous && tag == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			for k, v := range jsonFields(ft) {
				out[k] = v
			}
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		out[name] = strings.Contains(opts, "omitempty")
	}
	return out
}

// TestOpenAPISchemasMirrorTheGoWireTypes: for every component paired above,
// the schema's properties equal the Go type's JSON members (both directions),
// and `required` follows the direction: a response requires every
// non-omitempty member (plus the stamped ones); a request requires only what
// the handler demands, which must be a subset of the members. Types with
// custom marshalling (ToolDef, ToolCall, EmbeddingsRequest.input, stop,
// tool_choice) are checked by marshalling a populated sample; a new
// custom-marshal type must be added to wireSamples by hand.
func TestOpenAPISchemasMirrorTheGoWireTypes(t *testing.T) {
	root := loadOpenAPI(t)
	schemas := mappingGet(mappingGet(root, "components"), "schemas")
	for _, wt := range wireTypes {
		s := mappingGet(schemas, wt.schema)
		if s == nil {
			t.Errorf("components.schemas.%s missing (mirrors %s)", wt.schema, wt.goType)
			continue
		}
		props := map[string]bool{}
		for _, k := range mappingKeys(mappingGet(s, "properties")) {
			props[k] = true
		}
		fields := jsonFields(wt.goType)
		for name := range fields {
			if !props[name] {
				t.Errorf("%s: Go member %q (%s) is not a property", wt.schema, name, wt.goType)
			}
		}
		for name := range props {
			if _, ok := fields[name]; !ok {
				t.Errorf("%s: property %q has no Go member on %s", wt.schema, name, wt.goType)
			}
		}
		var required []string
		if n := mappingGet(s, "required"); n != nil {
			_ = n.Decode(&required)
		}
		req := map[string]bool{}
		for _, r := range required {
			req[r] = true
			if _, ok := fields[r]; !ok {
				t.Errorf("%s: required names %q, not a Go member", wt.schema, r)
			}
		}
		if !wt.request {
			stamped := map[string]bool{}
			for _, s := range wt.stamped {
				stamped[s] = true
			}
			for name, omitempty := range fields {
				if (!omitempty || stamped[name]) && !req[name] {
					t.Errorf("%s: %q is always emitted but not required", wt.schema, name)
				}
				if omitempty && !stamped[name] && req[name] {
					t.Errorf("%s: %q is omitempty in Go but required in the document", wt.schema, name)
				}
			}
		}
	}
	// Custom-marshal types: the top-level keys of a populated sample.
	for schema, sample := range wireSamples(t) {
		s := mappingGet(schemas, schema)
		if s == nil {
			t.Errorf("components.schemas.%s missing", schema)
			continue
		}
		props := map[string]bool{}
		for _, k := range mappingKeys(mappingGet(s, "properties")) {
			props[k] = true
		}
		for k := range sample {
			if !props[k] {
				t.Errorf("%s: marshalled member %q is not a property", schema, k)
			}
		}
		for k := range props {
			if _, ok := sample[k]; !ok {
				t.Errorf("%s: property %q is never marshalled", schema, k)
			}
		}
	}
}

// wireSamples marshals fully populated custom-marshal values and returns their
// top-level JSON members per schema.
func wireSamples(t *testing.T) map[string]map[string]json.RawMessage {
	t.Helper()
	out := map[string]map[string]json.RawMessage{}
	add := func(schema string, v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", schema, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s: %v", schema, err)
		}
		out[schema] = m
	}
	add("ToolDef", adapter.ToolDef{Name: "f", Description: "d", ParametersJSON: "{}", CacheControl: &adapter.CacheControl{TTL: "1h"}, Strict: true})
	add("ToolCall", adapter.ToolCall{ID: "c", Name: "f", ArgumentsJSON: "{}"})
	add("EmbeddingsRequest", adapter.EmbeddingRequest{Model: "m", Input: []string{"a"}, Dimensions: 3})
	return out
}

// TestOpenAPIExamplesMatchTheirSources: every example that names an
// x-kelvran-example-source `<repo-relative path>[#<JSON pointer>]` is
// JSON-equal to that fixture; an example without a source is allowed only
// under text/plain or when marked hand-authored.
func TestOpenAPIExamplesMatchTheirSources(t *testing.T) {
	root := loadOpenAPI(t)
	repoRoot := filepath.Join("..", "..", "..")
	checked := 0
	var walk func(n *yaml.Node, mediaType string)
	walk = func(n *yaml.Node, mediaType string) {
		if n == nil || n.Kind != yaml.MappingNode {
			for _, c := range n.Content {
				walk(c, mediaType)
			}
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i].Value, n.Content[i+1]
			if k == "examples" && v.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(v.Content); j += 2 {
					ex := v.Content[j+1]
					src := mappingGet(ex, "x-kelvran-example-source")
					val := mappingGet(ex, "value")
					switch {
					case src == nil && mediaType == "text/plain":
					case src == nil:
						t.Errorf("kelvran-gateway.openapi.yaml:%d: example without x-kelvran-example-source", ex.Line)
					case src.Value == "hand-authored":
					default:
						checked++
						compareExample(t, repoRoot, src.Value, val, ex.Line)
					}
				}
				continue
			}
			mt := mediaType
			if strings.Contains(k, "/") {
				mt = k
			}
			walk(v, mt)
		}
	}
	walk(root, "")
	if checked == 0 {
		t.Error("no sourced example was checked; the walker lost the document")
	}
}

func compareExample(t *testing.T, repoRoot, source string, val *yaml.Node, line int) {
	t.Helper()
	file, pointer, _ := strings.Cut(source, "#")
	// A trailing [] on the pointer wraps the fixture value in a one-element
	// array: the list responses carry an array of what the golden stores once.
	wrap := strings.HasSuffix(pointer, "[]")
	pointer = strings.TrimSuffix(pointer, "[]")
	// A source names a committed fixture: repo-relative, under gateway/, never
	// absolute or above the module -- the document is the only input, but the
	// reader must not become a way to copy a runner's files into the log.
	clean := filepath.Clean(file)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") || !strings.HasPrefix(clean, "gateway/") {
		t.Errorf("kelvran-gateway.openapi.yaml:%d: example source %q must be a repo-relative path under gateway/", line, source)
		return
	}
	raw, err := os.ReadFile(filepath.Join(repoRoot, clean))
	if err != nil {
		t.Errorf("kelvran-gateway.openapi.yaml:%d: example source %s: %v", line, source, err)
		return
	}
	var fixture any
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Errorf("%s: not JSON: %v", source, err)
		return
	}
	for _, seg := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		if seg == "" {
			continue
		}
		m, ok := fixture.(map[string]any)
		if !ok {
			t.Errorf("%s: pointer segment %q into a non-object", source, seg)
			return
		}
		fixture, ok = m[strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")]
		if !ok {
			t.Errorf("%s: pointer segment %q not found", source, seg)
			return
		}
	}
	if wrap {
		fixture = []any{fixture}
	}
	var example any
	if val == nil {
		t.Errorf("kelvran-gateway.openapi.yaml:%d: example has no value", line)
		return
	}
	if err := val.Decode(&example); err != nil {
		t.Errorf("kelvran-gateway.openapi.yaml:%d: %v", line, err)
		return
	}
	want, _ := json.Marshal(normalizeJSON(fixture))
	got, _ := json.Marshal(normalizeJSON(example))
	if !bytes.Equal(want, got) {
		t.Errorf("kelvran-gateway.openapi.yaml:%d: example differs from %s (%d bytes in the document, %d in the fixture; first difference at byte %d)", line, source, len(got), len(want), firstDifference(got, want))
	}
}

// normalizeJSON makes YAML-decoded and JSON-decoded values comparable:
// map[string]any keys sorted by json.Marshal, ints and floats unified.
func normalizeJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = normalizeJSON(vv)
		}
		return out
	case []any:
		for i := range x {
			x[i] = normalizeJSON(x[i])
		}
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	default:
		return v
	}
}

var _ = sort.Strings

// firstDifference is the index of the first differing byte, or the shorter length.
func firstDifference(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}
