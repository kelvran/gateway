package main

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/kelvran/gateway/gateway/internal/admin"
)

// securityFor derives an operation's expected security list from the Go
// tables: ordered single-scheme requirements, OR semantics, wrapper order.
func securityFor(t *testing.T, method, path string) []map[string][]string {
	t.Helper()
	for _, rt := range dataPlaneRoutes {
		if rt.Method == method && rt.Path == path {
			switch rt.Auth {
			case authNone:
				return []map[string][]string{}
			case authBearer:
				return []map[string][]string{{"virtualKey": {}}}
			case authBearerOrXAPIKey:
				return []map[string][]string{{"virtualKey": {}}, {"virtualKeyXAPIKey": {}}}
			}
		}
	}
	for _, rt := range admin.AllRoutes() {
		if rt.Pattern() == method+" "+path {
			switch rt.Tier {
			case admin.TierAdmin:
				return []map[string][]string{{"adminToken": {}}}
			case admin.TierAdminOrViewer:
				return []map[string][]string{{"adminToken": {}}, {"viewerToken": {}}}
			case admin.TierAdminOrOperator:
				return []map[string][]string{{"adminToken": {}}, {"operatorToken": {}}}
			case admin.TierAdminViewerCostViewer:
				return []map[string][]string{{"adminToken": {}}, {"viewerToken": {}}, {"costViewerToken": {}}}
			}
		}
	}
	t.Fatalf("%s %s is in neither route table", method, path)
	return nil
}

func decodeSecurity(t *testing.T, op operation) (present bool, list []map[string][]string) {
	t.Helper()
	sec := mappingGet(op.node, "security")
	if sec == nil {
		return false, nil
	}
	if err := sec.Decode(&list); err != nil {
		t.Fatalf("%s (yaml:%d): security does not decode as a list of requirement objects: %v", op.key(), sec.Line, err)
	}
	if list == nil {
		list = []map[string][]string{}
	}
	return true, list
}

func securityEqual(a, b []map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for k := range a[i] {
			if _, ok := b[i][k]; !ok {
				return false
			}
		}
	}
	return true
}

var operationIDPattern = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)

// TestOpenAPIEveryOperationHasIDAndSecurity: operationIds present, unique and
// camelCase (the admin ones equal the route table's); `security` present on
// every operation and equal to what the tables derive (empty only on the
// three credential-free probes); every referenced scheme declared; the six
// declared schemes each used; the x-api-key alias exactly on models,
// messages and count_tokens; a 401 response iff security is non-empty.
func TestOpenAPIEveryOperationHasIDAndSecurity(t *testing.T) {
	root := loadOpenAPI(t)
	schemes := mappingGet(mappingGet(root, "components"), "securitySchemes")
	declared := map[string]bool{}
	for _, k := range mappingKeys(schemes) {
		declared[k] = true
	}
	want := map[string]bool{"virtualKey": true, "virtualKeyXAPIKey": true, "adminToken": true, "viewerToken": true, "costViewerToken": true, "operatorToken": true}
	for k := range want {
		if !declared[k] {
			t.Errorf("components.securitySchemes lacks %s", k)
		}
	}
	for k := range declared {
		if !want[k] {
			t.Errorf("components.securitySchemes declares %s, which the gateway has no credential for", k)
		}
	}
	adminIDs := map[string]string{}
	for _, rt := range admin.AllRoutes() {
		adminIDs[rt.Pattern()] = rt.OperationID
	}
	seen, used, alias := map[string]operation{}, map[string]bool{}, map[string]bool{}
	for _, op := range documentOperations(t, root) {
		idNode := mappingGet(op.node, "operationId")
		if idNode == nil || !operationIDPattern.MatchString(idNode.Value) {
			t.Errorf("%s (yaml:%d): operationId missing or not camelCase", op.key(), op.line)
			continue
		}
		if prev, dup := seen[idNode.Value]; dup {
			t.Errorf("operationId %q used by %s (yaml:%d) and %s (yaml:%d)", idNode.Value, prev.key(), prev.line, op.key(), op.line)
		}
		seen[idNode.Value] = op
		if wantID, ok := adminIDs[op.key()]; ok && wantID != idNode.Value {
			t.Errorf("%s (yaml:%d): operationId %q, the route table says %q", op.key(), op.line, idNode.Value, wantID)
		}
		present, got := decodeSecurity(t, op)
		if !present {
			t.Errorf("%s (yaml:%d): no security key (write `security: []` for a credential-free route)", op.key(), op.line)
			continue
		}
		if expected := securityFor(t, op.method, op.path); !securityEqual(got, expected) {
			t.Errorf("%s (yaml:%d): security %v, the route tables derive %v", op.key(), op.line, got, expected)
		}
		for _, req := range got {
			for scheme := range req {
				used[scheme] = true
				if !declared[scheme] {
					t.Errorf("%s (yaml:%d): security references undeclared scheme %s", op.key(), op.line, scheme)
				}
				if scheme == "virtualKeyXAPIKey" {
					alias[op.path] = true
				}
			}
		}
		_, has401 := responseNode(op, "401")
		if has401 != (len(got) > 0) {
			t.Errorf("%s (yaml:%d): 401 declared=%t but security non-empty=%t", op.key(), op.line, has401, len(got) > 0)
		}
	}
	for k := range want {
		if !used[k] {
			t.Errorf("security scheme %s is declared but no operation uses it", k)
		}
	}
	wantAlias := map[string]bool{"/v1/models": true, "/v1/messages": true, "/v1/messages/count_tokens": true}
	for p := range wantAlias {
		if !alias[p] {
			t.Errorf("%s should accept virtualKeyXAPIKey (bearerFromRequest)", p)
		}
	}
	for p := range alias {
		if !wantAlias[p] {
			t.Errorf("%s declares virtualKeyXAPIKey but its handler reads Authorization alone", p)
		}
	}
}

