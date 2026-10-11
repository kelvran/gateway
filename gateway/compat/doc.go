// Package compat is the gateway's compatibility matrix (RFC-1 §11, item 11
// slice S12): a separate Go module whose tests drive the official
// anthropic-sdk-go and openai-go clients, and the official `anthropic` and
// `openai` Python packages, against a gateway binary built from this tree,
// with replaying httptest upstreams in place of the providers. It is the CI
// merge gate (a required status check on main since 2026-10-11), not a
// unit-test package -- it has no non-test code of its own.
//
// Own module on purpose: the SDKs are test dependencies of this matrix, never
// of the gateway binary, so they stay out of gateway/go.mod, out of the
// gateway's govulncheck surface and out of the coverage profile
// (gateway/.testcoverage.yml) -- `go test ./...` from gateway/ does not
// descend into a nested module. CI runs `cd gateway/compat && go test ./...`
// as its own job.
//
// What is and is not under test: the SDKs' own suites run against a
// spec-driven mock server (Stainless's Steady, scripts/mock in both modules
// at the pinned versions) and never see a gateway, so the tests here are
// Kelvran-authored -- the public-API calls a client makes against the
// gateway's routes (buffered, streaming, a tool loop replaying thinking
// blocks with their signatures, count_tokens, models.list, both error
// envelopes with their `code`, Anthropic's own 400 verbatim and every other
// upstream failure redacted, both credential header forms and their
// precedence) against what the gateway answers, plus the one translate-hop
// case RFC-1 line 191 names: a Bedrock 400 for a forwarded thinking type
// reaches the client under Kelvran's own envelope, never Bedrock's wording.
//
// Since plan item 14 slice S5 the module also holds the OpenAPI conformance
// tests (openapi_conformance_test.go): the document at
// gateway/openapi/kelvran-gateway.openapi.yaml validated against the
// embedded OpenAPI 3.1 meta-schema with libopenapi-validator, the committed
// wire fixtures validated against the components that document them, and the
// started gateway's data-plane answers (buffered, both streaming routes frame
// by frame, the probes, two 401s and three 400s) validated against the
// document, with the observed (operationId, status) pairs pinned to
// testdata/openapi_live_coverage.txt. The validator is a test dependency of this
// module for the same reason the SDKs are: it never enters gateway/go.mod;
// the structural contract tests in gateway/cmd/gateway parse the YAML with
// yaml/v3 alone.
package compat
