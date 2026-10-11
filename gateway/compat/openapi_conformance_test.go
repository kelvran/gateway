package compat

// OpenAPI conformance (plan item 14 slice S5): the document at
// gateway/openapi/kelvran-gateway.openapi.yaml validated against the embedded
// OpenAPI 3.1 meta-schema, the committed fixtures validated against the
// components they document, and the gateway TestMain already started probed
// live with its real data-plane responses validated against the document.
// The validator lives in this module on purpose: its dependency graph never
// enters gateway/go.mod (the structural contract tests there parse the YAML
// with yaml/v3 alone).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
	verrors "github.com/pb33f/libopenapi-validator/errors"
	"github.com/pb33f/libopenapi-validator/schema_validation"
	"github.com/pb33f/libopenapi/datamodel/high/base"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"
)

const (
	openapiDocumentPath = "../openapi/kelvran-gateway.openapi.yaml"
	liveCoverageLedger  = "testdata/openapi_live_coverage.txt"
)

// loadDocument parses the document and builds its v3 model. A dangling or a
// file $ref fails the build here: with the default configuration (no base
// path, file and remote lookups off) the reference cannot be located and the
// resolver reports it missing, so the document is single-file by
// construction.
func loadDocument(t *testing.T) (libopenapi.Document, *v3.Document) {
	t.Helper()
	raw, err := os.ReadFile(openapiDocumentPath)
	if err != nil {
		t.Fatalf("the OpenAPI document is missing: %v", err)
	}
	doc, err := libopenapi.NewDocument(raw)
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	model, err := doc.BuildV3Model()
	if err != nil {
		t.Fatalf("BuildV3Model: %v", err)
	}
	return doc, &model.Model
}

// TestOpenAPIDocumentIsValid31: the document is OpenAPI 3.1 and passes the
// embedded meta-schema. What this proves is structure and reference
// resolution; Schema-keyword spelling is pinned Go-side in cmd/gateway,
// because the 3.1 meta-schema leaves Schema Objects open.
func TestOpenAPIDocumentIsValid31(t *testing.T) {
	doc, _ := loadDocument(t)
	if info := doc.GetSpecInfo(); info == nil || info.VersionNumeric < 3.1 || info.VersionNumeric >= 3.2 {
		t.Fatalf("spec version = %v, want 3.1", doc.GetVersion())
	}
	v, errs := validator.NewValidator(doc)
	if len(errs) != 0 {
		t.Fatalf("NewValidator: %v", errs)
	}
	ok, verrs := v.ValidateDocument()
	for _, e := range verrs {
		t.Errorf("%s (line %d col %d): %s — %s", e.ValidationType, e.SpecLine, e.SpecCol, e.Message, e.HowToFix)
	}
	if !ok {
		t.Fatal("the document does not pass the OpenAPI 3.1 meta-schema")
	}
}

// schemaOf returns a named component schema.
func schemaOf(t *testing.T, model *v3.Document, name string) *base.Schema {
	t.Helper()
	proxy := model.Components.Schemas.GetOrZero(name)
	if proxy == nil {
		t.Fatalf("components.schemas.%s missing", name)
	}
	s := proxy.Schema()
	if s == nil {
		t.Fatalf("components.schemas.%s: %v", name, proxy.GetBuildError())
	}
	return s
}

// validateBytes fails the test when payload does not conform to schema.
func validateBytes(t *testing.T, what string, schema *base.Schema, payload []byte) {
	t.Helper()
	ok, errs := schema_validation.NewSchemaValidator().ValidateSchemaBytesWithVersion(schema, payload, 3.1)
	if ok {
		return
	}
	for _, e := range errs {
		msg := e.Message
		for _, se := range e.SchemaValidationErrors {
			loc := se.FieldPath
			if loc == "" {
				loc = strings.Join(se.InstancePath, "/")
			}
			msg += "; " + loc + ": " + se.Reason
		}
		t.Errorf("%s does not conform: %s", what, msg)
	}
}

