package main

// The OpenAPI contract tests (plan item 14, owner gate G5): the document at
// gateway/openapi/kelvran-gateway.openapi.yaml must equal what the gateway
// registers, in both directions, and every claim the document makes that the
// vocabulary cannot carry is pinned here against the code. The YAML is parsed
// with go.yaml.in/yaml/v3 into a *yaml.Node so a key's presence is observable
// and every diff line carries the document's line number. Schema validity
// against the OpenAPI 3.1 meta-schema lives in gateway/compat (a separate Go
// module, so the validator's dependency graph never enters gateway/go.mod).

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/kelvran/gateway/gateway/internal/admin"
)

// openapiDocumentPath is the fixed relative path from this package to the
// document, the same way adminapi_test reads its wire golden.
const openapiDocumentPath = "../../openapi/kelvran-gateway.openapi.yaml"

// loadOpenAPI parses the document once per test into its root mapping node.
func loadOpenAPI(t *testing.T) *yaml.Node {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(openapiDocumentPath))
	if err != nil {
		t.Fatalf("the OpenAPI document is missing: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the OpenAPI document is not YAML: %v", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		t.Fatal("the OpenAPI document is not a single mapping")
	}
	return doc.Content[0]
}

// mappingGet returns the value node for key in a mapping node, or nil.
func mappingGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// mappingKeys returns a mapping node's keys in document order.
func mappingKeys(m *yaml.Node) []string {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	keys := make([]string, 0, len(m.Content)/2)
	for i := 0; i < len(m.Content); i += 2 {
		keys = append(keys, m.Content[i].Value)
	}
	return keys
}

// httpMethods are the only path-item keys that are operations (OpenAPI 3.1
// §4.8.9); everything else under a path item is metadata.
var httpMethods = map[string]string{"get": "GET", "put": "PUT", "post": "POST", "delete": "DELETE", "options": "OPTIONS", "head": "HEAD", "patch": "PATCH", "trace": "TRACE"}

// operation is one METHOD path pair the document declares, with where it is.
type operation struct {
	method, path string
	node         *yaml.Node
	line         int
}

func (o operation) key() string { return o.method + " " + o.path }

// documentOperations lists every operation under paths, in document order.
func documentOperations(t *testing.T, root *yaml.Node) []operation {
	t.Helper()
	paths := mappingGet(root, "paths")
	if paths == nil {
		t.Fatal("the document has no paths")
	}
	var ops []operation
	for i := 0; i+1 < len(paths.Content); i += 2 {
		pathKey, item := paths.Content[i], paths.Content[i+1]
		for j := 0; j+1 < len(item.Content); j += 2 {
			if method, ok := httpMethods[item.Content[j].Value]; ok {
				ops = append(ops, operation{method: method, path: pathKey.Value, node: item.Content[j+1], line: item.Content[j].Line})
			}
		}
	}
	return ops
}

// registeredRoutes is the code's own route set: the data-plane table and the
// admin table with every condition on.
func registeredRoutes() map[string]string {
	routes := map[string]string{}
	for _, rt := range dataPlaneRoutes {
		routes[rt.Method+" "+rt.Path] = "cmd/gateway/mux.go"
	}
	for _, rt := range admin.AllRoutes() {
		routes[rt.Pattern()] = "internal/admin/routes.go, tier " + string(rt.Tier)
	}
	return routes
}

// TestOpenAPIRouteSetEqualsRegisteredRoutes is the rule the whole item rests
// on: {METHOD path} over the document == the two Go route tables, both
// directions, exact strings (Go wildcard names, the pprof subtree's slash).
func TestOpenAPIRouteSetEqualsRegisteredRoutes(t *testing.T) {
	root := loadOpenAPI(t)
	ops := documentOperations(t, root)
	registered := registeredRoutes()
	documented := map[string]int{}
	for _, op := range ops {
		if _, dup := documented[op.key()]; dup {
			t.Errorf("%s declared twice (yaml:%d)", op.key(), op.line)
		}
		documented[op.key()] = op.line
	}
	var missing, extra []string
	for key, where := range registered {
		if _, ok := documented[key]; !ok {
			missing = append(missing, key+" ("+where+")")
		}
	}
	for key, line := range documented {
		if _, ok := registered[key]; !ok {
			extra = append(extra, key+" (kelvran-gateway.openapi.yaml:"+strconv.Itoa(line)+")")
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	for _, m := range missing {
		t.Errorf("registered, not in the document: + %s", m)
	}
	for _, e := range extra {
		t.Errorf("in the document, not registered: - %s", e)
	}
	if got := len(mappingKeys(mappingGet(root, "paths"))); got != 37 {
		t.Errorf("the document has %d path items, want 37 (8 data-plane + 29 admin)", got)
	}
	if len(ops) != 42 {
		t.Errorf("the document has %d operations, want 42 (8 data-plane + 34 admin)", len(ops))
	}
	for _, op := range ops {
		if op.method == "HEAD" && op.path != "/api/hello" {
			t.Errorf("head: declared on %s (yaml:%d); HEAD is an operation only on /api/hello", op.path, op.line)
		}
	}
}
