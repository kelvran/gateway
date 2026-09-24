package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
)

// writeField writes one hash-input field as tag + ":" + decimal-length +
// ":" + the raw value bytes. This is a length-prefixed (netstring-style)
// encoding, not a bare separator-delimited one: the decimal length makes
// every field's end position unambiguous regardless of what bytes the
// value itself contains, including an embedded instance of "tag:" for
// some OTHER field.
//
// **Fixed 2026-09-17, real bug**: the prior scheme wrote
// "\x00tag=value" segments with no length prefix, trusting "\x00" plus a
// literal tag name to be an unambiguous separator. It wasn't: model (a
// fully client-controlled request field, written via bare %s, never
// re-escaped) can contain a literal NUL byte -- a client sends this via
// ordinary JSON's backslash-u-0000 escape, which encoding/json decodes into a
// real 0x00 byte in the resulting Go string, no special transport needed.
// Given that, two DIFFERENTLY-VALUED (model, messages) pairs could be
// crafted to produce byte-IDENTICAL hash input, e.g. model=`a\x00messages=b`
// with messages=`c` versus model=`a` with messages=`b\x00messages=c` --
// classic ambiguous-delimiter collision, confirmed by direct construction,
// not a theoretical concern. THREAT_MODEL.md's cache STRIDE table already
// names exactly this collision class (KeyPooling) as the attack this
// function's own tenant-folding was built to prevent; it just didn't
// close every instance of it. Length-prefixing each field removes the
// ambiguity structurally: reading `len(value)` decimal digits then
// skipping exactly that many bytes cannot be spoofed by any byte content
// inside the value, so two different (field values) tuples can no longer
// produce the same hash input by construction.
//
// This intentionally changes Key/NormalizedKey's output for every
// existing input versus the pre-fix scheme -- an accepted, one-time,
// self-healing consequence (L1/L2 are caches, not durable state; every
// existing entry silently stops being hit and gets naturally re-populated,
// exactly like this file's own documented guardrail-policy-version-bump
// behavior already does on every policy change).
func writeField(h hash.Hash, tag string, value string) {
	_, _ = fmt.Fprintf(h, "%s:%d:", tag, len(value))
	_, _ = io.WriteString(h, value)
}

// formatOptionalFloat/formatOptionalInt render Key/NormalizedKey's two
// pointer-typed fields as a value writeField can hash unambiguously,
// preserving the nil-vs-zero-value distinction the pointer type exists
// to carry (nil means "caller never set this field at all", not "set to
// zero"). "<nil>" is a safe sentinel here specifically because %v of a
// float64/int can never itself produce that string -- a real formatted
// number is always digits/./e/-/+ (or Inf/NaN), so a present value can
// never be mistaken for the absent-marker.
func formatOptionalFloat(v *float64) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%v", *v)
}

