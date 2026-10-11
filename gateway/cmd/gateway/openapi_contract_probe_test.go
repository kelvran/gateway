package main

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// methodNotAllowedClaim is a response's x-kelvran-method-not-allowed block.
type methodNotAllowedClaim struct {
	Status      int     `yaml:"status"`
	Allow       *string `yaml:"allow"`
	ContentType string  `yaml:"content_type"`
	Body        string  `yaml:"body"`
}

// TestOpenAPI405ClaimsMatchTheMux: every data-plane operation declares a 405
// whose x-kelvran-method-not-allowed block is re-executed here against the
// real mux (a wrong-method request through the recorder) and compared on
// status, Allow and Content-Type; no admin operation declares a 405 (that one
// is net/http's, before auth, stated at the root); every operation declares
// at least one 4xx.
func TestOpenAPI405ClaimsMatchTheMux(t *testing.T) {
	root := loadOpenAPI(t)
	mux := newDataPlaneMux(nil) // every handler checks the method before the pipeline
	for _, op := range documentOperations(t, root) {
		responses := mappingGet(op.node, "responses")
		has4xx := false
		for _, k := range mappingKeys(responses) {
			if strings.HasPrefix(k, "4") {
				has4xx = true
			}
		}
		if !has4xx {
			t.Errorf("%s (yaml:%d): no 4xx response declared", op.key(), op.line)
		}
		r405 := mappingGet(responses, "405")
		if strings.HasPrefix(op.path, "/admin/") {
			if r405 != nil {
				t.Errorf("%s (yaml:%d): a 405 on an admin operation; net/http answers those before auth, see x-kelvran-servemux", op.key(), op.line)
			}
			continue
		}
		if r405 == nil {
			t.Errorf("%s (yaml:%d): no 405 declared", op.key(), op.line)
			continue
		}
		var claim methodNotAllowedClaim
		if n := mappingGet(r405, "x-kelvran-method-not-allowed"); n == nil {
			t.Errorf("%s 405: no x-kelvran-method-not-allowed block", op.key())
			continue
		} else if err := n.Decode(&claim); err != nil {
			t.Fatalf("%s 405: %v", op.key(), err)
		}
		wrong := http.MethodPatch // never a data-plane method, so always the handler's own 405
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(wrong, op.path, nil))
		if rec.Code != claim.Status || rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: the mux answers %d to %s, the document claims %d", op.key(), rec.Code, wrong, claim.Status)
		}
		gotAllow := rec.Header().Get("Allow")
		switch {
		case claim.Allow == nil && gotAllow != "":
			t.Errorf("%s: the document claims no Allow header, the mux sends %q", op.key(), gotAllow)
		case claim.Allow != nil && gotAllow != *claim.Allow:
			t.Errorf("%s: Allow %q, the document claims %q", op.key(), gotAllow, *claim.Allow)
		case claim.Allow != nil && *claim.Allow != op.method:
			t.Errorf("%s: the document claims Allow %q but the operation is %s", op.key(), *claim.Allow, op.method)
		}
		if ct := rec.Header().Get("Content-Type"); ct != claim.ContentType {
			t.Errorf("%s: Content-Type %q, the document claims %q", op.key(), ct, claim.ContentType)
		}
		wantBody := map[string]string{"application/json; charset=utf-8": "", "text/plain; charset=utf-8": "text"}[claim.ContentType]
		if claim.ContentType == "application/json; charset=utf-8" {
			if strings.Contains(rec.Body.String(), `"type":"error"`) {
				wantBody = "anthropic-envelope"
			} else {
				wantBody = "openai-envelope"
			}
		}
		if claim.Body != wantBody {
			t.Errorf("%s: body claim %q, the mux wrote a %s", op.key(), claim.Body, wantBody)
		}
	}
}

// schemaKeywordAllowlist is the closed vocabulary a Schema Object may use:
// JSON Schema 2020-12 core, applicator, validation, meta-data, format,
// content and unevaluated vocabularies, the OAS 3.1 Schema Object additions,
// and this document's extensions. `nullable` (3.0) and the singular `example`
// (deprecated in 3.1) are deliberately absent, so one spelling survives.
var schemaKeywordAllowlist = map[string]bool{
	"$schema": true, "$id": true, "$ref": true, "$defs": true, "$comment": true, "$anchor": true, "$dynamicRef": true, "$dynamicAnchor": true,
	"type": true, "enum": true, "const": true, "multipleOf": true, "maximum": true, "exclusiveMaximum": true, "minimum": true, "exclusiveMinimum": true,
	"maxLength": true, "minLength": true, "pattern": true, "maxItems": true, "minItems": true, "uniqueItems": true, "maxContains": true, "minContains": true,
	"maxProperties": true, "minProperties": true, "required": true, "dependentRequired": true, "properties": true, "patternProperties": true,
	"additionalProperties": true, "propertyNames": true, "items": true, "prefixItems": true, "contains": true, "allOf": true, "anyOf": true, "oneOf": true,
	"not": true, "if": true, "then": true, "else": true, "dependentSchemas": true, "unevaluatedItems": true, "unevaluatedProperties": true,
	"title": true, "description": true, "default": true, "deprecated": true, "readOnly": true, "writeOnly": true, "examples": true,
	"format": true, "contentMediaType": true, "contentEncoding": true, "contentSchema": true,
	"discriminator": true, "xml": true, "externalDocs": true,
}

