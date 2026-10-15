package controlplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Round-5 Tier S item 24 (2026-10-15): virtual_keys.<name>.rate_limit.
// tokens_per_day. A single scalar (0 or absent = no daily cap) that is
// valid alone or beside the TPM pair; a negative or unparseable value is a
// load error, and the key is refused -- not silently ignored -- under
// per_model and under a deployment's rate_limit, the two scopes this slice
// does not implement.

func writeTPDConfig(t *testing.T, virtualKeyRateLimit, deploymentRateLimit string) string {
	t.Helper()
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\n" + virtualKeyRateLimit +
		"deployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n" + deploymentRateLimit
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestLoadParsesTokensPerDayAloneOrBesideTheTPMPair(t *testing.T) {
	cfg, err := Load(writeTPDConfig(t, "    rate_limit:\n      tokens_per_day: 500000\n", ""))
	if err != nil {
		t.Fatalf("Load(tokens_per_day alone): %v", err)
	}
	if got := cfg.VirtualKeys[0]; got.TokensPerDay != 500000 || got.TPMCapacity != 0 || got.TPMRefillPerSecond != 0 {
		t.Fatalf("alone: TokensPerDay %v, TPM %v/%v; want 500000 with no TPM pair", got.TokensPerDay, got.TPMCapacity, got.TPMRefillPerSecond)
	}
	cfg, err = Load(writeTPDConfig(t, "    rate_limit:\n      burst: 20\n      refill_per_second: 10\n      tpm_capacity: 1000\n      tpm_refill_per_second: 50\n      tokens_per_day: 2000000\n", ""))
	if err != nil {
		t.Fatalf("Load(tokens_per_day beside the pairs): %v", err)
	}
	if got := cfg.VirtualKeys[0]; got.TokensPerDay != 2000000 || got.TPMCapacity != 1000 {
		t.Fatalf("beside: TokensPerDay %v, TPMCapacity %v; want 2000000 and 1000", got.TokensPerDay, got.TPMCapacity)
	}
	cfg, err = Load(writeTPDConfig(t, "    rate_limit:\n      burst: 20\n      refill_per_second: 10\n", ""))
	if err != nil || cfg.VirtualKeys[0].TokensPerDay != 0 {
		t.Fatalf("absent: err %v, TokensPerDay %v; want nil and 0", err, cfg.VirtualKeys[0].TokensPerDay)
	}
}

func TestLoadRejectsANegativeTokensPerDay(t *testing.T) {
	_, err := Load(writeTPDConfig(t, "    rate_limit:\n      tokens_per_day: -1\n", ""))
	if err == nil || !strings.Contains(err.Error(), "tokens_per_day") {
		t.Fatalf("Load(tokens_per_day: -1) error = %v, want a load error naming tokens_per_day", err)
	}
}

// A present but unparseable value is loud, like tpm_accounting's
// output_token_multiplier: getFloat's (0, false) would otherwise read as
// "no daily cap" while the operator believes one is enforced.
func TestLoadRejectsAnUnparseableTokensPerDay(t *testing.T) {
	_, err := Load(writeTPDConfig(t, "    rate_limit:\n      tokens_per_day: \"a lot\"\n", ""))
	if err == nil || !strings.Contains(err.Error(), "tokens_per_day") || !strings.Contains(err.Error(), "not a number") {
		t.Fatalf("Load(tokens_per_day: \"a lot\") error = %v, want a load error saying the value is not a number", err)
	}
}

// strconv.ParseFloat accepts "inf" and "nan"; both would be a silent
// no-cap (and a script error on every Redis reservation), so they are
// refused like any other non-number.
func TestLoadRejectsAnInfiniteOrNaNTokensPerDay(t *testing.T) {
	for _, v := range []string{"inf", "+Inf", "NaN", ".nan"} {
		_, err := Load(writeTPDConfig(t, "    rate_limit:\n      tokens_per_day: "+v+"\n", ""))
		if err == nil || !strings.Contains(err.Error(), "tokens_per_day") || !strings.Contains(err.Error(), "not a number") {
			t.Errorf("Load(tokens_per_day: %s) error = %v, want a load error saying the value is not a number", v, err)
		}
	}
}

func TestLoadRejectsTokensPerDayUnderPerModel(t *testing.T) {
	_, err := Load(writeTPDConfig(t, "    rate_limit:\n      burst: 20\n      refill_per_second: 10\n      per_model:\n        gpt-4o:\n          burst: 5\n          refill_per_second: 1\n          tokens_per_day: 1000\n", ""))
	if err == nil || !strings.Contains(err.Error(), "per_model") || !strings.Contains(err.Error(), "tokens_per_day") {
		t.Fatalf("Load(per_model tokens_per_day) error = %v, want a load error refusing the key under per_model", err)
	}
}

func TestLoadRejectsTokensPerDayUnderADeploymentRateLimit(t *testing.T) {
	_, err := Load(writeTPDConfig(t, "", "    rate_limit:\n      burst: 500\n      refill_per_second: 200\n      tokens_per_day: 1000\n"))
	if err == nil || !strings.Contains(err.Error(), "deployment") || !strings.Contains(err.Error(), "tokens_per_day") {
		t.Fatalf("Load(deployment tokens_per_day) error = %v, want a load error refusing the key on a deployment", err)
	}
}
