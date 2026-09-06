package adapter

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrContentPartMIMEMismatch is returned by ValidateContentParts when a
// ContentPart's declared MediaType and its inline Data's actually-detected
// content type fall into different top-level categories (e.g. declared
// "image/png" but the bytes sniff as a PDF).
var ErrContentPartMIMEMismatch = errors.New("adapter: declared MediaType does not match the inline content's detected type")

// ValidateContentParts checks every inline (base64 Data-carrying)
// ContentPart across messages for a declared-vs-detected MIME-type
// mismatch, per docs/rfcs/2026-09-06-gateway-multimodal-content.md's own
// named gap and OWASP's File Upload Cheat Sheet ("a client-declared
// Content-Type... is trivial to spoof... should not be relied upon for
// security"). URL-referenced parts (Data == "") have no inline bytes to
// sniff and are skipped — Kelvran never fetches URLs itself, so there is
// nothing here to validate against.
//
// Only a definite cross-category mismatch is rejected (declared category
// vs. detected category, e.g. "image" vs "application" — not the exact
// subtype, since some legitimate document formats like docx/pptx are
// zip-based and would otherwise false-positive against a naive subtype
// comparison). net/http.DetectContentType's "application/octet-stream"
// fallback means "could not identify the content," which is inconclusive,
// never treated as evidence of a mismatch — a false negative here (missing
// a real spoof) costs nothing beyond what the hard-gate design elsewhere in
// this codebase already assumes providers will reject; a false positive
// would incorrectly block a legitimate request, the worse failure mode for
// this specific check.
func ValidateContentParts(messages []Message) error {
	for _, m := range messages {
		for _, part := range m.Parts {
			if part.Type == "text" || part.Data == "" {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(part.Data)
			if err != nil {
				return fmt.Errorf("adapter: content part has invalid base64 data: %w", err)
			}
			declaredCategory, _, _ := strings.Cut(part.MediaType, "/")
			detected := http.DetectContentType(raw)
			if detected == "application/octet-stream" {
				continue
			}
			detectedCategory, _, _ := strings.Cut(detected, "/")
			if declaredCategory != "" && detectedCategory != declaredCategory {
				return fmt.Errorf("%w: declared %q, detected %q", ErrContentPartMIMEMismatch, part.MediaType, detected)
			}
		}
	}
	return nil
}
