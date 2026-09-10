# RFC: Server-side prompt/template management

## Status

Accepted, implementing 2026-09-13.

## Summary

Adds `internal/prompt`, a new package holding operator-managed, versioned prompt templates (`prompt_id` + `prompt_version` + `prompt_variables`), resolved at the gateway into real `adapter.Message` content before routing. New Admin API CRUD (`GET/POST/DELETE /admin/prompts...`). Prompts are **global**, not tenant-scoped — a resolved design fork, confirmed with the user via `AskUserQuestion` before this RFC was written.

## Motivation

`docs/upgrade-research/gateway-next-upgrade-round3-2026-09-11.md`'s Finding 2 confirmed two independent competitors (LiteLLM Beta, Helicone GA, 13+ months live) ship a converging API shape for exactly this — resolving `prompt_id`/`version`/`variables` server-side rather than requiring inline prompt text in every client call. Scoped to CRUD + resolution only, deferring prompt experimentation/A-B routing, which even the category leaders haven't shipped.

## Detailed Design

### Global, not tenant-scoped — the resolved design fork

Every existing per-request-computed-state resource in this codebase (cache entries, rate-limit buckets, budget trackers) is keyed by `vk.ID` to prevent cross-tenant leakage (`THREAT_MODEL.md`'s KeyPooling concern). Prompts don't fit that category: they're created and edited only through the Admin API by an operator, the same way `price_table`/`deployments`/`guardrails config` are — all global today. Since no existing precedent in the codebase forced one answer either way, this was raised to the user directly; the resolution is that any virtual key may reference any `prompt_id`, with no per-key ownership field anywhere on `Prompt`.

### `internal/prompt` — `Store`

`Prompt{ID, Version, Messages []adapter.Message, CreatedAt}`. `Store.state atomic.Pointer[storeState]` — the same rebuild-whole-then-swap idiom `identity.Verifier`/`dataplane.Pipeline.verifier` already establish as this codebase's only concurrency pattern for admin-mutable, read-heavy config (never `sync.Map`, never a mutex). One real refinement over that precedent: `Upsert`/`Delete` use `atomic.Pointer.CompareAndSwap` in a retry loop rather than a plain Load-then-Store — `identity.Verifier`'s own admin-write pattern has no lost-update guarantee under concurrent writers, which is fine for its own low-concurrency, human-driven traffic, but this package's own `-race` concurrent-Upsert stress test (`TestConcurrentUpsertGetResolveUnderRace`) requires zero lost/corrupted writes for concurrent Upserts to the same ID. Same core primitive, same whole-map-swap shape — just CAS instead of an unconditional store.

`Persister` (`Load`/`Save`) is a seam only, with zero real implementation anywhere in this module — mirrors `internal/budget`'s own identical separation from its optional `boltstore` backing, which shipped unimplemented for a full RFC cycle before a real store was written against it. `NewStore()` is pure in-memory (the real v1 scope); `NewStoreWithPersister` exists but has no real caller yet.

Version history is append-only: `Upsert` always computes `version = len(existing) + 1` and never mutates an existing version's stored content — a caller pinning an old version by ID+number keeps seeing that exact content forever, even while concurrent Upserts advance "latest."

### Template substitution: minimal `{{name}}` allowlist, not `text/template`

Deliberately not Go's `text/template` — that package's range/if/pipeline/custom-func execution semantics would hand operator-supplied prompt content (edited by potentially less-trusted operators than those touching source code) a real, if low-severity, template-injection surface. `substitute` performs only a literal key-for-value swap for a `{{name}}` placeholder whose name is present in the caller's `variables` map.

**Design decision: an unresolved placeholder (no matching key) is left as literal, unchanged text — never an error, never silently dropped to `""`.** Silently dropping it would turn a forgotten variable into invisible data loss; erroring would make prompt resolution a hard, deploy-order-fragile dependency between template edits and every call site's variable-passing — a strong coupling that cuts against the "any virtual key may reference any prompt_id" global-config design. Leaving the literal text in place keeps a caller's mistake visible and cheap to notice without ever failing the request.

**Substitution reaches both `Message.Content` and multi-modal `Message.Parts[i].Text`.** Found during review: the initial implementation only substituted into `Content`, silently skipping `Parts`-carried text (Kelvran's own multimodal schema, per `docs/rfcs/2026-09-06-gateway-multimodal-content.md`, lets a message carry lead-in `Content` alongside `Parts` for images/documents, or text-typed parts of its own). A prompt template combining an image part with a `{{name}}`-bearing text part would otherwise resolve the placeholder in `Content` but leave `Parts[i].Text`'s own placeholder completely untouched — not merely "unresolved" (which is the documented, visible behavior above) but "never attempted." Fixed before this phase closed: `Resolve` now substitutes into every `Type == "text"` part alongside `Content`, leaving non-text parts (`MediaType`/`Data`/`URL`) untouched. New regression test: `TestResolveSubstitutesTextContentPartsNotJustMessageContent`.

### Cache-key fold

`Resolve` returns a fingerprint alongside the resolved messages: `p.ID + p.Version + sha256(json.Marshal(p.Messages))`, derived from the prompt's **stored, pre-substitution** content, not the resolved output (the resolved output already flows into the cache key via the existing `serializeMessages(req.Messages)` fold, once dataplane sets `req.Messages` to `Resolve`'s result — this fingerprint's job is narrower: a stable identity for *which* template produced that content). `cache.Key`/`cache.NormalizedKey` gain a second new trailing parameter, `promptFingerprint string`, folded in with the exact same conditional pattern Phase 1's `responseFormatFingerprint` established — omitted entirely when empty, so every request with no `PromptID` produces a byte-identical key to before this parameter existed.

Deriving the fingerprint fresh from whatever `Get` returns on every call makes this automatically correct with no extra bookkeeping: stable across repeated calls against the same version, and different the instant a new version exists (the only way stored content can ever change at all, since `Upsert` never edits an existing version in place).

### Call-site wiring

`dataplane.Pipeline` gains a `prompts *prompt.Store` field (defaults to `prompt.NewStore()`), with delegating `UpsertPrompt`/`DeletePrompt`/`GetPrompt`/`ListPrompts` mirroring `UpsertVirtualKey`/`DeleteVirtualKey`'s exact shape. A shared `resolvePromptIfSet` helper (matching this codebase's existing convention of one shared helper called from both `HandleChatCompletion` and `HandleChatCompletionStream`, e.g. `checkRateLimit`/`checkCache`) runs immediately after the model-allowlist check, before rate-limiting: if `req.PromptID != ""` and `len(req.Messages) > 0`, returns `ErrPromptAndMessagesBothSet` (replace, never silently merge); otherwise resolves via `prompts.Resolve`, sets `req.Messages`, and threads the fingerprint into both cache-key call sites.

### Errors

`ErrPromptAndMessagesBothSet` and `ErrPromptResolutionFailed` both map to `400` in `main.go`'s `writeErrorResponse` — a resolution failure (unknown `prompt_id`/`prompt_version`) is treated as a malformed-request condition (closer to `ErrGuardrailBlocked`'s "this request itself is invalid" bucket) rather than a `404`, since this codebase reserves `404` for the Admin API's own resource routes, not the client-facing chat-completions endpoint.

