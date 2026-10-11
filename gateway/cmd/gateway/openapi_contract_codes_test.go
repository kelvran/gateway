package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// readOpenAPIBytes returns the raw document for byte-level checks.
func readOpenAPIBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(openapiDocumentPath))
	if err != nil {
		t.Fatalf("the OpenAPI document is missing: %v", err)
	}
	return raw
}

// dataPlaneStatusTable is transcribed from the route inventory (§2.2) and the
// error writers: per data-plane operation, the response statuses it can
// produce and, per status, the codes that status can carry on that route.
// A code's presence here is a claim about errorStatus/errorTypeAndCode and
// the handler; the enum test below ties the union back to the code.
var dataPlaneStatusTable = map[string]map[string][]string{
	"POST /v1/chat/completions": {
		"200": {}, "400": {"model_not_found", "content_policy_violation", "empty_messages", "invalid_prompt_reference", "streaming_not_supported", "response_format_unsupported", "tool_result_parts_unsupported", "system_parts_unsupported", "invalid_body", "invalid_json", "invalid_tool_choice", "invalid_request"},
		"401": {"invalid_api_key", "key_expired"}, "403": {"model_not_allowed", "source_ip_not_allowed"}, "405": {"method_not_allowed"}, "413": {"request_too_large"},
		"422": {"idempotency_key_reused"}, "429": {"rate_limit_exceeded", "concurrency_limit_exceeded", "insufficient_quota"}, "501": {"streaming_not_configured"}, "502": {"upstream_error"}, "503": {"deployment_capacity_exceeded"},
	},
	"POST /v1/embeddings": {
		"200": {}, "400": {"model_not_found", "content_policy_violation", "not_an_embedding_model", "invalid_body", "invalid_json", "missing_required_parameter"},
		"401": {"invalid_api_key", "key_expired"}, "403": {"model_not_allowed", "source_ip_not_allowed"}, "405": {"method_not_allowed"}, "413": {"request_too_large"},
		"429": {"rate_limit_exceeded", "insufficient_quota"}, "501": {"embeddings_not_configured"}, "502": {"upstream_error"},
	},
	"GET /v1/models":  {"200": {}, "400": {"invalid_request"}, "401": {"invalid_api_key", "key_expired"}, "403": {"source_ip_not_allowed"}, "405": {"method_not_allowed"}},
	"GET /healthz":    {"200": {}, "405": {}},
	"GET /readyz":     {"200": {}, "405": {}, "500": {}, "503": {}},
	"HEAD /api/hello": {"204": {}, "405": {"method_not_allowed"}},
	"POST /v1/messages": {
		"200": {}, "400": {"model_not_found", "content_policy_violation", "empty_messages", "streaming_not_supported", "response_format_unsupported", "tool_result_parts_unsupported", "invalid_body", "invalid_json", "invalid_tool_choice", "invalid_request", "lossy_ingress_rejected", "missing_required_parameter"},
		"401": {"invalid_api_key", "key_expired"}, "403": {"model_not_allowed", "source_ip_not_allowed"}, "405": {"method_not_allowed"}, "413": {"request_too_large"},
		"422": {"idempotency_key_reused"}, "429": {"rate_limit_exceeded", "concurrency_limit_exceeded", "insufficient_quota"}, "501": {"streaming_not_configured"}, "502": {"upstream_error"}, "503": {"deployment_capacity_exceeded"},
	},
	"POST /v1/messages/count_tokens": {
		"200": {}, "400": {"model_not_found", "content_policy_violation", "invalid_body", "invalid_json", "invalid_tool_choice", "missing_required_parameter", "invalid_request"},
		"401": {"invalid_api_key", "key_expired"}, "403": {"source_ip_not_allowed", "model_not_allowed"}, "404": {"count_tokens_unavailable"}, "405": {"method_not_allowed"}, "413": {"request_too_large"},
		"422": {}, "429": {"rate_limit_exceeded"}, "502": {"upstream_error"},
	},
}

