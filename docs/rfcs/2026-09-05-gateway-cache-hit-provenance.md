# RFC: Cache-hit provenance in telemetry (layer, similarity, age)

## Status

Accepted, implemented 2026-09-05.

## Context

`THREAT_MODEL.md`'s Cache Repudiation row named a real gap, corrected to present-tense honesty on 2026-09-05 but never actually closed: "Only a flat boolean ever leaves a cache decision today: `kelvran.cache.hit`... confirmed via direct read, no age/similarity-score/layer-served-from field exists on the span, the log line, or `api/gatewayevents/v1/gatewayevents.proto`, even though the underlying data... is already captured at L3 write time and could be propagated. A downstream failure today can be traced back to 'this was a cache hit,' not to which specific cache decision/layer/age produced it." A fresh backlog audit re-confirmed this directly against `checkCache`/`checkLexicalCache` before ranking it.

## Design

### `cacheProvenance`: one small struct threaded from cache check to telemetry

`dataplane.HandleChatCompletion`/`HandleChatCompletionStream` both replace their bare `cacheHit bool` local with a new `cacheProvenance{Layer string; Similarity float64; AgeMs float64}` value, populated directly from whichever check actually matched:

- `checkCache` (L1/L2) now returns `(cached []byte, layer string, hit bool)` — `"L1"` or `"L2"` on a hit, `""` on a miss. This data was always known (the function already branches on which layer matched); it was simply discarded at the `return cached, true` line before this change.
- `checkLexicalCache` (L3) now returns `(cached []byte, similarity float64, ageMs float64, hit bool)` — `c.Similarity` (the Jaccard estimate) and `time.Since(c.WrittenAt)` were already captured on `LexicalCandidate` at write time; this change is purely "stop discarding what's already computed," not new data capture.

`cacheProvenance.Hit() bool` (`Layer != ""`) replaces the old bare boolean everywhere it was checked.

### Emitted on both existing telemetry surfaces, following existing precedent exactly

Both the OTel span (`telemetry.RecordChatCompletionResult`) and the structured JSON log line (`logRequest`) already carried `kelvran.cache.hit`/`cache_hit` as a bare bool — this change adds `kelvran.cache.layer`/`cache_layer` alongside it on both surfaces, and `kelvran.cache.similarity`/`cache_similarity` + `kelvran.cache.age_ms`/`cache_age_ms` **only when `Layer == "L3"`** (never a fabricated `0.0` for L1/L2/no-hit, matching this codebase's own established "zero value only when genuinely not applicable" discipline already used for `VirtualKeyID`/`Provider`/etc. in the same function).

Deliberately **not** added to `api/gatewayevents/v1/gatewayevents.proto`: that message's own header comment already states its design principle — "never re-encodes anything already real on that [OTel] span" — and `cache_hit` itself already follows that rule (it lives on the span/log line only, never as a proto field). Adding a proto field here would be inconsistent with the message's own stated scope and would need a `buf breaking`-checked schema change for data already available via the existing `trace_id`/`span_id` join. If a consumer someday needs this in the structured decision-event stream specifically, that's a real, separate, smaller follow-on — not bundled into this pass.

### Why L1/L2 don't get similarity/age

L1 is an exact byte match — a "similarity score" isn't a meaningful concept for it. L2's normalized-match layer, like L1, doesn't currently capture a write-time timestamp on its cache entries at all (`cache.Cache`'s interface is a plain `Get`/`Put`, no metadata) — reporting an age for an L1/L2 hit would require widening that interface across every `cache.Cache` implementation, a real, separate, larger change than this pass's actual finding (which was specifically about data *already captured* at L3 write time being discarded). Named explicitly as a real future extension, not solved here.

## Alternatives considered

**Adding the fields to `api/gatewayevents/v1/gatewayevents.proto` instead of (or in addition to) the OTel span** — rejected; see "Deliberately not added" above.

**Widening `cache.Cache`'s interface to capture a write timestamp for L1/L2 too, closing the age gap for all three layers at once** — rejected for this pass: it's a real, legitimate future improvement, but a materially larger change (touching every `cache.Cache` implementation, not just reading data L3 already has) than what this specific finding called for. Named as explicit future work.

## Verification

`internal/gateway/dataplane/cache_l2_test.go`: extended `checkCache`'s existing L2-promotion test with a `layer == "L2"` assertion. `internal/gateway/dataplane/telemetry_wiring_test.go`: extended `TestHandleChatCompletionEmitsSpanWithCacheHitTrue` with assertions that a real miss carries no `cache_layer` attribute at all (not a fabricated `""`) and a real L1 hit reports `"L1"` with no similarity/age attributes; new `TestHandleChatCompletionEmitsSpanWithL3CacheProvenance` (reusing `lexical_cache_test.go`'s own proven near-duplicate fixture text exactly, not a new unverified pair of sentences) proves a real L3 hit reports `"L3"` plus a real similarity value in `(0, 1]` and a non-negative age. Sanity-checked by temporarily hardcoding `checkLexicalCache`'s L3 return to `(c.Resp, 0, 0, true)` (discarding the real similarity/age again) and re-running the new test: failed with `kelvran.cache.similarity = 0, want a real Jaccard estimate in (0, 1]` — the exact wrong value, then restored.
