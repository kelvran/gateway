package credentialstate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadFileReportsAnEmptyOrWhitespaceOnlyFileAsErrEmptyCredentialFile(t *testing.T) {
	cases := map[string]string{"zero bytes": "", "whitespace only": " \n\t"}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			got, err := ReadFile(path)
			if !errors.Is(err, ErrEmptyCredentialFile) {
				t.Fatalf("ReadFile = (%q, %v), want an error wrapping ErrEmptyCredentialFile", got, err)
			}
			if got != "" {
				t.Errorf("ReadFile value = %q on an empty file, want empty", got)
			}
		})
	}
}
