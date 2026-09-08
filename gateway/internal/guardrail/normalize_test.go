package guardrail

import "testing"

func TestStripHiddenUnicodeNoOpOnCleanText(t *testing.T) {
	text := "Charge my card 4111 1111 1111 1111 for the invoice."
	stripped, origOffsets := stripHiddenUnicode(text)
	if stripped != text {
		t.Errorf("stripped = %q, want unchanged %q", stripped, text)
	}
	if len(origOffsets) != len(text) {
		t.Fatalf("len(origOffsets) = %d, want %d", len(origOffsets), len(text))
	}
	for i, off := range origOffsets {
		if off != i {
			t.Errorf("origOffsets[%d] = %d, want %d (identity mapping on clean text)", i, off, i)
		}
	}
}

func TestStripHiddenUnicodeRemovesZeroWidthSpace(t *testing.T) {
	zwsp := string(rune(0x200B))
	text := "4111" + zwsp + "1111"
	stripped, origOffsets := stripHiddenUnicode(text)
	if stripped != "41111111" {
		t.Errorf("stripped = %q, want %q", stripped, "41111111")
	}
	if len(origOffsets) != len(stripped) {
		t.Fatalf("len(origOffsets) = %d, want %d", len(origOffsets), len(stripped))
	}
	// The first 4 bytes ("4111") map 1:1; the ZWSP (3 bytes, U+200B) is
	// skipped entirely, so the next stripped byte ("1" of the second
	// "1111") must map to original offset 7 (4 bytes of "4111" + 3 bytes
	// of the removed ZWSP), not 4.
	want := []int{0, 1, 2, 3, 7, 8, 9, 10}
	for i, off := range origOffsets {
		if off != want[i] {
			t.Errorf("origOffsets[%d] = %d, want %d", i, off, want[i])
		}
	}
}

func TestStripHiddenUnicodeRemovesEveryDocumentedRange(t *testing.T) {
	// One representative rune from each hiddenUnicodeRanges entry
	// (promptinjection.go) -- this helper must stay in exact sync with
	// that set, never define a second, inconsistent one.
	cases := []struct {
		name string
		r    rune
	}{
		{"zero_width_space", 0x200B},
		{"zero_width_joiner", 0x200D},
		{"bom", 0xFEFF},
		{"bidi_control", 0x202E},
		{"unicode_tag", 0xE0041},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := "a" + string(tc.r) + "b"
			stripped, _ := stripHiddenUnicode(text)
			if stripped != "ab" {
				t.Errorf("stripped = %q, want %q", stripped, "ab")
			}
		})
	}
}

func TestRemapMatchRoundTripsThroughStrip(t *testing.T) {
	zwsp := string(rune(0x200B))
	text := "prefix " + "4111" + zwsp + "1111" + " suffix"
	stripped, origOffsets := stripHiddenUnicode(text)

	strippedStart := len("prefix ")
	strippedEnd := strippedStart + len("41111111")

	start, end := remapMatch(origOffsets, strippedStart, strippedEnd)
	wantStart := len("prefix ")
	wantEnd := len(text) - len(" suffix")
	if start != wantStart || end != wantEnd {
		t.Errorf("remapMatch = (%d, %d), want (%d, %d)", start, end, wantStart, wantEnd)
	}
	if text[start:end] != "4111"+zwsp+"1111" {
		t.Errorf("text[start:end] = %q, want %q", text[start:end], "4111"+zwsp+"1111")
	}
	_ = stripped
}
