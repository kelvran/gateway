package main

import (
	"fmt"
	"runtime"
)

// Build identity, injected by the release build through
//
//	go build -trimpath -ldflags "-s -w -X main.version=<semver> -X main.commit=<sha> -X main.date=<rfc3339>"
//
// — the same three -X names cmd/gateway/version.go binds, declared again
// here because ldflags symbols are per main package: gateway/.goreleaser.yaml's
// `cli` build and the Dockerfile's second `go build` set them for
// ./cmd/kelvran separately (RFC-3 decision 1). A plain `go build` keeps the
// defaults, so a development build identifies itself as exactly that.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// buildInfoString renders the single line `kelvran -version` prints:
// binary name, version, commit, build date, Go toolchain and target
// platform — the shape cmd/gateway prints, so the release acceptance job
// asserts both with the same `<binary> ${VERSION} (` prefix check.
func buildInfoString(version, commit, date string) string {
	return fmt.Sprintf("kelvran %s (%s, built %s, %s, %s/%s)",
		version, commit, date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
