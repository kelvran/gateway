package dataplane

import "testing"

// TestStreamUpstreamURLGeminiDerivesStreamingEndpoint proves the real
// architectural fix docs/rfcs/2026-09-04-gemini-adapter.md's Motivation
// section names: Gemini's streaming endpoint is a genuinely different URL
// (:streamGenerateContent?alt=sse), not a body-flag difference, derived
// from the configured buffered (:generateContent) base_url.
func TestStreamUpstreamURLGeminiDerivesStreamingEndpoint(t *testing.T) {
	dep := Deployment{
		Provider: "gemini",
		BaseURL:  "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent",
	}

	got, err := streamUpstreamURL(dep)
	if err != nil {
		t.Fatalf("streamUpstreamURL: %v", err)
	}
	want := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse"
	if got != want {
		t.Errorf("streamUpstreamURL() = %q, want %q", got, want)
	}
}

// TestStreamUpstreamURLGeminiPreservesExistingQueryParams proves the
// alt=sse derivation doesn't produce a broken double-"?" or clobber an
// operator-configured query string already present on base_url.
func TestStreamUpstreamURLGeminiPreservesExistingQueryParams(t *testing.T) {
	dep := Deployment{
		Provider: "gemini",
		BaseURL:  "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent?key=abc123",
	}

	got, err := streamUpstreamURL(dep)
	if err != nil {
		t.Fatalf("streamUpstreamURL: %v", err)
	}
	want := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse&key=abc123"
	if got != want {
		t.Errorf("streamUpstreamURL() = %q, want %q", got, want)
	}
}

// TestStreamUpstreamURLGeminiRejectsUnexpectedSuffix proves a
// misconfigured base_url (not ending in :generateContent) fails loudly
// rather than silently producing a malformed streaming URL.
func TestStreamUpstreamURLGeminiRejectsUnexpectedSuffix(t *testing.T) {
	dep := Deployment{
		Provider: "gemini",
		BaseURL:  "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash",
	}

	_, err := streamUpstreamURL(dep)
	if err == nil {
		t.Fatal("streamUpstreamURL: want error for base_url not ending in \":generateContent\", got nil")
	}
}

// TestStreamUpstreamURLNonGeminiProvidersUnchanged is the decisive
// backward-compatibility proof, mirroring the weighted-router RFC's own
// "degrades to identical behavior" precedent: every provider that isn't
// Gemini must get back exactly dep.BaseURL, unmodified.
func TestStreamUpstreamURLNonGeminiProvidersUnchanged(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic", "openaicompat"} {
		dep := Deployment{
			Provider: provider,
			BaseURL:  "https://example.com/v1/chat/completions",
		}
		got, err := streamUpstreamURL(dep)
		if err != nil {
			t.Fatalf("streamUpstreamURL(%s): %v", provider, err)
		}
		if got != dep.BaseURL {
			t.Errorf("streamUpstreamURL(%s) = %q, want unchanged %q", provider, got, dep.BaseURL)
		}
	}
}
