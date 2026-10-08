# RFC: Fold `tools` and `tool_choice` into every cache key and the L3 hard-gate

- **Status**: accepted
- **Date**: 2026-10-08
- **Author(s)**: gateway maintainers (plan item 18 of the 2026-10-08 discoverability round; found while verifying item 8a)

## Summary

`cache.Key` (L1), `cache.NormalizedKey` (L2) and `checkLexicalCache`'s hard-gate set (L3) gain one more request-semantic input, `toolsFingerprint`: the canonical JSON of `ChatRequest.Tools` and `ChatRequest.ToolChoice`, `""` when the request carries neither. Two requests that differ only in the tools they offer, or in how they force tool use, no longer share a cached response at any layer. As with every previous fold, every L1/L2 key differs from the previous build's (the new field is hashed even when empty), so entries written before the upgrade become unreachable and expire; nothing has to be flushed by hand.

## Motivation

Until this change neither `Key` nor `NormalizedKey` saw `tools` or `tool_choice`, and `checkLexicalCache` did not gate on them. Identical messages with `tool_choice: "required"` and `tool_choice: "none"`, or with and without tool definitions, collided in L1/L2 — a response that called a tool could be served to a request that offered no tools, or a plain-text answer to a request that required a tool call. The gap predates item 8a but was unreachable for most clients: until 8a, every OpenAI SDK request carrying `tool_choice` was rejected before it got near the cache. 8a's own integration test tripped over it on its first run (identical messages across subtests were served from L1 regardless of `tool_choice`), so the collision is observed, not theoretical.

This is the same class of gap the reasoning-content RFC closed twice (`ReasoningBlocks`, then `ThinkingBindingMode`): a field that changes what the model is asked to do, or may do, is part of the request's identity. The rule that RFC established — every new request-semantic field folds into L1, L2 *and* L3, never into a subset — applies verbatim.

## Detailed Design

**Fingerprint.** `dataplane.toolsFingerprint(req adapter.ChatRequest) string` returns `""` when `len(req.Tools) == 0 && req.ToolChoice == nil`, otherwise `json.Marshal` of `struct{ Tools []adapter.ToolDef; ToolChoice *adapter.ToolChoice }`. `encoding/json` emits struct fields in declaration order and `ToolChoice.MarshalJSON` is canonical (8a kept it so for the idempotency fingerprint), so the string is deterministic for equal inputs. `ToolDef.ParametersJSON` is folded as the caller sent it: two schemas that differ only in whitespace or key order produce different fingerprints and therefore a cache miss, never a false hit — the conservative direction, matching how `responseFormatFingerprint` already treats `response_format`. Tool order is significant for the same reason (and because providers see the order).

**L1/L2.** `Key` and `NormalizedKey` take `toolsFingerprint string` as their last parameter and fold it with `writeField(h, "tools", toolsFingerprint)`, unconditionally, exactly like `response_format`, `prompt`, `end_user` and `thinking_binding_mode`: `""` folds in like every other empty-string case — deterministic and identical for every tool-free request, so such requests still share entries with each other, but not with entries written by the previous build (see rollout). Callers (`HandleChatCompletion`, `HandleChatCompletionStream`, `EraseCacheEntry`) pass `toolsFingerprint(req)`.

**L3.** `cache.LexicalCache.Put` and `cache.LexicalCandidate` gain `ToolsFingerprint`; `writeCache` threads it through; `checkLexicalCache` adds an exact-match gate `c.ToolsFingerprint != toolsFingerprint(req) → skip`, both-empty counting as a match, placed with the `ReasoningBlocksFingerprint` and `ThinkingBindingMode` gates and, like them, not one of the three named gates `telemetry.RecordCacheL3GateOutcome` counts. The in-process store copies the string into its entry; the gRPC cache adapters implement only the L1 `Cache` interface and are untouched.

**Key change and rollout.** `writeField` hashes the new field even when it is empty, so EVERY L1/L2 key differs from the previous build's — tool-free requests included, exactly as the `response_format`, `prompt`, `end_user` and `thinking_binding_mode` folds each did on their day. Entries written by a pre-change binary are therefore unreachable (never served, evicted by TTL or LRU). The in-process caches are per process and empty on every restart, so for the default deployment this is invisible; a remote L1 behind the gRPC adapter keeps its old entries until they expire. There is nothing to flush by hand, and an operator who wants a clean slate on a remote cache can restart it. Mixed replicas during a rolling deploy simply do not share entries with each other for the duration.

**Idempotency is unaffected.** The idempotency fingerprint is `sha256(json.Marshal(req))` over the whole request and already covered `tools`/`tool_choice`.

## Drawbacks

- One more parameter on `Key`/`NormalizedKey`/`Put` (the fifth such addition); every call site, including thirty-odd test sites, changes. A struct parameter would be cleaner but is a wider refactor than this fix should carry — recorded as a follow-up, not done here.
- Cache hit rate drops for tool-calling workloads whose tool schemas vary cosmetically between requests. That is the correct direction for a hard-gate, and it mirrors `response_format`.

## Alternatives Considered

- **Fold only into L1/L2.** Rejected: the reasoning-content RFC's rule exists because its first implementation did exactly this and left L3 serving across the field; an L3 near-duplicate hit written with a different tool set is the same wrong answer.
- **Hash tool names only.** Rejected: two tools with the same name and different schemas, or the same tools with `tool_choice` `required` vs `none`, are different requests. Fingerprinting the full canonical JSON is cheap (the request is already in memory) and leaves no such hole.
- **Normalise `ParametersJSON` before hashing** (parse and re-marshal). Rejected for now: it would raise hit rates for cosmetically different schemas but adds a parse per request and a new way for two requests to be judged equal; `response_format` has lived with the conservative behaviour since 2026-09-17.
- **Bypass the cache when tools are present.** Rejected: tool-calling conversations are exactly where repeated identical turns (retries, agent loops) occur; the fingerprint keeps caching correct instead of switching it off.

## Unresolved Questions

- Whether to normalise `ParametersJSON` before hashing if tool-calling hit rates turn out to matter (above).
- A struct-shaped `Put`/`Key` signature (above).

## Verification

Tests written first: `TestKeyDiffersOnToolsFingerprint`/`TestNormalizedKeyDiffersOnToolsFingerprint` (`internal/cache/key_test.go`; the empty fingerprint is asserted deterministic, not equal to the previous scheme's output); `TestToolsFingerprint*` (empty without tools or tool_choice, differs on `tool_choice` mode, differs on tool definitions, equal inputs equal); `TestHandleChatCompletionNeverServesAcrossDifferentToolChoice` and `…AcrossToolsPresence` (two upstream calls for identical messages that differ only in `tool_choice` / tool presence, then a genuine hit for an identical repeat); `TestHandleChatCompletionStreamNeverServesAcrossDifferentToolChoice`; `TestCheckLexicalCacheNeverServesAcrossDifferentTools` (an L3 entry written with one tool set is not served to a near-duplicate carrying another, and is served to an exact match). Full `go test ./... -race`, `golangci-lint`, `go-arch-lint` as for every change.
