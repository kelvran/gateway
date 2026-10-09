package dataplane

// Whole-key expiry through the dataplane (RFC-3 decision 4, slice (b)):
// the propagation payload carries ExpiresAt as a pointer so a receiver can
// tell "no expiry" from "sender predates the field" and carry the local
// value forward; RotateVirtualKeyWithExpiry sets the instant; an expired
// key is an auth failure whose key id reaches the log line and the
// decision event while the client envelope never names it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/configpropagation"
	"github.com/kelvran/gateway/gateway/internal/identity"
	identityboltstore "github.com/kelvran/gateway/gateway/internal/identity/boltstore"
)

var futureExpiry = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)

func TestVirtualKeyPayloadCarriesExpiresAtAsAnAlwaysSetPointer(t *testing.T) {
	withExpiry := identity.VirtualKey{ID: "k", KeyHash: testHashOf("k"), ExpiresAt: futureExpiry}
	p := virtualKeyToPayload(withExpiry)
	if p.ExpiresAt == nil || !p.ExpiresAt.Equal(futureExpiry) {
		t.Fatalf("payload ExpiresAt = %v, want the key's instant", p.ExpiresAt)
	}
	if got := payloadToVirtualKey(p); !got.ExpiresAt.Equal(futureExpiry) {
		t.Errorf("round trip lost ExpiresAt: %v", got.ExpiresAt)
	}
	never := virtualKeyToPayload(identity.VirtualKey{ID: "k", KeyHash: testHashOf("k")})
	if never.ExpiresAt == nil || !never.ExpiresAt.IsZero() {
		t.Errorf("a never-expiring key must still send a (zero) expires_at so receivers can tell it from an old sender: %v", never.ExpiresAt)
	}
	body, err := json.Marshal(never)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"expires_at":"0001-01-01T00:00:00Z"`) {
		t.Errorf("zero expiry must be on the wire explicitly: %s", body)
	}
	if got := payloadToVirtualKey(configpropagation.VirtualKeyPayload{ID: "k", KeyHash: testHashOf("k")}); !got.ExpiresAt.IsZero() {
		t.Errorf("a nil (absent) payload member decodes to the zero time, got %v", got.ExpiresAt)
	}
}

// TestApplyVirtualKeyUpsertFromEventWithoutExpiresAtCarriesTheLocalExpiryForward
// is the mixed-version rollout guard: an event from a replica that predates
// the field (no expires_at member) must not erase this replica's expiry,
// neither in the Verifier nor in the shared identity store it re-persists
// to; an event that explicitly carries the zero time does clear it.
func TestApplyVirtualKeyUpsertFromEventWithoutExpiresAtCarriesTheLocalExpiryForward(t *testing.T) {
	store, err := identityboltstore.Open(t.TempDir() + "/identity.db")
	if err != nil {
		t.Fatalf("boltstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	key := identity.VirtualKey{ID: "team-a", KeyHash: testHashOf("team-a"), RateLimitBurst: 100, RateLimitRefill: 100, ExpiresAt: futureExpiry}
	p := newTestPipelineWithIdentityStore(t, []identity.VirtualKey{key}, store)

	// Old sender: same key, rotated hash, no expires_at member at all.
	oldSender := configpropagation.VirtualKeyPayload{ID: "team-a", KeyHash: testHashOf("team-a-v2"), RateLimitBurst: 100, RateLimitRefill: 100}
	if err := p.ApplyVirtualKeyUpsertFromEvent(configpropagation.VirtualKeyUpsertPayload{VirtualKey: oldSender}, time.Now().UnixNano(), "old-replica"); err != nil {
		t.Fatalf("ApplyVirtualKeyUpsertFromEvent(old sender): %v", err)
	}
	got, ok := p.GetVirtualKey("team-a")
	if !ok || got.KeyHash != testHashOf("team-a-v2") {
		t.Fatalf("the upsert itself must apply: %+v ok=%v", got, ok)
	}
	if !got.ExpiresAt.Equal(futureExpiry) {
		t.Errorf("Verifier expiry = %v after an old-sender event, want the local %v carried forward", got.ExpiresAt, futureExpiry)
	}
	persisted, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if !persisted["team-a"].ExpiresAt.Equal(futureExpiry) {
		t.Errorf("persisted expiry = %v after an old-sender event, want %v (the receive side persists what it applies)", persisted["team-a"].ExpiresAt, futureExpiry)
	}

	// New sender clearing the expiry on purpose: explicit zero time.
	zero := time.Time{}
	newSender := oldSender
	newSender.ExpiresAt = &zero
	if err := p.ApplyVirtualKeyUpsertFromEvent(configpropagation.VirtualKeyUpsertPayload{VirtualKey: newSender}, time.Now().UnixNano()+1, "new-replica"); err != nil {
		t.Fatalf("ApplyVirtualKeyUpsertFromEvent(new sender, zero): %v", err)
	}
	if got, _ := p.GetVirtualKey("team-a"); !got.ExpiresAt.IsZero() {
		t.Errorf("an explicit zero expires_at must clear the expiry, got %v", got.ExpiresAt)
	}
}

func TestRotateVirtualKeyWithExpirySetsTheInstantAndPlainRotateKeepsIt(t *testing.T) {
	keys := []identity.VirtualKey{{ID: "team-a", KeyHash: testHashOf("team-a"), RateLimitBurst: 100, RateLimitRefill: 100}}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}, chatDeployment(), keys)
	if err := p.RotateVirtualKeyWithExpiry("team-a", testHashOf("team-a-v2"), 0, futureExpiry); err != nil {
		t.Fatalf("RotateVirtualKeyWithExpiry: %v", err)
	}
	got, _ := p.GetVirtualKey("team-a")
	if !got.ExpiresAt.Equal(futureExpiry) || got.KeyHash != testHashOf("team-a-v2") {
		t.Fatalf("after rotate-with-expiry: %+v", got)
	}
	if err := p.RotateVirtualKey("team-a", testHashOf("team-a-v3"), 0); err != nil {
		t.Fatalf("RotateVirtualKey: %v", err)
	}
	if got, _ := p.GetVirtualKey("team-a"); !got.ExpiresAt.Equal(futureExpiry) {
		t.Errorf("a plain rotation must leave the expiry untouched, got %v", got.ExpiresAt)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer team-a-v3", "", "", chatRequest("gpt-4o"), ""); err != nil {
		t.Errorf("the rotated secret must authenticate: %v", err)
	}
}

// TestExpiredKeyRequestIsAnAuthFailureWithTheKeyIdOnTheLogLineAndEvent:
// the client gets ErrKeyExpired (401 key_expired at the HTTP layer) with
// no key id in the message; the gateway's own chat_completion log line and
// decision event carry virtual_key_id and key_expired_at.
func TestExpiredKeyRequestIsAnAuthFailureWithTheKeyIdOnTheLogLineAndEvent(t *testing.T) {
	expired := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	keys := []identity.VirtualKey{{ID: "team-old", KeyHash: testHashOf("team-old"), RateLimitBurst: 100, RateLimitRefill: 100, ExpiresAt: expired}}
	var logBuf bytes.Buffer
	p := newTestPipelineWithKeysAndLogger(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		t.Fatal("upstream must not be called for an expired key")
		return nil, nil
	}, chatDeployment(), keys, slog.New(slog.NewJSONHandler(&logBuf, nil)))

	_, err := p.HandleChatCompletion(context.Background(), "Bearer team-old", "", "", chatRequest("gpt-4o"), "")
	if !errors.Is(err, identity.ErrKeyExpired) {
		t.Fatalf("err = %v, want ErrKeyExpired", err)
	}
	if strings.Contains(err.Error(), "team-old") {
		t.Errorf("client-facing error names the key: %q", err.Error())
	}
	var line map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(raw), &m) == nil && m["msg"] == "chat_completion" {
			line = m
		}
	}
	if line == nil {
		t.Fatalf("no chat_completion log line in %s", logBuf.String())
	}
	if line["virtual_key_id"] != "team-old" || line["key_expired_at"] != "2000-01-01T00:00:00Z" {
		t.Errorf("log line virtual_key_id=%v key_expired_at=%v, want team-old / 2000-01-01T00:00:00Z", line["virtual_key_id"], line["key_expired_at"])
	}
	eventJSON, _ := line["gatewayevents_v1"].(string)
	var event map[string]any
	if err := json.Unmarshal([]byte(eventJSON), &event); err != nil {
		t.Fatalf("gatewayevents_v1 is not a JSON string: %v (%q)", err, eventJSON)
	}
	if event["outcome"] != "OUTCOME_AUTH_FAILED" || event["virtualKeyId"] != "team-old" {
		t.Errorf("decision event outcome=%v virtualKeyId=%v, want OUTCOME_AUTH_FAILED / team-old", event["outcome"], event["virtualKeyId"])
	}
}
