package controlplane

import "fmt"

// Validate runs every startup check that depends ONLY on cfg's own content
// and on the set of provider names the calling binary registers — never
// opening a file, never touching the network, never reading an environment
// variable — so it is safe to call from the gateway's -validate dry run
// (no side effects at all), as buildPipeline's first statement before any
// store is opened, and from the kelvran CLI's doctor (RFC-3 decision 6,
// docs/rfcs/2026-10-09-gateway-kelvran-cli-and-single-user-mode.md).
//
// providers is the set of adapter names the binary knows: cmd/gateway
// passes the key set of its real adapter registry, cmd/kelvran passes
// adapter.ProviderNames(); cmd/gateway's TestAdapterRegistryMatchesProviderNames
// pins the two equal in both directions. Load cannot perform either check
// itself: it parses one deployment's mapping at a time with no view of the
// full set, and it does not know which adapters the binary ships.
//
// Two checks, moved verbatim (error strings unchanged) from cmd/gateway's
// validateConfig on 2026-10-10: every deployment's provider is registered
// — fail fast at startup rather than on the first request that routes to
// it, per docs/rfcs/2026-09-05-gateway-gen-ai-provider-name-validation.md
// — and every fallback_chains target names a configured deployment, per
// docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md.
func Validate(cfg *Config, providers map[string]struct{}) error {
	names := make(map[string]struct{}, len(cfg.Deployments))
	for _, d := range cfg.Deployments {
		if _, ok := providers[d.Provider]; !ok {
			return fmt.Errorf("deployment %q: no adapter registered for provider %q", d.Name, d.Provider)
		}
		names[d.Name] = struct{}{}
	}
	for _, d := range cfg.Deployments {
		for class, targets := range d.FallbackChains {
			for _, target := range targets {
				if _, ok := names[target]; !ok {
					return fmt.Errorf("deployment %q fallback_chains.%s names %q, which is not a configured deployment", d.Name, class, target)
				}
			}
		}
	}
	return nil
}
