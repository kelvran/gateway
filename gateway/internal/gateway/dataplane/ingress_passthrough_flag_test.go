package dataplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// The ingress carrier describes the hop that served the request (item 11
// slice S11a): on an anthropic deployment the body was relayed as received,
// so passthrough is true and nothing is reported dropped even when the shadow
// had unknown members; on a translate hop passthrough is false and the
// unknown members are the dropped_fields; a rejected request reached no hop;
// the OpenAI route carries nothing.
func TestIngressCarrierFollowsTheServingHop(t *testing.T) {
	ingress := adapter.ChatRequest{Passthrough: &adapter.Passthrough{
		Format:        adapter.IngressFormatAnthropicMessages,
		UnknownFields: map[string]json.RawMessage{"/service_tier": json.RawMessage(`"auto"`)},
	}}
	anthropicDep := Deployment{Name: "claude-primary", Provider: "anthropic"}
	bedrockDep := Deployment{Name: "bedrock-primary", Provider: "bedrock"}
	failed := errors.New("upstream failed")

	cases := []struct {
		name            string
		req             adapter.ChatRequest
		dep             Deployment
		err             error
		wantPassthrough bool
		wantDropped     string
		wantFields      string
	}{
		{"anthropic hop", ingress, anthropicDep, nil, true, "", "[ingress_format anthropic-messages passthrough true]"},
		{"bedrock hop", ingress, bedrockDep, nil, false, "/service_tier", "[ingress_format anthropic-messages passthrough false dropped_fields /service_tier]"},
		{"anthropic hop, request failed", ingress, anthropicDep, failed, false, "", "[ingress_format anthropic-messages passthrough false]"},
		{"openai route", adapter.ChatRequest{}, anthropicDep, nil, false, "", "[]"},
	}
	for _, tc := range cases {
		if got := ingressPassthrough(tc.req, tc.dep, tc.err); got != tc.wantPassthrough {
			t.Errorf("%s: ingressPassthrough = %v, want %v", tc.name, got, tc.wantPassthrough)
		}
		if got := droppedFields(tc.req, tc.dep, tc.err); got != tc.wantDropped {
			t.Errorf("%s: droppedFields = %q, want %q", tc.name, got, tc.wantDropped)
		}
		if got := fmt.Sprint(ingressLogFields(tc.req, tc.dep, tc.err)); got != tc.wantFields {
			t.Errorf("%s: ingressLogFields = %s, want %s", tc.name, got, tc.wantFields)
		}
	}
}
