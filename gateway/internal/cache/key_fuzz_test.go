package cache

import (
	"testing"
)

// FuzzKey exercises internal/cache.Key(), the L1 exact-match cache-key
// fabricator, against arbitrary input. Per docs/testing/TESTING.md §9,
// this is one of the two highest-value fuzz targets in the gateway,
// given THREAT_MODEL.md's cache-poisoning concerns: Key() consumes
// caller-controlled strings (a serialized message history can contain
// arbitrary client-supplied text/JSON) and its output is used to look up
// and store cache entries other tenants' requests could collide with.
//
// Property under test: Key must never panic on arbitrary input (a
// malformed/adversarial request must degrade to "some cache key", never
// crash the gateway process), and must be deterministic — calling Key
// twice with the same arguments must always produce the same output, since
// a non-deterministic key would silently defeat caching (permanent
// misses) without ever surfacing as a visible bug.
func FuzzKey(f *testing.F) {
	// Seed with real example inputs pulled from the existing table-driven
	// unit tests in key_test.go, plus a couple of adversarial shapes
	// (empty strings, embedded NUL bytes — since Key's internal separator
	// is "\x00" — and non-UTF8 bytes) that are the actual failure modes a
	// cache-key fabricator should be robust against.
	type seed struct {
		model     string
		messages  string
		hasTemp   bool
		temp      float64
		hasMaxTok bool
		maxTok    int64
	}
	seeds := []seed{
		{model: "gpt-4o", messages: `[{"role":"user","content":"hi"}]`, hasTemp: true, temp: 0.5, hasMaxTok: true, maxTok: 100},
		{model: "", messages: "", hasTemp: false, hasMaxTok: false},
		{model: "claude-opus-4", messages: "\x00embedded\x00nulls\x00", hasTemp: true, temp: -1.5, hasMaxTok: true, maxTok: -1},
		{model: "gpt-4o-mini", messages: `not even json`, hasTemp: false, hasMaxTok: true, maxTok: 0},
	}
	for _, s := range seeds {
		f.Add(s.model, s.messages, s.hasTemp, s.temp, s.hasMaxTok, s.maxTok)
	}

	f.Fuzz(func(t *testing.T, model, messages string, hasTemp bool, temp float64, hasMaxTok bool, maxTok int64) {
		var tempPtr *float64
		if hasTemp {
			tempPtr = &temp
		}
		var maxTokPtr *int
		if hasMaxTok {
			mt := int(maxTok)
			maxTokPtr = &mt
		}

		// Property 1: never panics. If Key panics on any input, the
		// fuzz engine reports it as a failure; there is nothing further
		// to assert here beyond "the call returns."
		k1 := Key(model, messages, tempPtr, maxTokPtr)

		// Property 2: deterministic. The exact same arguments must
		// produce the exact same key on a second call.
		k2 := Key(model, messages, tempPtr, maxTokPtr)
		if k1 != k2 {
			t.Fatalf("Key is not deterministic for model=%q messages=%q temp=%v maxTok=%v: %q != %q",
				model, messages, tempPtr, maxTokPtr, k1, k2)
		}

		// A hex-encoded SHA-256 digest is always exactly 64 hex chars;
		// this is a cheap sanity check that Key's output shape hasn't
		// silently changed (e.g. picking up a different hash size).
		if len(k1) != 64 {
			t.Fatalf("Key returned a digest of length %d, want 64 (hex-encoded SHA-256): %q", len(k1), k1)
		}
	})
}
