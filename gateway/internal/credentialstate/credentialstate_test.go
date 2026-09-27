package credentialstate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFilesHasAnyFalseOnZeroValue(t *testing.T) {
	if (Files{}).HasAny() {
		t.Error("HasAny on zero-value Files = true, want false")
	}
}

func TestFilesHasAnyTrueWhenAnySingleFieldSet(t *testing.T) {
	cases := []Files{
		{APIKey: "x"},
		{AccessKeyID: "x"},
		{SecretAccessKey: "x"},
		{SessionToken: "x"},
	}
	for _, f := range cases {
		if !f.HasAny() {
			t.Errorf("HasAny(%+v) = false, want true", f)
		}
	}
}

func TestNewStateSeedsInitialValue(t *testing.T) {
	initial := Credentials{AccessKeyID: "AKIA...", SecretAccessKey: "secret"}
	state := NewState(initial)
	got := state.Load()
	if got == nil || *got != initial {
		t.Errorf("state.Load() = %+v, want %+v", got, initial)
	}
}

func TestReadFileTrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("  the-real-value\n\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got != "the-real-value" {
		t.Errorf("ReadFile = %q, want %q", got, "the-real-value")
	}
}

func TestReadFileErrorsOnMissingPath(t *testing.T) {
	if _, err := ReadFile(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Error("ReadFile on a missing path = nil error, want an error")
	}
}
