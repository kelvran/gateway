package adapter

import (
	"encoding/base64"
	"errors"
	"testing"
)

func b64(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

var realPNGSignature = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

func TestValidateContentPartsAcceptsTextOnlyMessages(t *testing.T) {
	messages := []Message{{Role: "user", Content: "hello"}}
	if err := ValidateContentParts(messages); err != nil {
		t.Fatalf("ValidateContentParts() = %v, want nil", err)
	}
}

func TestValidateContentPartsAcceptsMatchingImage(t *testing.T) {
	messages := []Message{{
		Role: "user",
		Parts: []ContentPart{
			{Type: "image", MediaType: "image/png", Data: b64(realPNGSignature)},
		},
	}}
	if err := ValidateContentParts(messages); err != nil {
		t.Fatalf("ValidateContentParts() = %v, want nil", err)
	}
}

func TestValidateContentPartsRejectsDeclaredImageButDetectedText(t *testing.T) {
	messages := []Message{{
		Role: "user",
		Parts: []ContentPart{
			{Type: "image", MediaType: "image/png", Data: b64([]byte("this is plain text, not an image"))},
		},
	}}
	err := ValidateContentParts(messages)
	if !errors.Is(err, ErrContentPartMIMEMismatch) {
		t.Fatalf("ValidateContentParts() = %v, want ErrContentPartMIMEMismatch", err)
	}
}

func TestValidateContentPartsSkipsURLReferencedParts(t *testing.T) {
	messages := []Message{{
		Role: "user",
		Parts: []ContentPart{
			{Type: "image", MediaType: "image/png", URL: "https://example.com/cat.png"},
		},
	}}
	if err := ValidateContentParts(messages); err != nil {
		t.Fatalf("ValidateContentParts() = %v, want nil (URL parts have no inline bytes to sniff)", err)
	}
}

func TestValidateContentPartsTreatsUnidentifiableContentAsInconclusive(t *testing.T) {
	// Random, high-entropy bytes that DetectContentType cannot classify
	// fall back to "application/octet-stream" — inconclusive, not a
	// mismatch, per this function's own documented failure-mode choice.
	messages := []Message{{
		Role: "user",
		Parts: []ContentPart{
			{Type: "document", MediaType: "application/pdf", Data: b64([]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0xFF, 0xFE, 0xFD})},
		},
	}}
	if err := ValidateContentParts(messages); err != nil {
		t.Fatalf("ValidateContentParts() = %v, want nil (undetectable content is inconclusive)", err)
	}
}

func TestValidateContentPartsRejectsInvalidBase64(t *testing.T) {
	messages := []Message{{
		Role: "user",
		Parts: []ContentPart{
			{Type: "image", MediaType: "image/png", Data: "not-valid-base64!!!"},
		},
	}}
	if err := ValidateContentParts(messages); err == nil {
		t.Fatal("ValidateContentParts() = nil, want an error for invalid base64")
	}
}
