package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// One Go toolchain (round-5 item 1, 2026-10-15): the Go version the suite,
// the linters and the release binaries build with is the version the
// container image builds with, and it is declared once. The declarations
// are read from the tree, not from runtime.Version(): CI's actions/setup-go
// reads gateway/go.mod (go-version-file) and then exports GOTOOLCHAIN=local,
// and the official golang image sets GOTOOLCHAIN=local too, so a go.mod
// ahead of the image fails `go mod download` inside `docker build` instead
// of fetching a toolchain, and an image ahead of go.mod ships a standard
// library the suite never ran on. Dependabot moves the image on its own
// (docker ecosystem), nothing moves the go directive, so the two drift
// unless something fails. scripts/release-preflight.sh check 7 repeats this
// at the tag, in a job that runs no setup-go (the check is pure shell).
const (
	gatewayGoModPath = "../../go.mod"
	compatGoModPath  = "../../compat/go.mod"
	dockerfilePath   = "../../Dockerfile"
	workflowsDir     = "../../../.github/workflows"
)

var (
	// goDirective: the `go X.Y.Z` line of a go.mod, alone on its line (a CRLF
	// checkout is tolerated). The patch component is required so the
	// directive can equal the image tag; `go 1.27` would be a floor, not a
	// version, and `go 1.27.2 // note` is refused so the line reads the same
	// way the preflight's sed reads it.
	goDirective = regexp.MustCompile(`(?m)^go (\d+\.\d+\.\d+)[ \t\r]*$`)
	// toolchainDirective is refused outright: actions/setup-go installs a
	// toolchain line in preference to the go line, while the GOTOOLCHAIN=local
	// image only enforces the go line, so the directive would re-split CI
	// from the image without either version above changing.
	toolchainDirective = regexp.MustCompile(`(?m)^toolchain\b`)
	// builderImage: every FROM line that builds on a golang image. The tag may
	// carry an Alpine release suffix (-alpine3.22); the digest pin after the
	// tag is Scorecard's business, not this test's.
	builderImage = regexp.MustCompile(`(?m)^FROM .*\bgolang:(\d+\.\d+\.\d+)-alpine`)
	// setupGoStep / literalGoVersion / goVersionFile: every actions/setup-go
	// step (quoted or not) reads gateway/go.mod, and no workflow pins a
	// literal anywhere.
	setupGoStep      = regexp.MustCompile(`(?m)^\s*-?\s*uses: ["']?actions/setup-go@`)
	literalGoVersion = regexp.MustCompile(`(?m)^\s*go-version:`)
	goVersionFile    = regexp.MustCompile(`(?m)^\s*go-version-file: gateway/go\.mod[ \t\r]*$`)
)

// goDirectiveOf returns the go directive of the go.mod at path; name is the
// repository-relative spelling the messages use.
func goDirectiveOf(t *testing.T, path, name string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is one of the two fixed go.mod constants above
	if err != nil {
		t.Fatal(err)
	}
	if toolchainDirective.Match(b) {
		t.Errorf("%s declares a toolchain directive: actions/setup-go would install that version while the GOTOOLCHAIN=local image builds with the go directive -- delete it and move the go line instead", name)
	}
	m := goDirective.FindAllSubmatch(b, -1)
	if len(m) != 1 {
		t.Fatalf("%s: want exactly one `go X.Y.Z` line (a patch version, so it can equal the image tag), found %d -- a two-part version or a trailing comment does not count", name, len(m))
	}
	return string(m[0][1])
}

// workflowFiles returns every workflow GitHub would run: *.yml and *.yaml.
func workflowFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, pattern := range []string{"*.yml", "*.yaml"} {
		matches, err := filepath.Glob(filepath.Join(workflowsDir, pattern))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, matches...)
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatalf("no workflows under %s: the workflows moved?", workflowsDir)
	}
	return files
}

// TestOneGoToolchain: gateway/go.mod, gateway/compat/go.mod and every golang
// builder line of the Dockerfile name one Go version, neither go.mod carries
// a toolchain directive, and every workflow's setup-go step reads the
// version from gateway/go.mod.
func TestOneGoToolchain(t *testing.T) {
	want := goDirectiveOf(t, gatewayGoModPath, "gateway/go.mod")
	if got := goDirectiveOf(t, compatGoModPath, "gateway/compat/go.mod"); got != want {
		t.Errorf("gateway/compat/go.mod declares go %s, gateway/go.mod declares go %s: one toolchain", got, want)
	}

	df, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatal(err)
	}
	images := builderImage.FindAllSubmatch(df, -1)
	if len(images) == 0 {
		t.Fatalf("gateway/Dockerfile: no `FROM ... golang:X.Y.Z-alpine` line; the builder stage moved?")
	}
	for _, m := range images {
		if got := string(m[1]); got != want {
			t.Errorf("gateway/Dockerfile builds with golang:%s-alpine, gateway/go.mod declares go %s: the image sets GOTOOLCHAIN=local, so the two must be one version", got, want)
		}
	}

	steps := 0
	for _, wf := range workflowFiles(t) {
		b, err := os.ReadFile(wf) //nolint:gosec // G304: wf comes from the fixed workflows glob above
		if err != nil {
			t.Fatal(err)
		}
		name := ".github/workflows/" + filepath.Base(wf)
		if literalGoVersion.Match(b) {
			t.Errorf("%s pins a literal go-version; every setup-go step reads `go-version-file: gateway/go.mod`", name)
		}
		n := len(setupGoStep.FindAll(b, -1))
		if n == 0 {
			continue
		}
		steps += n
		if got := len(goVersionFile.FindAll(b, -1)); got != n {
			t.Errorf("%s: %d setup-go step(s) but %d `go-version-file: gateway/go.mod` line(s)", name, n, got)
		}
	}
	if steps == 0 {
		t.Fatalf("no actions/setup-go step under %s: the workflows moved?", workflowsDir)
	}
}
