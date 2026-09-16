package adapter

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func b64(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

var realPNGSignature = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

func TestValidateContentPartsAcceptsTextOnlyMessages(t *testing.T) {
	messages := []Message{{Role: "user", Content: "hello"}}
	if err := ValidateContentParts(messages); err != nil {
		t.Fatalf("ValidateContentParts() = %v, want nil", err)
	}
}

func TestValidateContentPartsAcceptsMatchingImage(t *testing.T) {
	messages := []Message{{
		Role: "user",
		Parts: []ContentPart{
			{Type: "image", MediaType: "image/png", Data: b64(realPNGSignature)},
		},
	}}
	if err := ValidateContentParts(messages); err != nil {
		t.Fatalf("ValidateContentParts() = %v, want nil", err)
	}
}

func TestValidateContentPartsRejectsDeclaredImageButDetectedText(t *testing.T) {
	messages := []Message{{
		Role: "user",
		Parts: []ContentPart{
			{Type: "image", MediaType: "image/png", Data: b64([]byte("this is plain text, not an image"))},
		},
	}}
	err := ValidateContentParts(messages)
	if !errors.Is(err, ErrContentPartMIMEMismatch) {
		t.Fatalf("ValidateContentParts() = %v, want ErrContentPartMIMEMismatch", err)
	}
}

func TestValidateContentPartsSkipsURLReferencedParts(t *testing.T) {
	messages := []Message{{
		Role: "user",
		Parts: []ContentPart{
			{Type: "image", MediaType: "image/png", URL: "https://example.com/cat.png"},
		},
	}}
	if err := ValidateContentParts(messages); err != nil {
		t.Fatalf("ValidateContentParts() = %v, want nil (URL parts have no inline bytes to sniff)", err)
	}
}

func TestValidateContentPartsTreatsUnidentifiableContentAsInconclusive(t *testing.T) {
	// Random, high-entropy bytes that DetectContentType cannot classify
	// fall back to "application/octet-stream" — inconclusive, not a
	// mismatch, per this function's own documented failure-mode choice.
	messages := []Message{{
		Role: "user",
		Parts: []ContentPart{
			{Type: "document", MediaType: "application/pdf", Data: b64([]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0xFF, 0xFE, 0xFD})},
		},
	}}
	if err := ValidateContentParts(messages); err != nil {
		t.Fatalf("ValidateContentParts() = %v, want nil (undetectable content is inconclusive)", err)
	}
}

func TestValidateContentPartsRejectsInvalidBase64(t *testing.T) {
	messages := []Message{{
		Role: "user",
		Parts: []ContentPart{
			{Type: "image", MediaType: "image/png", Data: "not-valid-base64!!!"},
		},
	}}
	if err := ValidateContentParts(messages); err == nil {
		t.Fatal("ValidateContentParts() = nil, want an error for invalid base64")
	}
}

// deeplyNestedObjectSchema builds a JSON document nested exactly depth
// levels deep (depth counts "{" opens), e.g. depth=2 produces
// `{"a":{"a":1}}`.
func deeplyNestedObjectSchema(depth int) json.RawMessage {
	var b strings.Builder
	for i := 0; i < depth; i++ {
		b.WriteString(`{"a":`)
	}
	b.WriteString("1")
	for i := 0; i < depth; i++ {
		b.WriteString("}")
	}
	return json.RawMessage(b.String())
}

// manyFlatPropertiesSchema builds a single, shallow (depth-1) JSON object
// with count string-valued properties -- many tokens, no meaningful
// nesting.
func manyFlatPropertiesSchema(count int) json.RawMessage {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < count; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"p%d":"v"`, i)
	}
	b.WriteString("}")
	return json.RawMessage(b.String())
}

func responseFormatWithSchema(schema json.RawMessage) *ResponseFormat {
	return &ResponseFormat{
		Type:       "json_schema",
		JSONSchema: &JSONSchema{Name: "test_schema", Schema: schema},
	}
}

func TestValidateResponseFormatSchemaAcceptsSchemaWellWithinBounds(t *testing.T) {
	rf := responseFormatWithSchema(json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`))
	if err := ValidateResponseFormatSchema(rf); err != nil {
		t.Fatalf("ValidateResponseFormatSchema() = %v, want nil", err)
	}
}

