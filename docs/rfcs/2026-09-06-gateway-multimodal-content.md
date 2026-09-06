# RFC: Multi-modal (image/document) content on the canonical `Message` schema

## Status

Accepted, implementing 2026-09-06.

## Context

`docs/rfcs/2026-09-04-gemini-adapter.md` and `docs/rfcs/2026-09-04-bedrock-adapter.md` both explicitly deferred this: "no canonical `Message` field exists for non-text content today, and canonical-schema changes affect every adapter, not just Gemini's/Bedrock's — out of this RFC's scope, named as a real future item." The fresh backlog audit re-surfaced it as item 14 (Large, needs a short RFC first).

Verified the real wire shape of every provider directly (Context7 against the real OpenAI OpenAPI spec and the Anthropic Go SDK's own type definitions, plus this codebase's own already-shipped Gemini/Bedrock adapter code) before designing, not assumed:

- **OpenAI/openaicompat**: `content` is either a plain string, or an array of parts — `{"type": "text", "text": "..."}` / `{"type": "image_url", "image_url": {"url": "..."}}` (`url` is either a real URL or a `data:` base64 URI). No native document/PDF part type in the Chat Completions API.
- **Anthropic**: `content` is always an array of typed blocks — `{"type": "image", "source": {"type": "base64"|"url", "media_type": "...", "data"|"url": "..."}}` / `{"type": "document", "source": {...same shape...}}`.
- **Gemini**: `contents[].parts[]` — `{"inlineData": {"mimeType": "...", "data": "..."}}` / `{"fileData": {"mimeType": "...", "fileUri": "..."}}`.
- **Bedrock Converse**: `content[]` — `{"image": {"format": "...", "source": {"bytes": "..."}}}` / `{"document": {"format": "...", "name": "...", "source": {"bytes": "..."}}}`.

A second, load-bearing finding from reading this codebase's own adapter code directly (not assumed from the wire-format research alone): **Anthropic, Gemini, and Bedrock's adapters already represent content as an array of typed parts/blocks internally** (`anthropic.ContentBlock`, `gemini.Part`, `bedrock.ContentBlock`) — only `text`/`tool_use`/`tool_result` variants exist today, but the union-of-typed-parts *shape* these three adapters already use is exactly what a new image/document variant needs to slot into. Only OpenAI/openaicompat use a flat `Content string` natively and need new wire-level handling.

## Design

### Canonical schema: additive `Parts`, `Content` unchanged

```go
// ContentPart is one piece of a multi-modal message's content.
type ContentPart struct {
	// Type is "text", "image", or "document".
	Type string `json:"type"`
	// Text is set when Type == "text".
	Text string `json:"text,omitempty"`
	// MediaType is the MIME type (e.g. "image/png", "application/pdf"),
	// set when Type != "text".
	MediaType string `json:"media_type,omitempty"`
	// Data is base64-encoded inline content. Mutually exclusive with URL.
	Data string `json:"data,omitempty"`
	// URL is a remote reference, passed through to the provider
	// verbatim — Kelvran itself never fetches it. Mutually exclusive
	// with Data.
	URL string `json:"url,omitempty"`
}
```

`Message` gains `Parts []ContentPart` (`json:"parts,omitempty"`). `Content string` is **unchanged** — every existing adapter/dataplane/test call site keeps working exactly as today. A message is multi-modal when `len(Parts) > 0`; `Content` and `Parts` are not mutually exclusive on the canonical type (a message may have plain lead-in text via `Content` and image parts via `Parts`, or an all-`Parts` message with a `Type:"text"` part instead of `Content` — adapters normalize this at the boundary, see below).

