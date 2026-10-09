package admin

// expires_at on the admin wire (RFC-3 decision 4, slice (b)): RFC 3339
// strings, 400 on a malformed or past value, absent/""/null mean never,
// omitted from the list entry when zero; billing_subject_id becomes
// settable; rotating an already-expired key requires a new expires_at
// (409 otherwise) and applies it in the same rotation.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

func TestUpsertVirtualKeyExpiresAtAndBillingSubjectRoundTripAndListOmitsZeroExpiry(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)
	body := `{"key_hash":"` + testHashOf("exp-secret") + `","budget_usd":"5","expires_at":"2099-01-01T00:00:00Z","billing_subject_id":"cc-7"}`
	if rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/expiring", fakeAdminCredential(), body); rec.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, body %q", rec.Code, rec.Body.String())
	}
	vk, ok := pipeline.GetVirtualKey("expiring")
	if !ok || !vk.ExpiresAt.Equal(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)) || vk.BillingSubjectID != "cc-7" {
		t.Fatalf("pipeline key = %+v ok=%v, want ExpiresAt 2099-01-01 and billing cc-7", vk, ok)
	}
	body = `{"key_hash":"` + testHashOf("forever-secret") + `","budget_usd":"5"}`
	if rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/forever", fakeAdminCredential(), body); rec.Code != http.StatusNoContent {
		t.Fatalf("POST forever status = %d, body %q", rec.Code, rec.Body.String())
	}
	rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}
	list := rec.Body.String()
	if !strings.Contains(list, `"id":"expiring"`) || !strings.Contains(list, `"expires_at":"2099-01-01T00:00:00Z"`) || !strings.Contains(list, `"billing_subject_id":"cc-7"`) {
		t.Errorf("list must carry expires_at and billing_subject_id for the expiring key: %s", list)
	}
	if strings.Count(list, `"expires_at"`) != 1 {
		t.Errorf("expires_at must be omitted for keys that never expire (want exactly one occurrence): %s", list)
	}
}

func TestUpsertVirtualKeyExpiresAtValidation(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)
	hash := testHashOf("validation-secret")
	for name, tc := range map[string]struct {
		expiresAt  string // raw JSON for the field
		wantStatus int
		wantBody   string
	}{
		"malformed":   {`"not-a-time"`, http.StatusBadRequest, `expires_at "not-a-time" is not a valid RFC 3339 timestamp`},
		"in the past": {`"2000-01-01T00:00:00Z"`, http.StatusBadRequest, "expires_at must be in the future"},
		"null":        {`null`, http.StatusNoContent, ""},
		"empty":       {`""`, http.StatusNoContent, ""},
		"future":      {`"2099-06-01T12:00:00+02:00"`, http.StatusNoContent, ""},
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"key_hash":"` + hash + `","expires_at":` + tc.expiresAt + `}`
			rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/val", fakeAdminCredential(), body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d (body %q), want %d", rec.Code, rec.Body.String(), tc.wantStatus)
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tc.wantBody)
			}
			if tc.wantStatus == http.StatusNoContent {
				vk, _ := pipeline.GetVirtualKey("val")
				if name == "future" {
					if !vk.ExpiresAt.Equal(time.Date(2099, 6, 1, 10, 0, 0, 0, time.UTC)) {
						t.Errorf("stored ExpiresAt = %v, want 2099-06-01T10:00:00Z (offset normalised)", vk.ExpiresAt)
					}
				} else if !vk.ExpiresAt.IsZero() {
					t.Errorf("%s must mean never expires, got %v", name, vk.ExpiresAt)
				}
			}
		})
	}
}

func TestRotateAnExpiredKeyRequiresANewExpiresAtAndAppliesIt(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)
	// The admin API refuses a past expires_at, so seed the expired key
	// directly, the way a restart loads a stored record.
	expired := identity.VirtualKey{ID: "stale", KeyHash: testHashOf("stale-v1"), RateLimitBurst: 100, RateLimitRefill: 100, ExpiresAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := pipeline.UpsertVirtualKey(expired, ratelimit.KeyConfig{ID: "stale", Capacity: 100, RefillPerSecond: 100}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/stale/rotate", fakeAdminCredential(), `{"new_key_hash":"`+testHashOf("stale-v2")+`"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `virtual key "stale" expired at 2000-01-01T00:00:00Z; supply expires_at to rotate it`) {
		t.Fatalf("rotate without expires_at: status = %d body %q, want 409 with the expired-at message", rec.Code, rec.Body.String())
	}
	if vk, _ := pipeline.GetVirtualKey("stale"); vk.KeyHash != testHashOf("stale-v1") {
		t.Fatalf("a refused rotation must not change the key: %+v", vk)
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/virtual_keys/stale/rotate", fakeAdminCredential(), `{"new_key_hash":"`+testHashOf("stale-v2")+`","expires_at":"bad"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `expires_at "bad" is not a valid RFC 3339 timestamp`) {
		t.Fatalf("rotate with a malformed expires_at: status = %d body %q", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/virtual_keys/stale/rotate", fakeAdminCredential(), `{"new_key_hash":"`+testHashOf("stale-v2")+`","expires_at":"2099-01-01T00:00:00Z"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("rotate with expires_at: status = %d body %q", rec.Code, rec.Body.String())
	}
	vk, _ := pipeline.GetVirtualKey("stale")
	if vk.KeyHash != testHashOf("stale-v2") || !vk.ExpiresAt.Equal(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("after rotate-with-expiry: %+v, want the new hash and the 2099 instant", vk)
	}
	// A key that is not expired rotates without expires_at as before.
	rec = doRequest(t, h, http.MethodPost, "/admin/virtual_keys/stale/rotate", fakeAdminCredential(), `{"new_key_hash":"`+testHashOf("stale-v3")+`"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("plain rotate of a live key: status = %d body %q", rec.Code, rec.Body.String())
	}
	if vk, _ := pipeline.GetVirtualKey("stale"); !vk.ExpiresAt.Equal(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("a plain rotation must keep the expiry: %v", vk.ExpiresAt)
	}
}
