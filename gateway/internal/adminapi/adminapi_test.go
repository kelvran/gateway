package adminapi

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// wireSamples is one fully populated instance of every type in this
// package, keyed by type name. Marshalling it pins the exact JSON field
// names, their order and their omitempty behaviour — the public admin
// wire contract (docs/reference/admin-api.md) that the move out of
// internal/admin must not change and that the kelvran CLI will decode.
func ptr(f float64) *float64 { return &f }

func wireSamples() map[string]any {
	at := time.Date(2026, 10, 10, 1, 2, 3, 0, time.UTC)
	return map[string]any{
		"VirtualKeyRequest": VirtualKeyRequest{
			KeyHash:                    strings.Repeat("ab", 32),
			BudgetUSD:                  decimal.RequireFromString("12.50"),
			BudgetResetIntervalSeconds: 86400,
			BudgetWarnPercent:          0.8,
			AllowedModels:              []string{"gpt-4o"},
			AllowedRegions:             []string{"eu-west-1"},
			AllowedSourceCIDRs:         []string{"10.0.0.0/8"},
			CacheScopeToEndUser:        true,
			AttributionIDsDisabled:     true,
			ExpiresAt:                  "2099-01-01T00:00:00Z",
			BillingSubjectID:           "cost-centre-7",
			RateLimit: &RateLimitRequest{
				Burst: 20, RefillPerSecond: 10, TPMCapacity: 1000, TPMRefillPerSecond: 100,
				PerModel: map[string]PerModelRateLimitRequest{
					"gpt-4o": {Burst: 5, RefillPerSecond: 1, TPMCapacity: 500, TPMRefillPerSecond: 50},
				},
			},
		},
		"VirtualKeyRequest.zero":              VirtualKeyRequest{},
		"RotateVirtualKeyRequest":             RotateVirtualKeyRequest{NewKeyHash: strings.Repeat("cd", 32), GracePeriodSeconds: 600, ExpiresAt: "2099-01-01T00:00:00Z"},
		"AuditEntryResponse":                  AuditEntryResponse{Time: at, Msg: "admin_virtual_key_upserted", Fields: map[string]string{"name": "team-a"}},
		"AuditEntryResponse.zero":             AuditEntryResponse{},
		"BackupResponse":                      BackupResponse{Files: []string{"identity.db"}},
		"BackupResponse.zero":                 BackupResponse{},
		"UpdateDeploymentWeightRequest":       UpdateDeploymentWeightRequest{Weight: 3},
		"EraseCacheEntryRequest":              EraseCacheEntryRequest{VirtualKeyID: "team-a", EndUserID: "u1", ChatRequest: adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}},
		"EraseCacheEntryResponse":             EraseCacheEntryResponse{L1Found: true, L2Found: false, L3Skipped: true},
		"VirtualKeySpendResponse":             VirtualKeySpendResponse{SpentUSD: "1.25", BudgetUSD: "5", BudgetResetIntervalSeconds: 86400, PercentUsed: ptr(0.25)},
		"VirtualKeyListEntry":                 VirtualKeyListEntry{ID: "team-a", BudgetUSD: "5", BudgetResetIntervalSeconds: 86400, BudgetWarnPercent: 0.8, AllowedModels: []string{"gpt-4o"}, AllowedRegions: []string{"eu-west-1"}, AllowedSourceCIDRs: []string{"10.0.0.0/8"}, CacheScopeToEndUser: true, AttributionIDsDisabled: true, RateLimitBurst: 20, RateLimitRefill: 10, BillingSubjectID: "cost-centre-7", ExpiresAt: "2099-01-01T00:00:00Z", PreviousKeyHashExpiresAt: "2026-10-10T02:00:00Z", MaxConcurrentRequests: 4, SpentUSD: "1.25", PercentUsed: ptr(0.25)},
		"VirtualKeyListEntry.zero":            VirtualKeyListEntry{},
		"VirtualKeyListEntry.unavailable":     VirtualKeyListEntry{ID: "team-b", BudgetUSD: "0", SpendUnavailable: true},
		"VirtualKeySpendResponse.unavailable": VirtualKeySpendResponse{BudgetUSD: "5", BudgetResetIntervalSeconds: 86400, SpendUnavailable: true},
		"DeploymentEntry":                     DeploymentEntry{Name: "d1", Model: "gpt-4o", UpstreamModel: "gpt-4o-2024", Provider: "openai", Kind: "chat", Healthy: true, Weight: 2, LatencyFactorPercent: 70, Sticky: true},
		"VirtualKeyInFlightResponse":          VirtualKeyInFlightResponse{TotalInFlight: 2, ByAgentRunID: map[string]int{"run-1": 2}},
		"PromptRequest":                       PromptRequest{Messages: []adapter.Message{{Role: "system", Content: "be brief"}}},
		"PromptResponse":                      PromptResponse{ID: "greeting", Version: 2, Messages: []adapter.Message{{Role: "system", Content: "be brief"}}, CreatedAt: at},
		"SetPromptLabelRequest":               SetPromptLabelRequest{Version: 2},
		"LabelResponse":                       LabelResponse{PromptID: "greeting", Label: "prod", Version: 2, UpdatedAt: at},
		"PerModelRateLimitRequest":            PerModelRateLimitRequest{Burst: 5, RefillPerSecond: 1},
		"RateLimitRequest.zero":               RateLimitRequest{},
	}
}

// TestWireShapesMatchGolden pins the JSON of every admin wire type to
// testdata/wire.golden.json. Regenerate deliberately with
// ADMINAPI_UPDATE_GOLDEN=1 go test ./internal/adminapi/ and read the diff:
// any change here is a change to the public admin API contract and needs
// a docs/reference/admin-api.md update in the same commit.
func TestWireShapesMatchGolden(t *testing.T) {
	got, err := json.MarshalIndent(wireSamples(), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "wire.golden.json")
	if os.Getenv("ADMINAPI_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("writing golden: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden (run once with ADMINAPI_UPDATE_GOLDEN=1 to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("admin wire JSON drifted from %s; if the change is intended, update docs/reference/admin-api.md and regenerate.\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// TestEveryJSONTagIsInTheGolden guards the golden itself: every json tag
// declared on every exported struct in this package must appear in the
// golden file, so a new field cannot slip in unpinned (the zero-value
// samples cover omitempty fields through their populated twins).
func TestEveryJSONTagIsInTheGolden(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "wire.golden.json"))
	if err != nil {
		t.Fatalf("reading golden: %v", err)
	}
	for name, sample := range wireSamples() {
		rt := reflect.TypeOf(sample)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.Anonymous {
				continue // adapter.ChatRequest's own fields are adapter's contract, not this package's
			}
			tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if tag == "" || tag == "-" {
				t.Errorf("%s.%s has no json name", name, f.Name)
				continue
			}
			if !bytes.Contains(golden, []byte(`"`+tag+`"`)) {
				t.Errorf("%s.%s json tag %q is not in the golden; populate it in wireSamples and regenerate", name, f.Name, tag)
			}
		}
	}
}