// TestOpenAPIFixturesConformToTheirSchemas: every fixture the document names
// as an example source, plus the tool-use response fixture and the two SSE
// goldens the live stream does not replay (the ingress golden and the mock's
// thinking+tool stream; the mock's text stream is validated frame by frame
// live), validates against the component that documents it. The admin
// golden is byte-pinned by adminapi's TestWireShapesMatchGolden (every wire
// type marshalled; the handlers use those types by alias and encode them with
// encoding/json), so this is a transitive proof for every admin JSON body
// those types carry — GET /admin/config's untyped config struct and the
// plain-text errors excepted — without an admin listener.
func TestOpenAPIFixturesConformToTheirSchemas(t *testing.T) {
	_, model := loadDocument(t)
	golden := map[string]json.RawMessage{}
	raw, err := os.ReadFile("../internal/adminapi/testdata/wire.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden) < 20 {
		t.Fatalf("wire golden has %d entries; the fixture moved", len(golden))
	}
	keys := make([]string, 0, len(golden))
	for key := range golden {
		keys = append(keys, key)
	}
	sort.Strings(keys) // deterministic failure order
	for _, key := range keys {
		component, _, _ := strings.Cut(key, ".")
		validateBytes(t, "wire.golden.json#/"+key, schemaOf(t, model, component), golden[key])
	}
	for file, component := range map[string]string{
		"../internal/adapter/openai/testdata/request_canonical.json":               "ChatCompletionRequest",
		"../internal/ingress/anthropicmsgs/testdata/request_claude_code_turn.json": "AnthropicMessagesRequest",
		"testdata/anthropic_response_text.json":                                    "AnthropicMessage",
		"testdata/anthropic_response_tool_use_thinking.json":                       "AnthropicMessage",
	} {
		body, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			t.Fatal(err)
		}
		validateBytes(t, file, schemaOf(t, model, component), body)
	}
	for _, sse := range []string{"../internal/ingress/anthropicmsgs/testdata/sse_text_tool_thinking.golden.txt", "testdata/anthropic_stream_thinking_tool.txt"} {
		for i, frame := range sseDataFrames(t, sse) {
			validateBytes(t, fmt.Sprintf("%s frame %d", filepath.Base(sse), i+1), schemaOf(t, model, "AnthropicStreamEvent"), frame)
		}
	}
}

// sseDataFrames returns every `data:` payload of an SSE transcript.
func sseDataFrames(t *testing.T, file string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatal(err)
	}
	frames, err := dataFrames(raw)
	if err != nil || len(frames) == 0 {
		t.Fatalf("%s: %d data frames, scanner error %v", file, len(frames), err)
	}
	return frames
}

// dataFrames returns every `data:` payload of an SSE transcript and the
// scanner's error, if a line exceeded its 8 MiB buffer or the read failed.
func dataFrames(raw []byte) ([][]byte, error) {
	var frames [][]byte
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		if after, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			frames = append(frames, []byte(after))
		}
	}
	return frames, sc.Err()
}

// liveProbe is one request against the gateway TestMain started and the
// operationId the document gives its route.
type liveProbe struct {
	operationID, method, path, body string
	headers                         map[string]string
	wantStatus                      int
	authenticated                   bool // true: ValidateHttpRequestResponse (request security validation on); false: ValidateHttpResponse only
}

