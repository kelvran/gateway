package main

import (
	"fmt"
	"log/slog"
	"runtime"
)

// Build identity, injected by the release build through
//
//	go build -trimpath -ldflags "-s -w -X main.version=<semver> -X main.commit=<sha> -X main.date=<rfc3339>"
//
// (see gateway/Dockerfile's ARG VERSION/COMMIT/DATE and the release workflow).
// A plain `go build` or `go run` keeps the defaults, so a development build
// identifies itself as exactly that instead of printing an empty string or a
// stale number. The variable NAMES are a contract with those build scripts;
// renaming one silently turns every release back into "dev".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// buildInfoString renders the single-line identity printed by -version and
// logged as build_info at startup: binary name, version, commit, build date,
// Go toolchain and target platform. Pure so it is unit-testable without
// running the binary.
func buildInfoString(version, commit, date string) string {
	return fmt.Sprintf("kelvran-gateway %s (%s, built %s, %s, %s/%s)",
		version, commit, date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// logBuildInfo emits the one structured build_info record a fleet operator
// needs to spot version skew across replicas (the same fields -version
// prints, as separate attributes rather than one string).
func logBuildInfo(logger *slog.Logger) {
	logger.Info("build_info",
		"version", version,
		"commit", commit,
		"date", date,
		"go_version", runtime.Version(),
		"platform", runtime.GOOS+"/"+runtime.GOARCH,
	)
}
