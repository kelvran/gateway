package guardrail

import (
	"strings"
	"unicode/utf8"
)

// stripHiddenUnicode removes every character in hiddenUnicodeRanges (defined
// once in promptinjection.go — reused here verbatim, never a second,
// possibly-inconsistent definition of "invisible Unicode") from text,
// returning the stripped copy plus a byte-offset mapping back into the
// ORIGINAL text: origOffsets[i] is text's own byte offset of the byte that
// ended up at position i in stripped. len(origOffsets) == len(stripped).
//
// This exists because a contiguous-run regex (credit card/SSN/IBAN/phone —
// any detector matching a \d-run or similarly tight character class with no
// tolerance for an injected character) is trivially evaded by inserting one
// zero-width space (or any other hiddenUnicodeRanges character) mid-run: the
// character class doesn't allow it, so the match breaks entirely and the
// detector finds nothing, per evals/tests/fixtures/regression_corpus_
// guardrail.json's regcorpus-guardrail-23 case. A vulnerable detector must
// run its regex against stripHiddenUnicode's stripped text, then remap every
// match's offsets back to the original text via remapMatch — never match
// against text directly, and never report a stripped-text offset as a
// Finding.Start/End, which by contract (see Finding's own doc comment) must
// always index into the ORIGINAL, unstripped text.
//
// When text has no hidden-Unicode characters at all (the overwhelmingly
// common case), stripped == text and origOffsets is the identity mapping
// (origOffsets[i] == i for every i) — so a detector switched to this
// mechanism is byte-for-byte behavior-identical to before on every clean
// input; only text actually containing an evasion attempt changes behavior.
func stripHiddenUnicode(text string) (stripped string, origOffsets []int) {
	var b strings.Builder
	origOffsets = make([]int, 0, len(text))
	for i, r := range text {
		if isHiddenUnicode(r) {
			continue
		}
		width := utf8.RuneLen(r)
		if width < 0 {
			// Invalid UTF-8 byte: the range loop already advanced by
			// exactly one byte and reported it as utf8.RuneError.
			width = 1
		}
		b.WriteString(text[i : i+width])
		for k := 0; k < width; k++ {
			origOffsets = append(origOffsets, i+k)
		}
	}
	return b.String(), origOffsets
}

// remapMatch converts a [start, end) byte-offset pair found in some
// stripHiddenUnicode(text)'s stripped output back to the equivalent [start,
// end) pair in that same call's ORIGINAL text, via its origOffsets mapping.
// end is remapped as origOffsets[end-1]+1 (one past the ORIGINAL position of
// the match's own last byte), deliberately not origOffsets[end]: one or more
// hidden-Unicode characters stripped out immediately after the match's last
// byte would otherwise be silently folded into the reported span, and
// origOffsets has no entry at index len(origOffsets) to look up regardless.
// Callers must only pass a non-empty match (end > start); every detector
// using this package's regexes always matches at least one character.
func remapMatch(origOffsets []int, start, end int) (int, int) {
	return origOffsets[start], origOffsets[end-1] + 1
}
