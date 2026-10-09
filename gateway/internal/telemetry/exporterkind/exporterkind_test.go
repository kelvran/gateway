package exporterkind

import (
	"strings"
	"testing"
)

// The exporter-name vocabulary is public surface: docs/reference/config.md
// and docs/how-to/troubleshooting.md name the three values and quote the
// startup error verbatim, so the set and its rendering are pinned here.

func TestNamesAreTheThreeDocumentedValuesInOrder(t *testing.T) {
	want := []string{"stdout", "otlp", "none"}
	if len(Names) != len(want) {
		t.Fatalf("Names = %v, want %v", Names, want)
	}
	for i := range want {
		if Names[i] != want[i] {
			t.Errorf("Names[%d] = %q, want %q", i, Names[i], want[i])
		}
	}
	if Stdout != "stdout" || OTLP != "otlp" || None != "none" {
		t.Errorf("constants = (%q, %q, %q), want (stdout, otlp, none)", Stdout, OTLP, None)
	}
}

// TestNamesHaveNoDuplicates guards the hand-maintained list: a repeated
// name would make WantList render it twice in the startup error.
func TestNamesHaveNoDuplicates(t *testing.T) {
	seen := make(map[string]bool, len(Names))
	for _, n := range Names {
		if seen[n] {
			t.Errorf("Names lists %q more than once: %v", n, Names)
		}
		seen[n] = true
	}
}

func TestValidAcceptsEveryNameAndTheEmptyDefault(t *testing.T) {
	for _, name := range append([]string{""}, Names...) {
		if !Valid(name) {
			t.Errorf("Valid(%q) = false, want true", name)
		}
	}
}

func TestValidRejectsAnythingElse(t *testing.T) {
	for _, name := range []string{"carrier-pigeon", "STDOUT", "Otlp", " none", "none ", "stdout,otlp"} {
		if Valid(name) {
			t.Errorf("Valid(%q) = true, want false (no case folding, no trimming)", name)
		}
	}
}

func TestWantListRendersTheDocumentedPhrase(t *testing.T) {
	// docs/how-to/troubleshooting.md quotes the startup error as
	// `telemetry: unknown exporter "x" (want "stdout", "otlp", or "none")`.
	if got := WantList(); got != `"stdout", "otlp", or "none"` {
		t.Errorf("WantList() = %s, want the documented phrase", got)
	}
	if strings.Count(WantList(), `"`) != 2*len(Names) {
		t.Errorf("every name must be quoted exactly once: %s", WantList())
	}
}
