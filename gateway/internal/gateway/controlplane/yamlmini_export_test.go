package controlplane

import "testing"

// The exported helpers must be the loader's own, byte for byte in behaviour:
// the CLI's offline writer classifies lines through them.
func TestExportedYAMLHelpersAreTheLoaders(t *testing.T) {
	for _, line := range []string{`key: "value" # note`, "  nested:", "# only a comment", `"my:key": 1`, "plain"} {
		if got, want := StripYAMLComment(line), stripYAMLComment(line); got != want {
			t.Errorf("StripYAMLComment(%q) = %q, want %q", line, got, want)
		}
		content := StripYAMLComment(line)
		if got, want := FindKeyColon(content), findKeyColon(content); got != want {
			t.Errorf("FindKeyColon(%q) = %d, want %d", content, got, want)
		}
	}
	for _, s := range []string{`"quoted"`, `'single'`, `bare`, `"mis'matched`, `""`} {
		if got, want := UnquoteYAMLScalar(s), unquoteYAMLScalar(s); got != want {
			t.Errorf("UnquoteYAMLScalar(%q) = %q, want %q", s, got, want)
		}
	}
}