func formatOptionalInt(v *int) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%v", *v)
}

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
// request with no ResponseFormat at all). **Changed 2026-09-17**: this
// used to be omitted from the hash input entirely when empty, as a
// byte-for-byte backward-compatibility guarantee versus the key this
// function produced before this parameter existed. That guarantee is
// gone -- writeField's fix for the real ambiguous-delimiter collision
// documented on writeField above already breaks exact byte-compatibility
// with every pre-fix key regardless, so there is no remaining reason to
// special-case the empty value here too; it's now folded in
// unconditionally like every other field, which is simpler and no less
// safe (an empty length-prefixed field is exactly as unambiguous as any
// other value).
//
// promptFingerprint is folded in the same unconditional way as
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
//
// endUserID is folded in the same unconditional way, per this session's
// own response-cache-compliance-risk research finding: this cache was
// tenant-scoped only, so a response generated for one end user could be
// served verbatim to a DIFFERENT end user behind the same virtual key
// (empirically demonstrated cross-tenant/cross-user cache attacks).
// Callers (dataplane) compute this as the caller-supplied
// X-Kelvran-End-User-Id header's value when the owning VirtualKey has
// CacheScopeToEndUser set, else "" (every request that doesn't opt into
// this, including every one built before this parameter existed) — see
// identity.VirtualKey.CacheScopeToEndUser's own doc comment for the
// full opt-in/fail-closed contract. "" folds in identically to how the
// pre-existing empty-string cases above already do — this fold alone
// only affects the two DIFFERENT-nonempty-values case; two requests
// both passing "" (the default, unscoped case) remain byte-for-byte
// unaffected.
//
// thinkingBindingMode is folded in the same unconditional way, per the
// 2026-09-24 addendum to
// docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md:
// callers (dataplane) pass adapter.ChatRequest.ThinkingBindingMode
// verbatim. Without this, two requests identical in every other field
// but differing only in ThinkingBindingMode (e.g. "" vs "strict") would
// collide on the same L1 key — silently serving a caller who opted into
// "strict" specifically to get a hard 400 on a stale-signed thinking
// block a cached response generated under Kelvran's own non-strict
// default instead, defeating that opt-in's entire purpose. This is
// exactly the class of gap this RFC's own original body already named
// for ReasoningBlocks ("Kelvran's cache-key fingerprint must include a
// hash of the full ReasoningBlocks sequence, not just Content") —
// ThinkingBindingMode governs how a replayed ReasoningBlocks/thinking
// block is handled on THIS call, so the identical rule applies to it.
// "" folds in identically to every other empty-string case above; this
// fold alone only affects the two DIFFERENT-nonempty-values case.
func Key(tenantID string, model string, serializedMessages string, temperature *float64, maxTokens *int, guardrailPolicyVersion string, responseFormatFingerprint string, promptFingerprint string, endUserID string, thinkingBindingMode string) string {
	h := sha256.New()
	// The leading "layer"/"l1" field exists so Key and NormalizedKey can
	// never collide even given byte-identical remaining inputs — cheap
	// insurance against a future refactor ever sharing one cache.Cache
	// instance across layers, since today's isolation relies entirely on
	// L1/L2 living in separate instances, per
	// docs/rfcs/2026-09-03-cache-l2-normalized-match.md.
	writeField(h, "layer", "l1")
	writeField(h, "tenant", tenantID)
	writeField(h, "model", model)
	writeField(h, "messages", serializedMessages)
	writeField(h, "temperature", formatOptionalFloat(temperature))
	writeField(h, "max_tokens", formatOptionalInt(maxTokens))
	writeField(h, "guardrail_policy", guardrailPolicyVersion)
	writeField(h, "response_format", responseFormatFingerprint)
	writeField(h, "prompt", promptFingerprint)
	writeField(h, "end_user", endUserID)
	writeField(h, "thinking_binding_mode", thinkingBindingMode)
	return hex.EncodeToString(h.Sum(nil))
}

// ScopeKey returns the value to pass as Cache.Get/Put/Delete's own
// tenantID partitioning parameter: tenantID unchanged when endUserID is
// empty (the default, unscoped case -- byte-for-byte identical to this
// cache's pre-existing tenantID-only partitioning), or a combined,
// collision-safe scope string when endUserID is set. Deliberately never
// a bare concatenation (e.g. tenantID+":"+endUserID) -- endUserID is
// caller-supplied per identity.VirtualKey.CacheScopeToEndUser's own doc
// comment, and a bare separator would reintroduce exactly the
// ambiguous-delimiter collision class writeField's own doc comment
// documents, just at the partition-scope layer instead of the hash-key
// layer. Reuses writeField's own length-prefixed encoding instead, the
// same reason Key/NormalizedKey do.
func ScopeKey(tenantID, endUserID string) string {
	if endUserID == "" {
		return tenantID
	}
	h := sha256.New()
	writeField(h, "tenant", tenantID)
	writeField(h, "end_user", endUserID)
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
// import internal/adapter. promptFingerprint, endUserID, and
// thinkingBindingMode mirror Key's own identical parameters -- see its
// doc comment above.
func NormalizedKey(tenantID string, model string, normalizedMessages string, temperature *float64, maxTokens *int, guardrailPolicyVersion string, responseFormatFingerprint string, promptFingerprint string, endUserID string, thinkingBindingMode string) string {
	h := sha256.New()
	writeField(h, "layer", "l2")
	writeField(h, "tenant", tenantID)
	writeField(h, "model", model)
	writeField(h, "messages", normalizedMessages)
	writeField(h, "temperature", formatOptionalFloat(temperature))
	writeField(h, "max_tokens", formatOptionalInt(maxTokens))
	writeField(h, "guardrail_policy", guardrailPolicyVersion)
	writeField(h, "response_format", responseFormatFingerprint)
	writeField(h, "prompt", promptFingerprint)
	writeField(h, "end_user", endUserID)
	writeField(h, "thinking_binding_mode", thinkingBindingMode)
	return hex.EncodeToString(h.Sum(nil))
}