// responseNode returns the response mapping for a status key, if declared.
func responseNode(op operation, status string) (*yaml.Node, bool) {
	n := mappingGet(mappingGet(op.node, "responses"), status)
	return n, n != nil
}

// TestOpenAPIListenerTagsServersConditionals: one tag per operation derived
// from the tables; path-item `servers` exactly on every /admin/ item and
// nowhere else; `x-kelvran-conditional` iff the row has a condition, equal to
// its config key; `x-kelvran-subtree` iff the row is the subtree; no other
// path ends in a slash.
func TestOpenAPIListenerTagsServersConditionals(t *testing.T) {
	root := loadOpenAPI(t)
	byPattern := map[string]admin.Route{}
	for _, rt := range admin.AllRoutes() {
		byPattern[rt.Pattern()] = rt
	}
	paths := mappingGet(root, "paths")
	for i := 0; i+1 < len(paths.Content); i += 2 {
		pathKey, item := paths.Content[i].Value, paths.Content[i+1]
		isAdmin := strings.HasPrefix(pathKey, "/admin/")
		servers := mappingGet(item, "servers")
		if isAdmin && servers == nil {
			t.Errorf("%s (yaml:%d): admin path item without servers (the admin listener)", pathKey, paths.Content[i].Line)
		}
		if !isAdmin && servers != nil {
			t.Errorf("%s (yaml:%d): servers on a data-plane path item", pathKey, paths.Content[i].Line)
		}
		if strings.HasSuffix(pathKey, "/") && pathKey != "/admin/debug/pprof/" {
			t.Errorf("%s: a trailing slash means a subtree; only GET /admin/debug/pprof/ is one", pathKey)
		}
	}
	for _, op := range documentOperations(t, root) {
		if mappingGet(op.node, "servers") != nil {
			t.Errorf("%s (yaml:%d): servers on an operation; put it on the path item", op.key(), op.line)
		}
		var tags []string
		if n := mappingGet(op.node, "tags"); n != nil {
			_ = n.Decode(&tags)
		}
		wantTag := "Data plane"
		switch {
		case op.path == "/healthz" || op.path == "/readyz" || op.path == "/api/hello":
			wantTag = "Health"
		case strings.HasPrefix(op.path, "/admin/debug/pprof/"):
			wantTag = "Debug"
		case strings.HasPrefix(op.path, "/admin/"):
			wantTag = "Admin"
		}
		if len(tags) != 1 || tags[0] != wantTag {
			t.Errorf("%s (yaml:%d): tags %v, want [%s]", op.key(), op.line, tags, wantTag)
		}
		rt, isAdmin := byPattern[op.key()]
		cond := mappingGet(op.node, "x-kelvran-conditional")
		switch {
		case isAdmin && rt.Condition != admin.Always && (cond == nil || cond.Value != string(rt.Condition)):
			t.Errorf("%s (yaml:%d): x-kelvran-conditional %v, want %q", op.key(), op.line, cond, rt.Condition)
		case (!isAdmin || rt.Condition == admin.Always) && cond != nil:
			t.Errorf("%s (yaml:%d): x-kelvran-conditional on an unconditional route", op.key(), op.line)
		}
		sub := mappingGet(op.node, "x-kelvran-subtree")
		if isAdmin && rt.Subtree() && (sub == nil || sub.Value != "true") {
			t.Errorf("%s (yaml:%d): x-kelvran-subtree: true missing on the subtree pattern", op.key(), op.line)
		}
		if (!isAdmin || !rt.Subtree()) && sub != nil {
			t.Errorf("%s (yaml:%d): x-kelvran-subtree on a non-subtree route", op.key(), op.line)
		}
	}
}

