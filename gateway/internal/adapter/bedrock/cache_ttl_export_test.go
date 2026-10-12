package bedrock

import "testing"

// The dataplane prices a Bedrock request's cache writes at the 1-hour rate
// only when the adapter actually forwarded the ttl, which happens only for
// the models on bedrockCacheTTLModelSubstrings -- so that check must be
// reachable from outside the package and agree with the adapter's own.
func TestModelSupportsCacheTTLIsExportedAndAgreesWithTheAdapter(t *testing.T) {
	for model, want := range map[string]bool{
		"global.anthropic.claude-sonnet-5-5-20260301-v1:0": true,
		"global.anthropic.claude-haiku-4-5-20251001-v1:0":  true,
		"amazon.nova-pro-v1:0":                             false,
		"":                                                 false,
	} {
		if got := ModelSupportsCacheTTL(model); got != want {
			t.Errorf("ModelSupportsCacheTTL(%q) = %v, want %v", model, got, want)
		}
		if ModelSupportsCacheTTL(model) != bedrockModelSupportsCacheTTL(model) {
			t.Errorf("ModelSupportsCacheTTL(%q) disagrees with the adapter's own check", model)
		}
	}
}