func TestValidateResponseFormatSchemaAcceptsSchemaNestedExactlyAtDepthLimit(t *testing.T) {
	rf := responseFormatWithSchema(deeplyNestedObjectSchema(maxJSONSchemaDepth))
	if err := ValidateResponseFormatSchema(rf); err != nil {
		t.Fatalf("ValidateResponseFormatSchema() = %v, want nil for a schema nested exactly at the %d-level depth limit", err, maxJSONSchemaDepth)
	}
}

func TestValidateResponseFormatSchemaRejectsSchemaNestedOneLevelBeyondDepthLimit(t *testing.T) {
	rf := responseFormatWithSchema(deeplyNestedObjectSchema(maxJSONSchemaDepth + 1))
	err := ValidateResponseFormatSchema(rf)
	if !errors.Is(err, ErrResponseFormatSchemaTooComplex) {
		t.Fatalf("ValidateResponseFormatSchema() = %v, want ErrResponseFormatSchemaTooComplex for a schema nested one level beyond the %d-level depth limit", err, maxJSONSchemaDepth)
	}
}

func TestValidateResponseFormatSchemaRejectsSchemaWithTooManyTokens(t *testing.T) {
	// Each property contributes 2 tokens (key + value); 6000 properties
	// (12002 tokens including the surrounding braces) exceeds
	// maxJSONSchemaTokens while staying at depth 1, well under
	// maxJSONSchemaDepth -- proving this is genuinely the token bound
	// rejecting, not the depth bound.
	rf := responseFormatWithSchema(manyFlatPropertiesSchema(6000))
	err := ValidateResponseFormatSchema(rf)
	if !errors.Is(err, ErrResponseFormatSchemaTooComplex) {
		t.Fatalf("ValidateResponseFormatSchema() = %v, want ErrResponseFormatSchemaTooComplex for a schema exceeding %d tokens", err, maxJSONSchemaTokens)
	}
}

func TestValidateResponseFormatSchemaNoopForNilResponseFormat(t *testing.T) {
	if err := ValidateResponseFormatSchema(nil); err != nil {
		t.Fatalf("ValidateResponseFormatSchema(nil) = %v, want nil", err)
	}
}

func TestValidateResponseFormatSchemaNoopForNilJSONSchema(t *testing.T) {
	rf := &ResponseFormat{Type: "json_schema"}
	if err := ValidateResponseFormatSchema(rf); err != nil {
		t.Fatalf("ValidateResponseFormatSchema() = %v, want nil for a nil JSONSchema", err)
	}
}

func TestValidateResponseFormatSchemaNoopForEmptySchema(t *testing.T) {
	rf := &ResponseFormat{Type: "json_schema", JSONSchema: &JSONSchema{Name: "empty"}}
	if err := ValidateResponseFormatSchema(rf); err != nil {
		t.Fatalf("ValidateResponseFormatSchema() = %v, want nil for an empty Schema", err)
	}
}

func TestValidateResponseFormatSchemaRejectsMalformedJSON(t *testing.T) {
	rf := responseFormatWithSchema(json.RawMessage(`{"type": not valid json`))
	err := ValidateResponseFormatSchema(rf)
	if err == nil {
		t.Fatal("ValidateResponseFormatSchema() = nil, want a real error for malformed JSON")
	}
	if errors.Is(err, ErrResponseFormatSchemaTooComplex) {
		t.Fatalf("ValidateResponseFormatSchema() = %v, want a JSON-parse error, not ErrResponseFormatSchemaTooComplex", err)
	}
}

func manyMessages(count int) []Message {
	messages := make([]Message, count)
	for i := range messages {
		messages[i] = Message{Role: "user", Content: "hi"}
	}
	return messages
}