// TestOpenAPIServeMuxBlock pins the root x-kelvran-servemux block to the
// literal the admin suite proves (TestAdminServeMuxSemantics): method-less
// data-plane patterns, method-carrying admin patterns, plain-text 405 before
// auth, HEAD for GET, one subtree, one 307 redirect.
func TestOpenAPIServeMuxBlock(t *testing.T) {
	root := loadOpenAPI(t)
	var got any
	if n := mappingGet(root, "x-kelvran-servemux"); n == nil {
		t.Fatal("no x-kelvran-servemux block")
	} else if err := n.Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"data_plane": map[string]any{"method_in_pattern": false},
		"admin": map[string]any{
			"method_in_pattern": true,
			"wrong_method":      map[string]any{"status": 405, "content_type": "text/plain; charset=utf-8", "allow_header": true, "before_auth": true},
			"head_for_get":      true,
			"subtree_patterns":  []any{"GET /admin/debug/pprof/"},
			"redirects":         []any{map[string]any{"from": "/admin/debug/pprof", "to": "/admin/debug/pprof/", "status": 307}},
		},
	}
	if !deepEqualYAML(got, want) {
		t.Errorf("x-kelvran-servemux = %#v\nwant %#v", got, want)
	}
}

// deepEqualYAML compares yaml-decoded values (ints arrive as int).
func deepEqualYAML(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if !deepEqualYAML(v, bv[k]) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !deepEqualYAML(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// kelvranExtensionAllowlist is the closed set of vendor extensions; a new one
// is added here, in the document's registry and in the reference page that
// documents the extensions (item 14 slice S6).
var kelvranExtensionAllowlist = map[string]bool{
	"x-kelvran-servemux": true, "x-kelvran-extensions": true, "x-kelvran-conditional": true, "x-kelvran-subtree": true,
	"x-kelvran-method-not-allowed": true, "x-kelvran-codes": true, "x-kelvran-retry-after": true, "x-kelvran-passthrough": true,
	"x-kelvran-example-source": true, "x-kelvran-max-body-bytes": true, "x-kelvran-sse-events": true,
	"x-kelvran-relayed-header-prefix": true, "x-kelvran-when-unconfigured": true, "x-kelvran-schema": true,
}

// TestOpenAPIExtensionsAreClosed: the x-kelvran-* keys used anywhere equal the
// root registry and lie within the allowlist above.
func TestOpenAPIExtensionsAreClosed(t *testing.T) {
	root := loadOpenAPI(t)
	used := map[string]bool{}
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if strings.HasPrefix(n.Content[i].Value, "x-kelvran-") {
					used[n.Content[i].Value] = true
				}
				walk(n.Content[i+1])
			}
		}
		for _, c := range n.Content {
			if n.Kind != yaml.MappingNode {
				walk(c)
			}
		}
	}
	walk(root)
	var registry []string
	if n := mappingGet(root, "x-kelvran-extensions"); n == nil {
		t.Fatal("no x-kelvran-extensions registry")
	} else if err := n.Decode(&registry); err != nil {
		t.Fatal(err)
	}
	reg := map[string]bool{}
	for _, r := range registry {
		reg[r] = true
	}
	for k := range used {
		if !reg[k] {
			t.Errorf("%s is used but not in x-kelvran-extensions", k)
		}
		if !kelvranExtensionAllowlist[k] {
			t.Errorf("%s is not in the test's allowlist", k)
		}
	}
	for k := range reg {
		if !used[k] {
			t.Errorf("%s is registered but used nowhere", k)
		}
	}
	keys := make([]string, 0, len(used))
	for k := range used {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("extensions in use: %s", strings.Join(keys, ", "))
}

