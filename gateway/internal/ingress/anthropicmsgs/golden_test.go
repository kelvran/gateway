package anthropicmsgs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// checkGolden compares got with testdata/<name>; KELVRAN_INGRESS_UPDATE_GOLDEN=1
// rewrites it (the cli package's KELVRAN_CLI_UPDATE_GOLDEN convention).
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("KELVRAN_INGRESS_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // G306: a committed golden file
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: the golden under testdata
	if err != nil {
		t.Fatalf("golden %s missing (%v); run once with KELVRAN_INGRESS_UPDATE_GOLDEN=1", name, err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
		t.Errorf("golden %s differs:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

// mustReadTestdata reads a fixture, failing the test on error.
func mustReadTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: a fixture under testdata
	if err != nil {
		t.Fatalf("reading testdata/%s: %v", name, err)
	}
	return data
}

// prettyJSON re-indents JSON for stable goldens, keeping the member order
// the encoder produced so a golden pins the wire bytes, not a sorted view.
func prettyJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		t.Fatalf("golden input is not JSON: %v\n%s", err, raw)
	}
	return append(buf.Bytes(), '\n')
}
