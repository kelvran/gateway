package dataplane

import (
	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
)

// oneHourCacheWriteTokens returns the cache writes finalize prices at the
// 1-hour rate (brainstorm item (i), 2026-10-13).
//
// A provider that reports the split -- Anthropic's
// usage.cache_creation.ephemeral_1h_input_tokens -- is believed as is.
// Bedrock's Converse reports cacheWriteInputTokens with no TTL split, but
// the gateway knows what it forwarded: when the request carried a 1-hour
// breakpoint and the adapter forwards the ttl for this model
// (bedrock.ModelSupportsCacheTTL mirrors cachePointFor), Bedrock billed the
// writes at the 1-hour rate, so every write is priced there. A request
// mixing 5-minute and 1-hour breakpoints is read the conservative way --
// all writes at 1h -- never the undercounting one. A default-TTL request, a
// model the adapter never forwards the ttl to, and every other provider
// keep 0.
func oneHourCacheWriteTokens(dep Deployment, req adapter.ChatRequest, usage adapter.Usage) int {
	if usage.CacheCreation1hTokens > 0 || usage.CacheCreationTokens == 0 {
		return usage.CacheCreation1hTokens
	}
	if dep.Provider != "bedrock" || !bedrock.ModelSupportsCacheTTL(dep.UpstreamModel) || !adapter.HasOneHourCacheBreakpoint(req) {
		return 0
	}
	return usage.CacheCreationTokens
}