// TestOpenAPILiveResponsesConform drives the started gateway's data plane and
// validates every buffered answer against the document and both streaming
// routes' frames by hand against their event components (the validator skips
// non-JSON bodies); the observed (operationId, status) pairs must equal the
// committed ledger, so live coverage can only shrink deliberately. The
// wrong-method 405s stay with cmd/gateway's TestOpenAPI405ClaimsMatchTheMux.
// The keyless 401 legs use ValidateHttpResponse: request security validation
// is on by default, so ValidateHttpRequestResponse reports false on the
// missing credential whatever the 401 body looks like.
func TestOpenAPILiveResponsesConform(t *testing.T) {
	doc, model := loadDocument(t)
	v, errs := validator.NewValidator(doc)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	client := &http.Client{Timeout: 30 * time.Second} // a stalled gateway fails the probe, not the job
	bearer := map[string]string{"Authorization": "Bearer " + gatewayCredential, "Content-Type": "application/json"}
	chat := `{"model":"` + modelAnthropic + `","messages":[{"role":"user","content":"` + question + ` [conformance]"}]}`
	messages := `{"model":"` + modelAnthropic + `","max_tokens":64,"messages":[{"role":"user","content":"` + question + ` [conformance messages]"}]}`
	probes := []liveProbe{
		{"getHealthz", http.MethodGet, "/healthz", "", nil, 200, false},
		{"getReadyz", http.MethodGet, "/readyz", "", nil, 200, false}, // waitReady saw 200 before any test ran
		{"probeHello", http.MethodHead, "/api/hello", "", nil, 204, false},
		{"listModels", http.MethodGet, "/v1/models", "", bearer, 200, true},
		{"createChatCompletion", http.MethodPost, "/v1/chat/completions", chat, bearer, 200, true},
		{"createChatCompletion", http.MethodPost, "/v1/chat/completions", chat, map[string]string{"Content-Type": "application/json"}, 401, false},
		{"createChatCompletion", http.MethodPost, "/v1/chat/completions", `{"model":"no-such-model","messages":[{"role":"user","content":"x"}]}`, bearer, 400, true},
		{"createEmbedding", http.MethodPost, "/v1/embeddings", `{"model":"` + modelAnthropic + `","input":"x"}`, bearer, 400, true},
		{"countTokens", http.MethodPost, "/v1/messages/count_tokens", `{"model":"no-such-model","messages":[{"role":"user","content":"x"}]}`, bearer, 400, true},
		{"createMessage", http.MethodPost, "/v1/messages", messages, bearer, 200, true},
		{"createMessage", http.MethodPost, "/v1/messages", messages, map[string]string{"Content-Type": "application/json"}, 401, false},
		{"countTokens", http.MethodPost, "/v1/messages/count_tokens", `{"model":"` + modelAnthropic + `","messages":[{"role":"user","content":"x"}]}`, bearer, 200, true},
	}
	observed := map[string]bool{}
	for _, p := range probes {
		req, err := http.NewRequest(p.method, gatewayURL+p.path, strings.NewReader(p.body))
		if err != nil {
			t.Fatal(err)
		}
		for k, val := range p.headers {
			req.Header.Set(k, val)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", p.method, p.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(body))
		if p.wantStatus != 0 && resp.StatusCode != p.wantStatus {
			t.Errorf("%s %s: %d, want %d: %s", p.method, p.path, resp.StatusCode, p.wantStatus, body)
			continue
		}
		observed[fmt.Sprintf("%s %d", p.operationID, resp.StatusCode)] = true
		var ok bool
		var verrs []*verrors.ValidationError
		// ValidateHttpResponse validates the response alone (status, headers,
		// body against the operation); ValidateHttpRequestResponse validates both
		// and would report false on the keyless request's security requirement
		// whatever the 401 body looks like.
		if p.authenticated {
			ok, verrs = v.ValidateHttpRequestResponse(req, resp)
		} else {
			ok, verrs = v.ValidateHttpResponse(req, resp)
		}
		if !ok {
			for _, e := range verrs {
				t.Errorf("%s %s → %d does not conform: %s — %s", p.method, p.path, resp.StatusCode, e.Message, e.HowToFix)
			}
		}
	}
	// Streams: the frames by hand (the validator skips non-JSON bodies).
	for _, s := range []struct{ operationID, path, body, component, first, last string }{
		{"createChatCompletion", "/v1/chat/completions", strings.Replace(chat, `"model"`, `"stream":true,"model"`, 1), "ChatCompletionChunk", "", "[DONE]"},
		{"createMessage", "/v1/messages", strings.Replace(messages, `"model"`, `"stream":true,"model"`, 1), "AnthropicStreamEvent", "message_start", "message_stop"},
	} {
		req, err := http.NewRequest(http.MethodPost, gatewayURL+s.path, strings.NewReader(s.body))
		if err != nil {
			t.Fatal(err)
		}
		for k, val := range bearer {
			req.Header.Set(k, val)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Errorf("stream %s: %d %s: %s", s.path, resp.StatusCode, resp.Header.Get("Content-Type"), raw)
			continue
		}
		frames, err := dataFrames(raw)
		if err != nil {
			t.Errorf("stream %s: %v", s.path, err)
			continue
		}
		if len(frames) < 3 {
			t.Errorf("stream %s: only %d frames", s.path, len(frames))
			continue
		}
		observed[s.operationID+" 200 stream"] = true
		schema := schemaOf(t, model, s.component)
		for i, f := range frames {
			if string(f) == "[DONE]" {
				continue
			}
			validateBytes(t, fmt.Sprintf("stream %s frame %d", s.path, i+1), schema, f)
		}
		if s.last == "[DONE]" && string(frames[len(frames)-1]) != "[DONE]" {
			t.Errorf("stream %s: last frame %q, want [DONE]", s.path, frames[len(frames)-1])
		}
		if s.first != "" && !bytes.Contains(frames[0], []byte(`"type":"`+s.first+`"`)) {
			t.Errorf("stream %s: first frame is not %s: %s", s.path, s.first, frames[0])
		}
		if s.last != "[DONE]" && !bytes.Contains(frames[len(frames)-1], []byte(`"type":"`+s.last+`"`)) {
			t.Errorf("stream %s: last frame is not %s: %s", s.path, s.last, frames[len(frames)-1])
		}
	}
	// The ledger: observed pairs == committed pairs, byte for byte.
	keys := make([]string, 0, len(observed))
	for k := range observed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	got := strings.Join(keys, "\n") + "\n"
	want, err := os.ReadFile(liveCoverageLedger)
	if err != nil {
		t.Fatalf("%s: %v (observed:\n%s)", liveCoverageLedger, err, got)
	}
	if string(want) != got {
		t.Errorf("live coverage changed; update %s deliberately.\n got:\n%s want:\n%s", liveCoverageLedger, got, want)
	}
}
