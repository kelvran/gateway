package main

import (
	"runtime"
	"strings"
	"testing"
)

// The three tests mirror cmd/gateway/version_test.go one-for-one (RFC-3
// decision 1): the CLI is a second main package with its own copy of the
// ldflags contract, so each half is pinned on its own.

func TestBuildInfoStringDefaults(t *testing.T) {
	got := buildInfoString("dev", "none", "unknown")
	for _, want := range []string{"kelvran dev", "(none", "built unknown", runtime.Version(), runtime.GOOS + "/" + runtime.GOARCH} {
		if !strings.Contains(got, want) {
			t.Errorf("buildInfoString(dev, none, unknown) = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("buildInfoString must be a single line, got %q", got)
	}
	if strings.HasPrefix(got, "kelvran-gateway") {
		t.Errorf("the CLI must identify itself as kelvran, not kelvran-gateway: %q", got)
	}
}

func TestBuildInfoStringInjected(t *testing.T) {
	got := buildInfoString("0.18.0", "57fd9f8d", "2026-10-08T05:03:00Z")
	want := "kelvran 0.18.0 (57fd9f8d, built 2026-10-08T05:03:00Z, " + runtime.Version() + ", " + runtime.GOOS + "/" + runtime.GOARCH + ")"
	if got != want {
		t.Errorf("buildInfoString = %q, want %q", got, want)
	}
}

func TestBuildInfoVariablesDefaultToDev(t *testing.T) {
	if version != "dev" || commit != "none" || date != "unknown" {
		t.Errorf("defaults = (%q, %q, %q), want (dev, none, unknown)", version, commit, date)
	}
}