// schemaSubschemaMaps are keywords whose value is a map of name -> schema.
var schemaSubschemaMaps = map[string]bool{"properties": true, "patternProperties": true, "$defs": true, "dependentSchemas": true}

// schemaSubschemaLists are keywords whose value is a list of schemas.
var schemaSubschemaLists = map[string]bool{"allOf": true, "anyOf": true, "oneOf": true, "prefixItems": true}

// schemaSubschemaSingles are keywords whose value is one schema.
var schemaSubschemaSingles = map[string]bool{"items": true, "contains": true, "additionalProperties": true, "propertyNames": true, "not": true, "if": true, "then": true, "else": true, "unevaluatedItems": true, "unevaluatedProperties": true, "contentSchema": true}

// walkSchema visits one Schema Object and everything it nests.
func walkSchema(n *yaml.Node, visit func(schema *yaml.Node)) {
	if n == nil || n.Kind != yaml.MappingNode {
		return
	}
	visit(n)
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		switch {
		case schemaSubschemaMaps[k] && v.Kind == yaml.MappingNode:
			for j := 0; j+1 < len(v.Content); j += 2 {
				walkSchema(v.Content[j+1], visit)
			}
		case schemaSubschemaLists[k] && v.Kind == yaml.SequenceNode:
			for _, c := range v.Content {
				walkSchema(c, visit)
			}
		case schemaSubschemaSingles[k] && v.Kind == yaml.MappingNode:
			walkSchema(v, visit)
		}
	}
}

// everySchemaObject finds the Schema Objects in the document: each entry of
// components.schemas and every `schema:` under parameters, headers, request
// bodies and responses, anywhere.
func everySchemaObject(root *yaml.Node) []*yaml.Node {
	var roots []*yaml.Node
	schemas := mappingGet(mappingGet(root, "components"), "schemas")
	if schemas == nil {
		return nil // no components.schemas: the keyword test then fails its minimum count
	}
	for i := 0; i+1 < len(schemas.Content); i += 2 {
		roots = append(roots, schemas.Content[i+1])
	}
	var find func(n *yaml.Node, insideSchemas bool)
	find = func(n *yaml.Node, insideSchemas bool) {
		if n == nil {
			return
		}
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i].Value, n.Content[i+1]
				if k == "schema" && !insideSchemas && v.Kind == yaml.MappingNode {
					roots = append(roots, v)
					continue
				}
				find(v, insideSchemas || k == "schemas")
			}
			return
		}
		for _, c := range n.Content {
			find(c, insideSchemas)
		}
	}
	find(root, false)
	return roots
}

// TestOpenAPISchemaObjectsUseOnlyKnownKeywords closes the gap the OAS 3.1
// meta-schema leaves (Schema Objects are open under 2020-12, so `nullable`,
// a singular `example` or a typo such as `requried` pass validation): every
// key of every Schema Object must be in the allowlist or an x-kelvran-*
// extension, and `required` must name declared properties unless the schema
// is marked permissive.
func TestOpenAPISchemaObjectsUseOnlyKnownKeywords(t *testing.T) {
	root := loadOpenAPI(t)
	seen := 0
	for _, s := range everySchemaObject(root) {
		walkSchema(s, func(schema *yaml.Node) {
			seen++
			props := map[string]bool{}
			if p := mappingGet(schema, "properties"); p != nil {
				for _, k := range mappingKeys(p) {
					props[k] = true
				}
			}
			permissive := mappingGet(schema, "x-kelvran-passthrough") != nil || (mappingGet(schema, "additionalProperties") != nil && mappingGet(schema, "additionalProperties").Value == "true")
			for i := 0; i+1 < len(schema.Content); i += 2 {
				k := schema.Content[i]
				if !schemaKeywordAllowlist[k.Value] && !strings.HasPrefix(k.Value, "x-kelvran-") {
					t.Errorf("kelvran-gateway.openapi.yaml:%d: schema keyword %q is not in the allowlist (nullable and example are deliberately absent)", k.Line, k.Value)
				}
			}
			if req := mappingGet(schema, "required"); req != nil && !permissive && len(props) > 0 {
				var required []string
				_ = req.Decode(&required)
				for _, r := range required {
					if !props[r] {
						t.Errorf("kelvran-gateway.openapi.yaml:%d: required names %q, which properties does not declare", req.Line, r)
					}
				}
			}
		})
	}
	if seen < 100 {
		t.Errorf("walked only %d Schema Objects; the walker lost the document", seen)
	}
	keys := make([]string, 0, len(schemaKeywordAllowlist))
	for k := range schemaKeywordAllowlist {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("%d Schema Objects checked against %d keywords", seen, len(keys))
}
