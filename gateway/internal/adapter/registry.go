package adapter

// Registry is a lookup map of every provider adapter this binary knows
// about, keyed by Adapter.Name(). Task 4 of the scaffolding plan requires
// all five adapters (openai, anthropic, gemini, bedrock, openaicompat) to
// compile registered together in a lookup map; this is that map's home,
// since it depends only on the Adapter interface this package already
// defines and stays a leaf the concrete adapter packages import into,
// never the reverse.
//
// NOTE: constructing the Registry lives in cmd/gateway (Task 8), not here,
// to avoid this package importing every concrete adapter package — that
// would invert the intended dependency direction (adapters depend on the
// canonical types in this package, not the other way around). This map
// type alias exists purely so callers share one vocabulary for "a lookup
// map of adapters by name."
type Registry map[string]Adapter

// ProviderNames returns the set of provider names this gateway ships
// adapters for — the key set of the Registry cmd/gateway constructs, which
// this leaf cannot build (see the note above). cmd/kelvran passes it to
// controlplane.Validate so the CLI's doctor validates a config against the
// same names as the gateway without importing the concrete adapters;
// cmd/gateway's TestAdapterRegistryMatchesProviderNames pins the two sets
// equal in both directions and each key equal to that adapter's Name().
// A fresh map per call, so no caller can mutate a shared set. Added
// 2026-10-10 (RFC-3 decision 6).
func ProviderNames() map[string]struct{} {
	return map[string]struct{}{
		"openai":       {},
		"anthropic":    {},
		"gemini":       {},
		"bedrock":      {},
		"openaicompat": {},
	}
}
