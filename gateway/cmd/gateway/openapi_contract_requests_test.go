package main

// The request-side openness rule (item 14 slice S3, contract review finding
// 2): the gateway decodes every request body with encoding/json and never
// asks a decoder to reject unknown members, so they are ignored at every
// depth. A request-reachable Schema Object closed with
// `additionalProperties: false` would make a validator or a generated client
// reject bodies the gateway accepts (an OpenAI SDK's messages[].name, an
// Anthropic-form cache_control.type). Both halves are pinned here, each with
// a positive control: the document's request schemas are open, and the code
// has no strict decoder.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// requestSchemaRoots returns every `schema:` under a requestBody keyed by the
// operation it belongs to, plus every component named *Request (the admin
// bodies S4 wires into their operations are already components).
func requestSchemaRoots(t *testing.T, root *yaml.Node) map[*yaml.Node]string {
	t.Helper()
	out := map[*yaml.Node]string{}
	for _, op := range documentOperations(t, root) {
		body := mappingGet(op.node, "requestBody")
		if body == nil {
			continue
		}
		content := mappingGet(body, "content")
		if content == nil {
			continue
		}
		for i := 0; i+1 < len(content.Content); i += 2 {
			if s := mappingGet(content.Content[i+1], "schema"); s != nil {
				out[s] = op.key()
			}
		}
	}
	schemas := mappingGet(mappingGet(root, "components"), "schemas")
	for _, name := range mappingKeys(schemas) {
		if strings.HasSuffix(name, "Request") {
			out[mappingGet(schemas, name)] = "components.schemas." + name
		}
	}
	return out
}

// walkRequestSchema descends a Schema Object the way walkSchema does, resolves
// a YAML alias to its anchor, follows `$ref` into components.schemas once per
// component and reports a `$ref` that names nothing. visit receives the
// component's name for everything inside it ("" for an inline schema).
func walkRequestSchema(t *testing.T, schemas, n *yaml.Node, seen map[string]bool, visit func(name string, schema *yaml.Node)) {
	t.Helper()
	if n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n == nil || n.Kind != yaml.MappingNode {
		return
	}
	if ref := mappingGet(n, "$ref"); ref != nil {
		const prefix = "#/components/schemas/"
		if !strings.HasPrefix(ref.Value, prefix) {
			t.Errorf("kelvran-gateway.openapi.yaml:%d: $ref %q is not a component schema", ref.Line, ref.Value)
			return
		}
		if seen[ref.Value] {
			return
		}
		seen[ref.Value] = true
		name := strings.TrimPrefix(ref.Value, prefix)
		target := mappingGet(schemas, name)
		if target == nil {
			t.Errorf("kelvran-gateway.openapi.yaml:%d: $ref %q names no component", ref.Line, ref.Value)
			return
		}
		walkRequestSchema(t, schemas, target, seen, func(inner string, s *yaml.Node) {
			if inner == "" {
				inner = name
			}
			visit(inner, s)
		})
		return
	}
	visit("", n)
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		switch {
		case schemaSubschemaMaps[k] && v.Kind == yaml.MappingNode:
			for j := 0; j+1 < len(v.Content); j += 2 {
				walkRequestSchema(t, schemas, v.Content[j+1], seen, visit)
			}
		case schemaSubschemaLists[k] && v.Kind == yaml.SequenceNode:
			for _, c := range v.Content {
				walkRequestSchema(t, schemas, c, seen, visit)
			}
		case schemaSubschemaSingles[k] && v.Kind == yaml.MappingNode:
			walkRequestSchema(t, schemas, v, seen, visit)
		}
	}
}

// closedRequestObjects walks every request root of root and returns one line
// per Schema Object closed with `additionalProperties: false`, with the
// number of components and Schema Objects walked.
func closedRequestObjects(t *testing.T, root *yaml.Node) (closed []string, components, objects int) {
	t.Helper()
	schemas := mappingGet(mappingGet(root, "components"), "schemas")
	if schemas == nil {
		t.Fatal("the document has no components.schemas")
	}
	seen := map[string]bool{}
	for s, where := range requestSchemaRoots(t, root) {
		if name := strings.TrimPrefix(where, "components.schemas."); name != where {
			if seen["#/components/schemas/"+name] {
				continue // already walked through a $ref
			}
			seen["#/components/schemas/"+name] = true
		}
		walkRequestSchema(t, schemas, s, seen, func(component string, schema *yaml.Node) {
			objects++
			ap := mappingGet(schema, "additionalProperties")
			if ap == nil || ap.Kind != yaml.ScalarNode || ap.Value != "false" {
				return
			}
			at := where
			if component != "" {
				at = "components.schemas." + component + " (reached from " + where + ")"
			}
			closed = append(closed, fmt.Sprintf("kelvran-gateway.openapi.yaml:%d: %s is closed (additionalProperties: false)", ap.Line, at))
		})
	}
	return closed, len(seen), objects
}

