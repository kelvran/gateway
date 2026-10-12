package dataplane

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// isDroppedForTelemetry is the rune set no client or upstream string may
// carry into a span name, a span or log field, or an echoed error message:
// every control character (unicode.IsControl: C0, DEL and C1), the
// U+2028/U+2029 line and paragraph separators a text sink treats as
// newlines, and utf8.RuneError -- what ranging over a string yields for
// every invalid byte (a literal U+FFFD goes with them; nothing legitimate
// carries one).
func isDroppedForTelemetry(r rune) bool {
	return unicode.IsControl(r) || r == utf8.RuneError || r == '\u2028' || r == '\u2029'
}

// dropControlRunes removes isDroppedForTelemetry's runes from s. An embedded
// newline, tab or ESC can then neither forge a second line in a plain-text
// log sink nor recolour an operator's terminal through an ANSI-rendering
// viewer (slog's JSON handler escapes them; a text handler does not), and
// the result is always valid UTF-8 -- the half no log handler covers: one
// invalid span attribute makes the OTLP exporter drop the whole batch
// (cmd/gateway/attribution.go's finding, 13a). strings.Map returns s itself
// when nothing is dropped, so clean ASCII costs no allocation.
func dropControlRunes(s string) string {
	return strings.Map(func(r rune) rune {
		if isDroppedForTelemetry(r) {
			return -1
		}
		return r
	}, s)
}

// truncateBytesOnRuneBoundary cuts s to at most maxBytes bytes without
// splitting a rune: a byte slice through a multi-byte rune manufactures the
// invalid UTF-8 dropControlRunes exists to keep out. s must be valid UTF-8.
func truncateBytesOnRuneBoundary(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
