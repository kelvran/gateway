package controlplane

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzLoad exercises the hand-rolled, stdlib-only YAML-subset parser
// (parseYAMLMini, reached via Load) against arbitrary byte input. Per
// docs/testing/TESTING.md §9, this is the second of the two
// highest-value fuzz targets in the gateway: config.go's package doc
// explains this parser exists specifically because a third-party YAML
// dependency was ruled out for this pass, which makes it exactly the
// kind of hand-rolled, untrusted-input-facing parser fuzzing is meant to
// harden.
//
// Property under test: Load must never panic on arbitrary byte input. The
// config file is operator-supplied (semi-trusted, not adversarial in the
// way a network request is) but it is still parsed from disk at process
// startup — a malformed or truncated config must degrade to a clean
// parse error the operator can read and fix, never a crash that takes
// down the gateway before it even starts serving traffic. This test does
// NOT assert anything about *which* error is returned (that's covered by
// config_test.go's table-driven unit tests against known-good/known-bad
// fixtures) — only that Load returns rather than panics.
func FuzzLoad(f *testing.F) {
	// Seed with a real, valid, checked-in config (config.example.yaml)
	// plus the hand-written malformed fixtures already used by
	// config_test.go's table-driven tests, so the fuzzer starts from
	// known-interesting inputs rather than pure noise.
	exampleConfig, err := os.ReadFile(filepath.Join("..", "..", "..", "config.example.yaml"))
	if err != nil {
		f.Fatalf("reading config.example.yaml: %v", err)
	}
	f.Add(exampleConfig)

	f.Add([]byte("listen_addr: \":8080\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"))
	f.Add([]byte("listen_addr: \":8080\"\napi_key_env: \"K\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n"))
	f.Add([]byte(""))
	f.Add([]byte("# just a comment\n"))
	f.Add([]byte("::::\n"))
	f.Add([]byte("key: value\n  bad indent: oops\nkey2\n"))
	f.Add([]byte("a:\n  b:\n    c:\n      d: \"deeply nested\"\n"))
	f.Add([]byte("unterminated quote: \"never closes\n"))
	f.Add([]byte("nul\x00byte: value\n"))
	f.Add([]byte("listen_addr: \":8080\"\napi_key_env: \"K\"\nprice_table:\n  m:\n    prompt_per_token: \"not-a-number\"\n"))
	f.Add([]byte("listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\n    budget_usd: 10.0\n    rate_limit:\n      burst: 5\n      refill_per_second: 1\n    allowed_models:\n      gpt-4o: true\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		// The only property under test: Load must return (an error is a
		// perfectly acceptable, expected outcome for most fuzzer-generated
		// input) rather than panic. See the package doc above for why a
		// panic here — not a returned error — is the actual bug class this
		// test defends against.
		_, _ = Load(path)
	})
}
