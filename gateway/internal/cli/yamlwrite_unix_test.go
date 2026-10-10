//go:build unix

package cli

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestWriteInPlaceRefusesANonRegularTarget(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "config.yaml")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if _, _, err := writeInPlace(fifo, []byte("x")); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a FIFO target must be refused: %v", err)
	}
}
