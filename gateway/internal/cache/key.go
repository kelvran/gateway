package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Key fabricates the L1 exact-match cache key from the fields that
// determine whether two requests are byte-for-byte equivalent for caching
// purposes: the tenant (virtual key) making the request, model, the
// serialized message history, temperature, max_tokens, and (see
// responseFormatFingerprint's own doc comment below) the requested
// response format. This is a real
// key fabricator, not a placeholder — callers (the dataplane pipeline) are
// expected to serialize a request's messages deterministically (e.g.
// canonical JSON) before calling Key, so the same logical request always
// produces the same key.
//
// tenantID is the requesting identity.VirtualKey's ID, per
// docs/rfcs/2026-09-02-virtual-keys-budgets.md — without it, two different
// tenants asking a byte-identical question would silently share one cache
// entry, which is exactly the cross-tenant cache leakage class
// THREAT_MODEL.md's Cache STRIDE table already names as a real, published
// attack (KeyPooling). Folding tenantID into the hash input closes that
// gap by construction: two different tenants can never collide on the
// same key, regardless of how identical their requests otherwise are.
//
// This function deliberately takes primitive/serialized inputs rather
// than a canonical adapter.ChatRequest, so this package never needs to
// import internal/adapter — keeping cache decoupled from the request
// schema entirely, per its documented extractability goal
// (docs/decisions/0002-cache-embedded-in-gateway.md).
//
// guardrailPolicyVersion is folded into the hash the same way
// tenant/model already are, per
// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md: L1/L2 have no
// metadata envelope to attach a stored+checked provenance field to (see
// internal/cache.Cache's own Get/Put([]byte) contract — no such field
// exists), so a guardrail policy/detector change is made safe the same
// way tenant/model isolation already is here — baked into the key
// itself. A policy-version bump implicitly and wholesale invalidates
// every existing L1/L2 entry; the existing key-equality check IS the
// version check, with zero new stored fields or "is this hit still
// valid" code.
// responseFormatFingerprint is folded into the hash the same way
// guardrailPolicyVersion already is, per this feature's own structured-
// output/JSON-schema normalization work: two requests differing only in
// their ResponseFormat must never collide on the same cache entry
// (serving a schema-conforming JSON response to a caller that asked for
// unstructured text, or vice versa, is exactly the kind of
// correctness-breaking collision this cache's whole "gated on
// correctness, not just similarity" design exists to prevent). Callers
// (dataplane) compute this as the raw JSON marshal of
// adapter.ChatRequest.ResponseFormat when non-nil, else "" — this
// package still never imports internal/adapter itself, matching Key's
// own "primitive/serialized inputs only" contract.
//
// Unlike guardrailPolicyVersion (always real, never empty in practice),
// an empty responseFormatFingerprint is the common, expected case (every
// request with no ResponseFormat at all) and the segment is omitted
// entirely for it, never emitted as an empty "\x00response_format=" --
// this is the backward-compatibility guarantee: a nil-ResponseFormat
// request produces the EXACT SAME key this function produced before
// this parameter existed, byte for byte, not merely "a key that's stable
// going forward."
//
// promptFingerprint is folded in exactly the same conditional way as
// responseFormatFingerprint immediately above -- per the server-side
// prompt-management feature (internal/prompt): callers (dataplane)
// compute this as internal/prompt.Store.Resolve's own returned
// fingerprint whenever a request set PromptID, else "" (every request
// that doesn't use prompt_id at all, including every one built before
// this parameter existed). Folding it in matters because dataplane
// resolves PromptID into req.Messages BEFORE computing this key, so two
// requests naming a different prompt_id/prompt_version that happen to
// resolve to byte-identical message content would otherwise be
// indistinguishable to this function -- an unlikely but real collision
// this fold closes the same way guardrailPolicyVersion already closes
// its own "stored provenance, not just message bytes" gap.
func Key(tenantID string, model string, serializedMessages string, temperature *float64, maxTokens *int, guardrailPolicyVersion string, responseFormatFingerprint string, promptFingerprint string) string {
	h := sha256.New()
	// hash.Hash.Write (which fmt.Fprint[f] calls into here) is documented
	// to never return an error, so there is nothing a caller could ever
	// meaningfully do with these return values — discarded explicitly
	// (rather than left unchecked) so that stays a visible, deliberate
	// choice instead of something errcheck has to keep flagging.
	//
	// The leading "layer=l1" tag exists so Key and NormalizedKey can never
	// collide even given byte-identical remaining inputs — cheap
	// insurance against a future refactor ever sharing one cache.Cache
	// instance across layers, since today's isolation relies entirely on
	// L1/L2 living in separate instances, per
	// docs/rfcs/2026-09-03-cache-l2-normalized-match.md.
	_, _ = fmt.Fprintf(h, "layer=l1\x00tenant=%s\x00model=%s\x00messages=%s\x00temperature=", tenantID, model, serializedMessages)
	if temperature != nil {
		_, _ = fmt.Fprintf(h, "%v", *temperature)
	}
	_, _ = fmt.Fprint(h, "\x00max_tokens=")
	if maxTokens != nil {
		_, _ = fmt.Fprintf(h, "%v", *maxTokens)
	}
	_, _ = fmt.Fprintf(h, "\x00guardrail_policy=%s", guardrailPolicyVersion)
	if responseFormatFingerprint != "" {
		_, _ = fmt.Fprintf(h, "\x00response_format=%s", responseFormatFingerprint)
	}
	if promptFingerprint != "" {
		_, _ = fmt.Fprintf(h, "\x00prompt=%s", promptFingerprint)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// NormalizedKey fabricates the L2 normalized-match cache key, per
// docs/rfcs/2026-09-03-cache-l2-normalized-match.md. Same shape and same
// tenantID discipline as Key (see its doc comment — the KeyPooling
// cross-tenant leakage class applies identically here); the only
// difference is normalizedMessages, which the caller (dataplane,
// per its own normalizeMessages helper) must produce by applying exactly
// the conservative allowlist that RFC specifies — this function has no
// opinion on normalization itself, matching Key's own "primitive/
// serialized inputs only" contract so this package still never needs to
// import internal/adapter. promptFingerprint mirrors Key's own identical
// parameter -- see its doc comment above.
func NormalizedKey(tenantID string, model string, normalizedMessages string, temperature *float64, maxTokens *int, guardrailPolicyVersion string, responseFormatFingerprint string, promptFingerprint string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "layer=l2\x00tenant=%s\x00model=%s\x00messages=%s\x00temperature=", tenantID, model, normalizedMessages)
	if temperature != nil {
		_, _ = fmt.Fprintf(h, "%v", *temperature)
	}
	_, _ = fmt.Fprint(h, "\x00max_tokens=")
	if maxTokens != nil {
		_, _ = fmt.Fprintf(h, "%v", *maxTokens)
	}
	_, _ = fmt.Fprintf(h, "\x00guardrail_policy=%s", guardrailPolicyVersion)
	if responseFormatFingerprint != "" {
		_, _ = fmt.Fprintf(h, "\x00response_format=%s", responseFormatFingerprint)
	}
	if promptFingerprint != "" {
		_, _ = fmt.Fprintf(h, "\x00prompt=%s", promptFingerprint)
	}
	return hex.EncodeToString(h.Sum(nil))
}
