package main

import (
	"runtime"
	"strings"
	"testing"
)

// TestBuildInfoStringDefaults pins the development-build string: without
// -ldflags the three package-level variables keep their defaults, so
// `go run ./cmd/gateway -version` must say so plainly instead of printing
// an empty or misleading version.
func TestBuildInfoStringDefaults(t *testing.T) {
	got := buildInfoString("dev", "none", "unknown")
	for _, want := range []string{"kelvran-gateway dev", "(none", "built unknown", runtime.Version(), runtime.GOOS + "/" + runtime.GOARCH} {
		if !strings.Contains(got, want) {
			t.Errorf("buildInfoString(dev, none, unknown) = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("buildInfoString must be a single line, got %q", got)
	}
}

// TestBuildInfoStringInjected pins the release-build string, where the
// release workflow injects the tag-derived version, the commit and the
// build date through -X main.version/-X main.commit/-X main.date.
func TestBuildInfoStringInjected(t *testing.T) {
	got := buildInfoString("0.18.0", "57fd9f8d", "2026-10-08T05:03:00Z")
	want := "kelvran-gateway 0.18.0 (57fd9f8d, built 2026-10-08T05:03:00Z, " + runtime.Version() + ", " + runtime.GOOS + "/" + runtime.GOARCH + ")"
	if got != want {
		t.Errorf("buildInfoString = %q, want %q", got, want)
	}
}

// TestBuildInfoVariablesDefaultToDev guards the ldflags contract: the
// variable NAMES are what the Dockerfile and release workflow reference
// (-X main.version etc.), and their defaults must read as a dev build.
func TestBuildInfoVariablesDefaultToDev(t *testing.T) {
	if version != "dev" || commit != "none" || date != "unknown" {
		t.Errorf("defaults = (%q, %q, %q), want (dev, none, unknown)", version, commit, date)
	}
}