### Admin API

Five new routes mirroring `upsertVirtualKeyHandler`/`deleteVirtualKeyHandler`'s exact shape: `GET /admin/prompts`, `GET /admin/prompts/{id}`, `GET /admin/prompts/{id}/versions/{version}` (viewer-or-admin), `POST /admin/prompts/{id}`, `DELETE /admin/prompts/{id}` (admin-only). Every successful write logs a structured audit entry with the prompt ID and version only, never message content — matching the existing "log identifiers, not payload" discipline.

## Drawbacks

- `Persister` has no real implementation — v1 prompts are lost on gateway restart, matching the exact same disclosed limitation `ARCHITECTURE.md` already documents for virtual keys and every other Admin-mutable resource.
- The cache-key fingerprint adds one more `sha256`/JSON-marshal per prompt-bearing request on every cache-key computation (both hit-check paths) — negligible relative to the upstream LLM call this gates, not measured separately.

## Alternatives Considered

**Tenant-scoped prompts** (keyed by `vk.ID`, matching cache/rate-limit/budget). Rejected via the resolved `AskUserQuestion` decision — prompts are operator-managed config content, not per-tenant computed state; no existing precedent required this shape, and global is simpler for v1 with no identified use case requiring isolation.

**`text/template` for substitution.** Rejected — exposes arbitrary template-execution semantics (range/if/pipeline/funcs) to operator-supplied content for no capability this feature actually needs; the `{{name}}` allowlist covers the real requirement at a fraction of the surface area.

**Erroring or dropping an unresolved placeholder.** Rejected — see the Design Decision section above; leaving it literal keeps mistakes visible without coupling template edits to call-site variable-passing in lockstep.

## Unresolved Questions

- Should `Persister` grow a dedicated delete-specific method once a real implementation exists, rather than `Save(ctx, id, nil)` standing in for removal? Left as an interpretation with zero real implementations to validate against yet.
- Should prompt+inline-`Messages` composition (prepend/append rather than reject) be a future v2 addition? Named as a deliberately deferred richer extension, not decided here.

## Verification

`cd gateway && go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./... && go run github.com/fe3dback/go-arch-lint@v1.18.0 check && gofmt -l . && go mod tidy` — clean except the two pre-existing rootless-Docker integration-test failures this session has repeatedly confirmed unrelated; `go mod tidy` produced an empty diff.

New tests: `internal/prompt/prompt_test.go` (version auto-bump, pinned-version stability across a later edit, delete, a `-race` concurrent-Upsert/Get/Resolve stress test, fingerprint stability/change semantics, the multi-modal-parts substitution regression test found during review); `internal/admin/admin_test.go` (all 5 new routes, viewer-cannot-write); a new `internal/gateway/dataplane/prompt_test.go` (full-pipeline `PromptID` resolution reaching a fake upstream; `PromptID`+`Messages` → 400; prompt-version change busts a cache hit); `internal/cache/key_test.go` new-parameter cases.

Sanity-checked-by-breaking, twice: (1) temporarily made `Upsert` overwrite index 0 instead of appending/bumping — the version-history and concurrency tests failed for the exact predicted reason, restored; (2) temporarily passed `""` for both `promptFP` cache-key arguments — the cache-bust tests failed for the exact predicted reason, restored.
