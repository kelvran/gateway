package dataplane

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Every client string that becomes a span name, a log field or an echoed
// pointer goes through boundedForTelemetry or boundedJoinWithin. Both must
// drop control characters (C0, DEL, C1 and the U+2028/U+2029 line separators)
// and invalid UTF-8, and cut on a rune boundary: a byte slice through a
// multi-byte rune makes an invalid string, and one invalid span attribute
// makes the OTLP exporter drop the whole batch (the 13a finding).

func TestBoundedForTelemetryDropsControlCharactersAndInvalidBytes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"gpt-4o", "gpt-4o"},
		{"claude-haiku-4-5", "claude-haiku-4-5"},
		{"gpt-4o\x1b[31mALERT\x1b[0m", "gpt-4o[31mALERT[0m"},
		{"a\nb\r\nc\td", "abcd"},
		{"x\x00y\x7fz", "xyz"},
		{"c1\u0085d\u009fe", "c1de"},
		{"line sep ok", "linesepok"},
		{"bad\xff\xfeutf8", "badutf8"},
		{"ünïcödé-stays", "ünïcödé-stays"},
	}
	for _, c := range cases {
		got := boundedForTelemetry(c.in)
		if got != c.want {
			t.Errorf("boundedForTelemetry(%q) = %q, want %q", c.in, got, c.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("boundedForTelemetry(%q) = %q is not valid UTF-8", c.in, got)
		}
	}
}

func TestBoundedForTelemetryCutsOnARuneBoundary(t *testing.T) {
	// 255 ASCII bytes then a 2-byte rune: a byte slice at 256 would keep
	// the rune's first byte alone.
	in := strings.Repeat("a", maxModelForTelemetry-1) + "é" + "tail"
	got := boundedForTelemetry(in)
	if !utf8.ValidString(got) {
		t.Fatalf("cut produced invalid UTF-8: %q", got[len(got)-4:])
	}
	if len(got) > maxModelForTelemetry {
		t.Errorf("len = %d, want <= %d", len(got), maxModelForTelemetry)
	}
	if want := strings.Repeat("a", maxModelForTelemetry-1); got != want {
		t.Errorf("got %d bytes ending %q, want the 255 a's and no partial rune", len(got), got[len(got)-3:])
	}
	// A 4-byte rune straddling the bound.
	in = strings.Repeat("b", maxModelForTelemetry-2) + "𝄞" + "tail"
	got = boundedForTelemetry(in)
	if !utf8.ValidString(got) || len(got) > maxModelForTelemetry || got != strings.Repeat("b", maxModelForTelemetry-2) {
		t.Errorf("4-byte straddle: got %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
	// Exactly at the bound with a multi-byte rune ending on it stays whole.
	in = strings.Repeat("c", maxModelForTelemetry-2) + "é"
	if got := boundedForTelemetry(in); got != in {
		t.Errorf("a string of exactly %d bytes ending in a 2-byte rune was cut: %d bytes", maxModelForTelemetry, len(got))
	}
}

func TestBoundedJoinWithinSanitisesItemsAndCutsOnARuneBoundary(t *testing.T) {
	got := boundedJoinWithin([]string{"/a\nb", "/c\x1b[2Jd", "/ok"}, 512)
	if got != "/ab,/c[2Jd,/ok" {
		t.Errorf("boundedJoinWithin = %q, want the control characters dropped and the items joined", got)
	}
	long := []string{strings.Repeat("é", 300)}
	got = boundedJoinWithin(long, 64)
	if !utf8.ValidString(got) {
		t.Errorf("cut produced invalid UTF-8: %q", got)
	}
	if len(got) > 64+len("…") {
		t.Errorf("len = %d, want the bound plus at most the cut marker", len(got))
	}
}
