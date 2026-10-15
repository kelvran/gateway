package admin

import (
	"context"
	"net/http"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// Round-5 Tier S item 24 (2026-10-15): the admin upsert body carries
// rate_limit.tokens_per_day with the same validation as config.yaml, so a
// daily cap set live is enforced and a negative one is refused -- the body
// ignores unknown members, so a missing field here would be a silent drop.
func TestUpsertVirtualKeyWithTokensPerDayIsEnforced(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)

	newBearerValue := "tokens-per-day-test-value"
	body := `{"key_hash":"` + testHashOf(newBearerValue) + `","rate_limit":{"burst":100,"refill_per_second":100,"tokens_per_day":1}}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-theta", fakeAdminCredential(), body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	authHeader := "Bearer " + newBearerValue
	if _, err := pipeline.HandleChatCompletion(context.Background(), authHeader, "", "", adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}, ""); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if _, err := pipeline.HandleChatCompletion(context.Background(), authHeader, "", "", adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi again"}}}, ""); err == nil {
		t.Fatal("second request succeeded, want a rate-limit rejection -- the Admin-API-configured tokens_per_day (1) is already exhausted")
	}
}

func TestUpsertVirtualKeyRejectsANegativeTokensPerDay(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)

	body := `{"key_hash":"` + testHashOf("irrelevant") + `","rate_limit":{"burst":100,"refill_per_second":100,"tokens_per_day":-5}}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-iota", fakeAdminCredential(), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}
