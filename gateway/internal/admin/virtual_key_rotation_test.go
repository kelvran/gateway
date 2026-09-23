package admin

import (
	"context"
	"net/http"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// TestRotateVirtualKeyViaHTTPKeepsOldSecretUsableDuringGracePeriod proves
// the admin route end to end: POST .../rotate returns 204 and, per
// dataplane.Pipeline.RotateVirtualKey, both the old and new client secrets
// authenticate the data-plane pipeline afterward.
func TestRotateVirtualKeyViaHTTPKeepsOldSecretUsableDuringGracePeriod(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger())

	body := `{"new_key_hash":"` + testHashOf("test-key-v2") + `","grace_period_seconds":3600}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/test-key/rotate", fakeAdminCredential(), body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST .../rotate: status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	if _, err := pipeline.HandleChatCompletion(context.Background(), "Bearer test-key", "", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Errorf("HandleChatCompletion with the OLD secret, still within grace period: %v", err)
	}
	if _, err := pipeline.HandleChatCompletion(context.Background(), "Bearer test-key-v2", "", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Errorf("HandleChatCompletion with the NEW secret: %v", err)
	}
}

// TestRotateVirtualKeyRequiresAdminCredential proves the write route stays
// admin-only, mirroring upsertVirtualKeyHandler/deleteVirtualKeyHandler's
// own gating.
func TestRotateVirtualKeyRequiresAdminCredential(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential(), Viewer: fakeViewerCredential()}, discardLogger())

	body := `{"new_key_hash":"` + testHashOf("irrelevant") + `"}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/test-key/rotate", fakeViewerCredential(), body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST .../rotate with the viewer credential: status = %d, want 401 — rotate must never authenticate via viewer", rec.Code)
	}
}

// TestRotateVirtualKeyMissingNewKeyHashIsRejected mirrors
// TestUpsertVirtualKeyMissingKeyHashIsRejected's own validation shape.
func TestRotateVirtualKeyMissingNewKeyHashIsRejected(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/test-key/rotate", fakeAdminCredential(), `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST .../rotate with no new_key_hash: status = %d, want 400", rec.Code)
	}
}

// TestRotateVirtualKeyMalformedNewKeyHashIsRejected proves the malformed
// entry point this real security-relevant boundary was missing:
// new_key_hash going through identity.NewVerifier's own normalizeKeyHash
// validation (via dataplane.Pipeline.RotateVirtualKey) rather than being
// accepted as an opaque, unvalidated string.
func TestRotateVirtualKeyMalformedNewKeyHashIsRejected(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	cases := []struct {
		name       string
		keyHashRaw string
	}{
		{"not hex", `"not-valid-hex!!"`},
		{"very long bogus hex string", `"` + testHashOf("irrelevant") + testHashOf("more-irrelevant") + `"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `{"new_key_hash":` + c.keyHashRaw + `}`
			rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/test-key/rotate", fakeAdminCredential(), body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST .../rotate with new_key_hash=%s: status = %d, want 400, body: %s", c.keyHashRaw, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestRotateVirtualKeyUnknownNameReturns404 mirrors
// TestDeleteVirtualKeyUnknownNameReturns404's own not-found convention.
func TestRotateVirtualKeyUnknownNameReturns404(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	body := `{"new_key_hash":"` + testHashOf("irrelevant") + `"}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/does-not-exist/rotate", fakeAdminCredential(), body)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST .../rotate for an unknown key: status = %d, want 404", rec.Code)
	}
}