// closedRequestControl is a document whose one request body refers to a
// closed component that in turn refers to an open one: the walker must report
// exactly the closed one, through the $ref.
const closedRequestControl = `openapi: 3.1.0
paths:
  /control:
    post:
      requestBody:
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/Closed'
components:
  schemas:
    Closed:
      type: object
      additionalProperties: false
      properties:
        inner:
          $ref: '#/components/schemas/Open'
    Open:
      type: object
      additionalProperties: true
`

// TestOpenAPIRequestSchemasAreOpen: no Schema Object reachable from a request
// body or a *Request component (following $ref) is closed with
// `additionalProperties: false`. Response-only components keep theirs: the
// gateway encodes exactly the members it declares, and the reflection test
// pins that set.
func TestOpenAPIRequestSchemasAreOpen(t *testing.T) {
	var control yaml.Node
	if err := yaml.Unmarshal([]byte(closedRequestControl), &control); err != nil {
		t.Fatal(err)
	}
	if closed, _, _ := closedRequestObjects(t, control.Content[0]); len(closed) != 1 || !strings.Contains(closed[0], "components.schemas.Closed") {
		t.Fatalf("positive control: the walker reported %v, want exactly components.schemas.Closed", closed)
	}
	root := loadOpenAPI(t)
	closed, components, objects := closedRequestObjects(t, root)
	for _, c := range closed {
		t.Errorf("%s but the gateway's decoders ignore unknown members at every depth; open it, or add the strict decoder TestDecodersNeverRejectUnknownMembers forbids", c)
	}
	if components < 15 || objects < 120 {
		t.Errorf("walked %d components and %d Schema Objects from request bodies; the walker lost the document", components, objects)
	}
	t.Logf("%d request components, %d Schema Objects walked", components, objects)
}

// strictDecoderSelectors are the selectors that make a JSON decoder reject
// unknown members: encoding/json's Decoder method and encoding/json/v2's
// option.
var strictDecoderSelectors = map[string]bool{"DisallowUnknownFields": true, "RejectUnknownMembers": true}

// strictDecoderCalls lists where a parsed file selects one of them.
func strictDecoderCalls(fset *token.FileSet, f *ast.File) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && strictDecoderSelectors[sel.Sel.Name] {
			out = append(out, fset.Position(sel.Pos()).String()+": "+sel.Sel.Name)
		}
		return true
	})
	return out
}

// TestDecodersNeverRejectUnknownMembers is the code half of the openness
// rule: no non-test file under cmd/gateway or internal selects a strict
// decoder switch. The positive control parses a source that uses both, so
// the inspection is known to see them before it is trusted to see none.
func TestDecodersNeverRejectUnknownMembers(t *testing.T) {
	fset := token.NewFileSet()
	src := "package p\n\ntype strict interface{ DisallowUnknownFields() }\n\nfunc f(d strict) { d.DisallowUnknownFields(); _ = json.RejectUnknownMembers(true) }\n"
	control, err := parser.ParseFile(fset, "control.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if got := strictDecoderCalls(fset, control); len(got) != 2 {
		t.Fatalf("positive control: the inspection found %d strict-decoder selectors, want 2: %v", len(got), got)
	}
	files := 0
	for _, dir := range []string{".", "../../internal"} {
		walk := func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			files++
			for _, pos := range strictDecoderCalls(fset, f) {
				t.Errorf("%s makes a decoder reject unknown members, which the OpenAPI request schemas say are ignored (TestOpenAPIRequestSchemasAreOpen); close that schema and this rule together", pos)
			}
			return nil
		}
		if err := filepath.WalkDir(dir, walk); err != nil {
			t.Fatal(err)
		}
	}
	if files < 100 {
		t.Errorf("parsed only %d non-test files; the walk lost the tree", files)
	}
	t.Logf("%d non-test files parsed, no strict decoder", files)
}
