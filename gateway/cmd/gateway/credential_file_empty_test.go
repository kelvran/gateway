package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadCredentialFileOrWarnTreatsAnEmptyFileAsUnreadable: at startup an
// empty *_file must produce the same warning an unreadable one does. Before
// 2026-10-08 an empty file was accepted silently and the deployment started
// with an empty credential, while the *_env branch right next to it warned
// for the same condition.
func TestReadCredentialFileOrWarnTreatsAnEmptyFileAsUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	if got := readCredentialFileOrWarn(logger, "dep-a", "api_key", path); got != "" {
		t.Errorf("readCredentialFileOrWarn on an empty file = %q, want empty", got)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "credential file") {
		t.Errorf("want a startup warning for an empty credential file, got logs: %q", logs.String())
	}
}