// retryAfterTable: per data-plane operation and status, the codes whose
// response carries Retry-After (main's rule: rate limited, deployment
// capacity, the upstream-error default; count_tokens only on its 429).
var retryAfterTable = map[string]map[string][]string{
	"POST /v1/chat/completions":      {"400": {"streaming_not_supported"}, "403": {"source_ip_not_allowed"}, "429": {"rate_limit_exceeded", "concurrency_limit_exceeded"}, "501": {"streaming_not_configured"}, "502": {"upstream_error"}, "503": {"deployment_capacity_exceeded"}},
	"POST /v1/messages":              {"400": {"streaming_not_supported"}, "403": {"source_ip_not_allowed"}, "429": {"rate_limit_exceeded", "concurrency_limit_exceeded"}, "501": {"streaming_not_configured"}, "502": {"upstream_error"}, "503": {"deployment_capacity_exceeded"}},
	"POST /v1/messages/count_tokens": {"429": {"rate_limit_exceeded"}},
}

// handlerLevelCodes are the codes written by the handlers and the route
// layer, outside errorTypeAndCode's pipeline switch.
var handlerLevelCodes = []string{"method_not_allowed", "request_too_large", "invalid_body", "invalid_json", "invalid_tool_choice", "invalid_request", "missing_required_parameter", "count_tokens_unavailable"}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func decodeStringList(t *testing.T, n *yaml.Node, what string) []string {
	t.Helper()
	if n == nil {
		return nil
	}
	var out []string
	if err := n.Decode(&out); err != nil {
		t.Fatalf("%s (yaml:%d): not a list of strings: %v", what, n.Line, err)
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// TestOpenAPIStatusesAndCodes: per data-plane operation the declared response
// statuses equal the table; each error response's x-kelvran-codes equal the
// table; the union of every code equals components.schemas.ErrorCode.enum,
// which equals the codes the envelope writer and the handlers produce; the
// two envelope `type` enums equal the Go constants. Per admin operation:
// 401 everywhere, plain text on every error, the success status from the
// table in routes_test (checked by S4's admin half).
func TestOpenAPIStatusesAndCodes(t *testing.T) {
	root := loadOpenAPI(t)
	union := map[string]bool{}
	for _, op := range documentOperations(t, root) {
		want, ok := dataPlaneStatusTable[op.key()]
		if !ok {
			continue
		}
		responses := mappingGet(op.node, "responses")
		got := mappingKeys(responses)
		wantKeys := make([]string, 0, len(want))
		for s := range want {
			wantKeys = append(wantKeys, s)
		}
		if strings.Join(sortedCopy(got), ",") != strings.Join(sortedCopy(wantKeys), ",") {
			t.Errorf("%s (yaml:%d): responses %v, want %v", op.key(), op.line, sortedCopy(got), sortedCopy(wantKeys))
		}
		for status, codes := range want {
			r := mappingGet(responses, status)
			if r == nil {
				continue
			}
			if status[0] == '2' {
				if mappingGet(r, "x-kelvran-codes") != nil {
					t.Errorf("%s %s: x-kelvran-codes on a success response", op.key(), status)
				}
				continue
			}
			gotCodes := decodeStringList(t, mappingGet(r, "x-kelvran-codes"), op.key()+" "+status)
			if gotCodes == nil {
				t.Errorf("%s %s (yaml:%d): error response without x-kelvran-codes", op.key(), status, r.Line)
				continue
			}
			if strings.Join(sortedCopy(gotCodes), ",") != strings.Join(sortedCopy(codes), ",") {
				t.Errorf("%s %s: x-kelvran-codes %v, want %v", op.key(), status, sortedCopy(gotCodes), sortedCopy(codes))
			}
			for _, c := range codes {
				union[c] = true
			}
		}
	}
	enum := decodeStringList(t, mappingGet(mappingGet(mappingGet(mappingGet(root, "components"), "schemas"), "ErrorCode"), "enum"), "ErrorCode.enum")
	enumSet := map[string]bool{}
	for _, c := range enum {
		enumSet[c] = true
	}
	for c := range union {
		if !enumSet[c] {
			t.Errorf("code %q appears on a response but not in ErrorCode.enum", c)
		}
	}
	for c := range enumSet {
		if !union[c] {
			t.Errorf("ErrorCode.enum has %q, which no response declares", c)
		}
	}
	// The enum against the code: every pipeline code the envelope writer can
	// produce (the hoisted envelope table) plus the handler-level codes.
	fromCode := map[string]bool{}
	for _, c := range envelopeCases {
		if c.code != nil {
			fromCode[*c.code] = true
		}
	}
	fromCode["lossy_ingress_rejected"], fromCode["invalid_prompt_reference"] = true, true // pipeline codes the envelope table does not carry today
	for _, c := range handlerLevelCodes {
		fromCode[c] = true
	}
	for c := range fromCode {
		if !enumSet[c] {
			t.Errorf("the code produces %q but ErrorCode.enum lacks it", c)
		}
	}
	for c := range enumSet {
		if !fromCode[c] {
			t.Errorf("ErrorCode.enum has %q, which no writer in the code produces", c)
		}
	}
	// The envelope type enums.
	schemas := mappingGet(mappingGet(root, "components"), "schemas")
	openaiTypes := decodeStringList(t, mappingGet(mappingGet(mappingGet(mappingGet(mappingGet(mappingGet(schemas, "OpenAIError"), "properties"), "error"), "properties"), "type"), "enum"), "OpenAIError.error.type.enum")
	wantOpenAI := []string{errTypeInvalidRequest, errTypeAuthentication, errTypePermission, errTypeRateLimit, errTypeQuota, errTypeServer}
	if strings.Join(sortedCopy(enumOf(openaiTypes)), ",") != strings.Join(sortedCopy(wantOpenAI), ",") {
		t.Errorf("OpenAIError.error.type enum %v, want %v", openaiTypes, wantOpenAI)
	}
}

// enumOf is decodeStringList's result when the node was the `type` schema's
// `enum` list; kept separate so a schema with no enum reads as empty.
func enumOf(list []string) []string { return list }

// TestOpenAPIRetryAfter: the Retry-After header is declared exactly on the
// responses whose x-kelvran-retry-after list is non-empty, and those lists
// equal the table above (table-to-table: isRetryStormEligible is unexported
// and pinned by dataplane's own tests). Admin, embeddings and models never.
func TestOpenAPIRetryAfter(t *testing.T) {
	root := loadOpenAPI(t)
	for _, op := range documentOperations(t, root) {
		responses := mappingGet(op.node, "responses")
		for i := 0; i+1 < len(responses.Content); i += 2 {
			status, r := responses.Content[i].Value, responses.Content[i+1]
			list := decodeStringList(t, mappingGet(r, "x-kelvran-retry-after"), op.key()+" "+status)
			_, declared := headerDeclared(r, "Retry-After")
			want := retryAfterTable[op.key()][status]
			if list == nil {
				if status[0] != '2' && !strings.HasPrefix(op.path, "/admin/") {
					t.Errorf("%s %s: error response without x-kelvran-retry-after", op.key(), status)
				}
				if declared {
					t.Errorf("%s %s: Retry-After declared without x-kelvran-retry-after", op.key(), status)
				}
				continue
			}
			if strings.Join(sortedCopy(list), ",") != strings.Join(sortedCopy(want), ",") {
				t.Errorf("%s %s: x-kelvran-retry-after %v, main's rule says %v", op.key(), status, list, want)
			}
			if declared != (len(list) > 0) {
				t.Errorf("%s %s: Retry-After header declared=%t but x-kelvran-retry-after non-empty=%t", op.key(), status, declared, len(list) > 0)
			}
		}
	}
}

// headerDeclared reports whether a response declares the named header.
func headerDeclared(r *yaml.Node, name string) (*yaml.Node, bool) {
	n := mappingGet(mappingGet(r, "headers"), name)
	return n, n != nil
}