func TestValidateMessageCountAcceptsCountWellWithinBounds(t *testing.T) {
	if err := ValidateMessageCount(manyMessages(3)); err != nil {
		t.Fatalf("ValidateMessageCount() = %v, want nil", err)
	}
}

func TestValidateMessageCountAcceptsExactlyAtLimit(t *testing.T) {
	if err := ValidateMessageCount(manyMessages(maxMessagesPerRequest)); err != nil {
		t.Fatalf("ValidateMessageCount() = %v, want nil for exactly %d messages", err, maxMessagesPerRequest)
	}
}

func TestValidateMessageCountRejectsOneBeyondLimit(t *testing.T) {
	err := ValidateMessageCount(manyMessages(maxMessagesPerRequest + 1))
	if !errors.Is(err, ErrTooManyMessages) {
		t.Fatalf("ValidateMessageCount() = %v, want ErrTooManyMessages for %d messages", err, maxMessagesPerRequest+1)
	}
}

func TestValidateMessageCountAcceptsNilMessages(t *testing.T) {
	if err := ValidateMessageCount(nil); err != nil {
		t.Fatalf("ValidateMessageCount(nil) = %v, want nil", err)
	}
}

func manyToolDefs(count int) []ToolDef {
	tools := make([]ToolDef, count)
	for i := range tools {
		tools[i] = ToolDef{Name: fmt.Sprintf("tool_%d", i)}
	}
	return tools
}

func TestValidateToolDefsAcceptsCountWellWithinBounds(t *testing.T) {
	if err := ValidateToolDefs(manyToolDefs(3)); err != nil {
		t.Fatalf("ValidateToolDefs() = %v, want nil", err)
	}
}

func TestValidateToolDefsAcceptsExactlyAtCountLimit(t *testing.T) {
	if err := ValidateToolDefs(manyToolDefs(maxToolDefsPerRequest)); err != nil {
		t.Fatalf("ValidateToolDefs() = %v, want nil for exactly %d tools", err, maxToolDefsPerRequest)
	}
}

func TestValidateToolDefsRejectsOneBeyondCountLimit(t *testing.T) {
	err := ValidateToolDefs(manyToolDefs(maxToolDefsPerRequest + 1))
	if !errors.Is(err, ErrTooManyToolDefs) {
		t.Fatalf("ValidateToolDefs() = %v, want ErrTooManyToolDefs for %d tools", err, maxToolDefsPerRequest+1)
	}
}

func TestValidateToolDefsAcceptsNilTools(t *testing.T) {
	if err := ValidateToolDefs(nil); err != nil {
		t.Fatalf("ValidateToolDefs(nil) = %v, want nil", err)
	}
}

func TestValidateToolDefsRejectsAToolWithParametersNestedBeyondDepthLimit(t *testing.T) {
	tools := []ToolDef{{Name: "deep", ParametersJSON: string(deeplyNestedObjectSchema(maxJSONSchemaDepth + 1))}}
	err := ValidateToolDefs(tools)
	if !errors.Is(err, ErrResponseFormatSchemaTooComplex) {
		t.Fatalf("ValidateToolDefs() = %v, want ErrResponseFormatSchemaTooComplex for a tool nested one level beyond the %d-level depth limit", err, maxJSONSchemaDepth)
	}
}

func TestValidateToolDefsRejectsAToolWithTooManyParameterTokens(t *testing.T) {
	tools := []ToolDef{{Name: "wide", ParametersJSON: string(manyFlatPropertiesSchema(6000))}}
	err := ValidateToolDefs(tools)
	if !errors.Is(err, ErrResponseFormatSchemaTooComplex) {
		t.Fatalf("ValidateToolDefs() = %v, want ErrResponseFormatSchemaTooComplex for a tool exceeding %d tokens", err, maxJSONSchemaTokens)
	}
}

func TestValidateToolDefsAcceptsEmptyParametersJSON(t *testing.T) {
	tools := []ToolDef{{Name: "no-params"}}
	if err := ValidateToolDefs(tools); err != nil {
		t.Fatalf("ValidateToolDefs() = %v, want nil for a tool with empty ParametersJSON", err)
	}
}