**Rejected alternative: making `Content` itself a wire-level union** (a JSON string OR array, matching OpenAI's own `content` field exactly, via a custom `MarshalJSON`/`UnmarshalJSON` pair like `ToolCall`/`ToolDef` already have in this same file). This would give byte-for-byte OpenAI client compatibility, but `Content string` is read/written as a plain string at dozens of existing call sites across every adapter, `dataplane.go`'s serialization/normalization functions, and hundreds of existing tests — changing its Go type is a large, invasive, high-regression-risk change for a compatibility property (accepting a real OpenAI SDK's raw multi-modal JSON verbatim under the key `content` instead of `parts`) that has no concrete consumer today. An additive `Parts` field delivers the real capability (a client can send Kelvran a multi-modal request) at a small fraction of the blast radius. Named explicitly, not silently chosen.

### Cache-key and guardrail coverage: free, by construction — verified, not assumed

`dataplane.serializeMessages` (the L1 cache-key fabricator, and the exact function `runMissPath`'s pre-call guardrail check also calls) does a blind `json.Marshal(messages)` of the whole `[]adapter.Message` slice — confirmed by reading it directly. Once `Parts` has a `json` tag, it is automatically included in both the L1 cache key (no collision risk between two different-image requests that happen to share identical `Content`/other fields) and the guardrail's PII/regex scan (no bypass via `Parts[].Text` instead of `Content`) — zero code changes needed in either function.

`dataplane.normalizeMessages` (L2) copies the whole `Message` struct (including `Parts`, verbatim, un-normalized) before re-marshaling — so two multi-modal messages only L2-collide if their `Parts` are byte-identical too, the correct, conservative behavior.

**Named limitation, not fixed this pass**: `internal/cache.Shingles`/L3's lexical near-duplicate matching operates on `normalizeMessages`'s text output, not on `Parts[].Text` — a multi-modal message's near-duplicate detection only considers `Content`, not text embedded in `Parts`. This can only cause a missed L3 hit (a cache **miss** where a hit might have been possible), never a false hit or cross-request collision — the safe direction, named as real future work rather than solved here.

**Named performance consideration, not a new DoS vector**: the guardrail's regex scan will now also run against base64-encoded binary data inside `Parts[].Data` (since `serializeMessages`'s output includes it verbatim). This is wasted CPU on non-text bytes, but not a new vulnerability class — guardrail scanning already runs against arbitrarily long `Content` text today with the same big-O behavior.

### Per-adapter mapping

- **`anthropic`/`gemini`/`bedrock`**: each `ToProvider`'s existing message-building loop already does `if m.Content != "" { blocks = append(blocks, <text block>) }` (confirmed by direct code read) — a parallel `for _, part := range m.Parts { ... }` appends the provider's own native image/document block/part variant right alongside it, reusing each adapter's existing typed-union `ContentBlock`/`Part` machinery. `anthropic.ContentBlock` gains a `Source *ContentSource` field (`{Type, MediaType, Data, URL}`, mirroring the real Anthropic wire shape); `gemini.Part` gains `InlineData *InlineData`/`FileData *FileData`; `bedrock.ContentBlock` gains `Image *ImageBlock`/`Document *DocumentBlock`.
- **`openai`/`openaicompat`**: `Message.Content string` becomes `json.RawMessage` in each adapter's own native wire type only (never the canonical type) — `ToProvider` marshals a plain JSON string when `Parts` is empty (byte-identical to today's output) or a JSON array of `{type, text}`/`{type, image_url: {url}}` parts when not. `FromProvider`/response-side decoding is untouched — responses stay text-only in this pass (see Alternatives).
- Every adapter's `document` mapping: Anthropic/Bedrock support it natively; Gemini's `fileData`/`inlineData` has no distinct "document" wire type (a PDF is just another `mimeType`) — this adapter passes a `Type:"document"` part through as `inlineData`/`fileData` with `MediaType` as the `mimeType`, identical code path to `image`. OpenAI/openaicompat have no native document content-part in the Chat Completions API — a `Type:"document"` part sent to either of those two adapters returns a real, typed error (`fmt.Errorf`), never a silently-dropped or mis-mapped field.

## Alternatives considered

**Also supporting multi-modal *response* content** (a model generating an image) — rejected for this pass; the audit's own framing and every adapter RFC's prior "no multi-modal" scope note were about *input* (a user sending an image to be understood), not output. `ChatResponse`/`Choice.Message` stay text-only, matching the Gemini RFC's own "image-generation-specific finish reasons... never reachable in practice today" precedent for exactly this class of out-of-scope capability.

**Video/audio content types** — rejected; no provider RFC or audit finding named these, and none of the 4 wire-format references above define them either. `ContentPart.Type` is `"text"|"image"|"document"` only; extending the enum later is a small, additive follow-on if a real need appears.

**Making `Content` a wire-level union** — see Design above.

## A genuinely new, adjacent finding — named, not fixed this pass

`cmd/gateway/main.go`'s `chatCompletionsHandler` does `io.ReadAll(r.Body)` with **no size limit at all** — confirmed via direct code read; no `http.MaxBytesReader`/`ContentLength` check exists anywhere in this codebase today. This gap already existed for arbitrarily long text prompts, but multi-modal content (inline base64 images/documents, naturally tens of MB) makes it dramatically more practically exploitable. Per this session's own established discipline (name adjacent findings, don't scope-creep them into the PR that surfaced them), this is **not fixed in this pass** — tracked as a new backlog item (a configurable request-body-size cap, e.g. via `http.MaxBytesReader`) for a dedicated follow-up.

## Verification

`go build ./... && go vet ./...`, `golangci-lint run ./...`, `go run github.com/fe3dback/go-arch-lint@v1.18.0 check`, `go mod tidy` (empty diff — stdlib `encoding/json` only, no new dependency), `go test ./... -race` — every package `ok` except the same two pre-existing, environmental rootless-Docker failures already documented earlier this session. All adapters' existing regression/golden fixtures pass byte-for-byte unmodified (confirmed, not assumed: the OpenAI/openaicompat golden-fixture regression tests, which compare real checked-in wire-format JSON field-for-field, passed unmodified even after `Message.Content` changed Go type from `string` to `json.RawMessage`).

New unit tests per adapter (17 total) proving a real image/document part maps to that provider's exact real wire shape, plus each adapter's own real-world scope limit returns a typed error rather than a silently wrong mapping (Bedrock: URL-based parts, since Converse has no generic-URL source; OpenAI/openaicompat: document parts, since Chat Completions has no native document content-part type; every adapter: an unrecognized part type). A new `internal/gateway/dataplane/multimodal_cache_guardrail_test.go` (3 tests) proves the cache-key and guardrail claims directly rather than only arguing them from reading the code: two requests differing only in `Parts` produce different L1 cache keys (sanity-checked by temporarily excluding `Parts` from JSON marshaling via its tag — the test failed with the exact expected wrong call count, then restored); two genuinely identical multi-modal requests still cache-hit (Parts doesn't break real caching either); a PII string placed only in `Parts[].Text` is still caught by the pre-call guardrail (no bypass).

**Scope note on end-to-end integration coverage**: rather than one `cmd/gateway` integration test per adapter (5 tests testing the same, structurally-unchanged HTTP→dataplane→adapter plumbing this project's existing adapter integration tests already cover), one representative real end-to-end test (`TestIntegrationMultiModalRequestReachesRealUpstreamAsImageURLPart`, OpenAI) proves the new capability reaches a real upstream HTTP call with the correct wire shape — the adapter-level unit tests above already prove every other provider's own wire-format correctness in isolation, and no adapter-specific *plumbing* changed (only each adapter's own internal `ToProvider` mapping did). Named explicitly as a deliberate scope reduction from this RFC's original verification plan, not a silent shortfall.
