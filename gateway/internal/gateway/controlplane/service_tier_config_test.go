package controlplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// price_table.<model>.service_tier_multipliers (round-5 Tier A item 4): the
// first nested mapping under a price entry -- OpenAI's per-tier price as a
// multiple of Standard (flex 0.5, priority/fast 2, ultrafast 6 on gpt-6.1-sol).
func TestLoadPriceTableParsesServiceTierMultipliers(t *testing.T) {
	cfg, err := loadPriceTableFixture(t, "  gpt-6.1-sol:\n    prompt_per_token: 0.000002\n    completion_per_token: 0.00001\n    service_tier_multipliers:\n      flex: 0.5\n      priority: 2\n      ultrafast: 6\n  gpt-4o:\n    prompt_per_token: 0.0000025\n    completion_per_token: 0.00001\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sol := cfg.PriceTable["gpt-6.1-sol"]
	want := map[string]string{"flex": "0.5", "priority": "2", "ultrafast": "6"}
	if len(sol.ServiceTierMultipliers) != len(want) {
		t.Fatalf("ServiceTierMultipliers = %v, want %v", sol.ServiceTierMultipliers, want)
	}
	for tier, w := range want {
		if got, ok := sol.ServiceTierMultipliers[tier]; !ok || !got.Equal(decimal.RequireFromString(w)) {
			t.Errorf("%s = %v (present %v), want %s", tier, got, ok, w)
		}
	}
	if cfg.PriceTable["gpt-4o"].ServiceTierMultipliers != nil {
		t.Errorf("gpt-4o without the key = %v, want nil", cfg.PriceTable["gpt-4o"].ServiceTierMultipliers)
	}
}

// Every refusal names the key and the model. A malformed value is a load
// error here (not the optional-decimal "treated as unset" rule): a silently
// unset ultrafast: 6 would be a 6x undercount. Premium tiers below 1 are the
// undercounting direction; flex may be below 1 (it is a discount by design);
// the base tiers are not keys (they ARE the Standard rate) and auto is a
// request-side routing preference, never a served tier; an unknown tier is
// accepted at 1 or above (a new OpenAI tier can be priced without a
// release; a typo can only over-bill) and refused below 1; keys are trimmed
// and lower-cased, so two that collide are refused as a duplicate.
func TestLoadPriceTableRejectsBadServiceTierMultipliers(t *testing.T) {
	base := "  gpt-6.1-sol:\n    prompt_per_token: 0.000002\n    completion_per_token: 0.00001\n"
	for _, tc := range []struct{ name, lines, wantSubstr string }{
		{"not a mapping", "    service_tier_multipliers: 2\n", "must be a mapping"},
		{"malformed value", "    service_tier_multipliers:\n      flex: fast\n", "flex"},
		{"zero", "    service_tier_multipliers:\n      flex: 0\n", "flex"},
		{"negative", "    service_tier_multipliers:\n      priority: -2\n", "priority"},
		{"priority below one", "    service_tier_multipliers:\n      priority: 0.9\n", "priority"},
		{"fast below one", "    service_tier_multipliers:\n      fast: 0.5\n", "fast"},
		{"ultrafast below one", "    service_tier_multipliers:\n      ultrafast: 0.99\n", "ultrafast"},
		{"scale below one", "    service_tier_multipliers:\n      scale: 0.5\n", "scale"},
		{"default is the base rate", "    service_tier_multipliers:\n      default: 1\n", "default"},
		{"standard is the base rate", "    service_tier_multipliers:\n      standard: 1\n", "standard"},
		{"auto is a request-side value", "    service_tier_multipliers:\n      auto: 1\n", "auto"},
		{"unknown tier below one", "    service_tier_multipliers:\n      turbo: 0.5\n", "turbo"},
		{"fast and priority differ", "    service_tier_multipliers:\n      fast: 2.5\n      priority: 2\n", "one price column"},
		{"same tier twice after normalisation", "    service_tier_multipliers:\n      priority: 2\n      Priority: 2\n", "twice"},
	} {
		_, err := loadPriceTableFixture(t, base+tc.lines)
		if err == nil {
			t.Errorf("%s: Load returned nil error, want a config error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "service_tier_multipliers") || !strings.Contains(err.Error(), "gpt-6.1-sol") || !strings.Contains(err.Error(), tc.wantSubstr) {
			t.Errorf("%s: error %q, want it to name service_tier_multipliers, the model and %q", tc.name, err, tc.wantSubstr)
		}
	}
	for _, ok := range []string{
		"    service_tier_multipliers:\n      flex: 1\n",
		"    service_tier_multipliers:\n      fast: 1\n",
		"    service_tier_multipliers:\n      flex: 1.5\n",
		"    service_tier_multipliers:\n      priority: 2\n      fast: 2\n      scale: 1\n",
		// A tier this build does not know is accepted at 1 or above, so a new
		// OpenAI tier can be priced without a release; a typo can only over-bill.
		"    service_tier_multipliers:\n      turbo: 2\n",
	} {
		if _, err := loadPriceTableFixture(t, base+ok); err != nil {
			t.Errorf("%q must load, got %v", strings.TrimSpace(ok), err)
		}
	}
	// Keys are normalised to the lower-case tier OpenAI echoes.
	cfg, err := loadPriceTableFixture(t, base+"    service_tier_multipliers:\n      Priority: 2\n      FLEX: 0.5\n")
	if err != nil {
		t.Fatalf("capitalised keys must load, got %v", err)
	}
	m := cfg.PriceTable["gpt-6.1-sol"].ServiceTierMultipliers
	if !m["priority"].Equal(decimal.NewFromInt(2)) || !m["flex"].Equal(decimal.RequireFromString("0.5")) || len(m) != 2 {
		t.Errorf("normalised multipliers = %v, want priority 2 and flex 0.5", m)
	}
}

// deployments.<name>.service_tier_policy: forward (the default: OpenAI's
// service_tier reaches the provider and the served tier is priced) or strip
// (the member is dropped and logged on this deployment); any other value is a
// load error. Mirrors anthropic_beta_policy.
func TestLoadDeploymentServiceTierPolicy(t *testing.T) {
	load := func(t *testing.T, extra string) (*Config, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(minimalDeploymentConfig(extra)), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	cfg, err := load(t, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Deployments[0].ServiceTierPolicy != ServiceTierPolicyForward {
		t.Errorf("default service_tier_policy = %q, want %q", cfg.Deployments[0].ServiceTierPolicy, ServiceTierPolicyForward)
	}
	for _, v := range []string{ServiceTierPolicyForward, ServiceTierPolicyStrip} {
		cfg, err := load(t, "    service_tier_policy: \""+v+"\"\n")
		if err != nil {
			t.Fatalf("Load(%s): %v", v, err)
		}
		if cfg.Deployments[0].ServiceTierPolicy != v {
			t.Errorf("service_tier_policy = %q, want %q", cfg.Deployments[0].ServiceTierPolicy, v)
		}
	}
	if _, err := load(t, "    service_tier_policy: \"drop\"\n"); err == nil || !strings.Contains(err.Error(), "service_tier_policy") || !strings.Contains(err.Error(), "drop") {
		t.Errorf("Load err = %v, want a refusal naming service_tier_policy and the bad value", err)
	}
	if _, err := load(t, "    service_tier_policy: true\n"); err == nil || !strings.Contains(err.Error(), "service_tier_policy") {
		t.Errorf("Load err = %v, want a refusal of a non-string service_tier_policy", err)
	}
}