// TestOpenAPIPermissiveMessages pins RFC-1 Option C in the document: the two
// Anthropic request schemas are explicitly open (`additionalProperties: true`,
// `x-kelvran-passthrough: anthropic`) with exactly the members the handlers
// demand on every path (`model`, `max_tokens` and `messages` on messages;
// `model` alone on count_tokens, whose `messages` is demanded only where the
// body is parsed, on an `anthropic` deployment), and the Messages response
// does not require `type` (the passthrough fixture and the compat mock omit
// it).
func TestOpenAPIPermissiveMessages(t *testing.T) {
	root := loadOpenAPI(t)
	schemas := mappingGet(mappingGet(root, "components"), "schemas")
	for name, wantRequired := range map[string][]string{"AnthropicMessagesRequest": {"model", "max_tokens", "messages"}, "AnthropicCountTokensRequest": {"model"}} {
		s := mappingGet(schemas, name)
		if s == nil {
			t.Errorf("components.schemas.%s missing", name)
			continue
		}
		if ap := mappingGet(s, "additionalProperties"); ap == nil || ap.Value != "true" {
			t.Errorf("%s: additionalProperties must be an explicit true", name)
		}
		if pt := mappingGet(s, "x-kelvran-passthrough"); pt == nil || pt.Value != "anthropic" {
			t.Errorf("%s: x-kelvran-passthrough: anthropic missing", name)
		}
		var required []string
		if n := mappingGet(s, "required"); n != nil {
			_ = n.Decode(&required)
		}
		if strings.Join(required, ",") != strings.Join(wantRequired, ",") {
			t.Errorf("%s: required %v, want %v", name, required, wantRequired)
		}
	}
	var required []string
	if n := mappingGet(mappingGet(schemas, "AnthropicMessage"), "required"); n != nil {
		_ = n.Decode(&required)
	}
	for _, r := range required {
		if r == "type" {
			t.Error("AnthropicMessage.required must not contain type (the passthrough path relays bodies without it)")
		}
	}
}

// forbiddenHeaderMentions finds every request-id spelling in the document.
func forbiddenHeaderMentions(doc []byte) []string {
	return regexp.MustCompile(`(?i)request[-_]?id`).FindAllString(string(doc), -1)
}

// TestOpenAPINoRequestIDHeader: the gateway has no request-id header, so the
// document must not mention one; the positive control proves the detector.
func TestOpenAPINoRequestIDHeader(t *testing.T) {
	if got := forbiddenHeaderMentions([]byte("headers:\n  X-Request-Id:\n")); len(got) == 0 {
		t.Fatal("positive control: the detector missed X-Request-Id")
	}
	raw := readOpenAPIBytes(t)
	if got := forbiddenHeaderMentions(raw); len(got) != 0 {
		t.Errorf("the document mentions a request-id header %v; the gateway has none", got)
	}
}
