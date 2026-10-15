# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `gateway` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.2.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) — load-bearing for the Go module path/tag (`v0.1.0`, `v0.2.0`, ...).

## Added

## Changed

- One Go toolchain: `gateway/go.mod` and `gateway/compat/go.mod` declare `go 1.27.2`, the version `gateway/Dockerfile`'s builder (`golang:1.27.2-alpine`) has used since 2026-10-09, so the `-race` suite, `golangci-lint`, CodeQL, `govulncheck` and the release binaries now run on the Go minor the container image ships (before, they ran on `go 1.26.9`). Every workflow's `actions/setup-go` step reads the version from `gateway/go.mod` (`go-version-file`, as `release.yml` already did) instead of a literal; `TestOneGoToolchain` (`gateway/cmd/gateway`) fails on every push, and `scripts/release-preflight.sh` check 7 at the tag, when the two `go.mod` files and the Dockerfile tag differ or a workflow pins a literal again. Not public surface (`docs/VERSIONING.md` § 2): the Go field of `-version` and the `build_info` log moves from `go1.26.9` to `go1.27.2` for the release packages (the image already said `go1.27.2`); request handling is unchanged: Go 1.27 enables `GOEXPERIMENT=jsonv2` by default, so `encoding/json` is now implemented on `encoding/json/v2` with the v1 option set (duplicate members, invalid UTF-8 and case-insensitive field matching are accepted as before, per the package doc; only decoder error wording may differ) — the implementation the container image has shipped since `gateway/v0.13.0` (the first release whose image built on a Go 1.27 tag, `golang:1.27.1-alpine`) and that the `-race` suite now runs on too, while `gateway/v0.20.0` and earlier packages used the classic implementation. The `go` directive also moves the binaries to Go 1.27's `GODEBUG` defaults: `tracebacklabels` (a no-op here, the gateway sets no goroutine labels) and, for the darwin and windows packages only, `x509sslcertoverrideplatform` (`crypto/x509` honours `SSL_CERT_FILE`/`SSL_CERT_DIR` instead of the platform store); the linux packages and the image are unaffected.

## Deprecated

## Removed

## Fixed

## Security
