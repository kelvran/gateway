# Release artifacts

This page lists exactly what a Kelvran release consists of: the archives, packages, SBOMs, checksums, Sigstore bundle, provenance attestation and container image tags that a `gateway/vX.Y.Z` or `evals/vX.Y.Z` tag produces, the pipeline stages that produce them, and the commands that verify each artifact. It is for operators who install or pin a release and for maintainers who cut one. The procedure for cutting a release is in [RELEASE.md](../../RELEASE.md); the version policy is in [docs/VERSIONING.md](../VERSIONING.md); the image's own reference card is [container-image.md](./container-image.md).

## Status as of 2026-10-08

| Item | State |
|---|---|
| Newest gateway tag | `gateway/v0.17.0` (2026-10-07) |
| Newest evals tag | `evals/v0.10.1` (2026-09-22) |
| Gateway release pipeline (`.github/workflows/release.yml`, `gateway/.goreleaser.yaml`, `deploy/nfpm/postinstall.sh`, `scripts/release-preflight.sh`) | On main since 2026-10-08, not in `gateway/v0.17.0` |
| Evals release pipeline (`.github/workflows/release-evals.yml`) | On main since 2026-10-08, not in `evals/v0.10.1` |
| Release assets on any existing Release | None. Every Release at or before `gateway/v0.17.0` and `evals/v0.10.1` carries zero assets. Tags that predate the pipeline have no `.goreleaser.yaml` or preflight script in their tree and cannot be rebuilt by it (`release.yml` header). The first `gateway/v*` and `evals/v*` tags pushed after 2026-10-08 are the first Releases that carry assets |
| Container image `ghcr.io/kelvran/gateway` | Published on every push to main since 2026-09-13. Multi-platform (`linux/amd64` + `linux/arm64`) for every image built on or after 2026-10-08 (on main since 2026-10-08, not in `gateway/v0.17.0`); the `:v0.17.0` image and earlier are `linux/amd64` only |

## Naming

| Element | Form | Example | Source |
|---|---|---|---|
| Gateway git tag | `gateway/vX.Y.Z[-pre]`, matching `^gateway/v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$` | `gateway/v0.17.0` | `release.yml` preflight job |
| Evals git tag | `evals/vX.Y.Z[-pre]`, matching `^evals/v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$` | `evals/v0.10.1` | `release-evals.yml` preflight job |
| Version | The tag without its `gateway/v` or `evals/v` prefix | `0.17.0` | `VERSION="${TAG#gateway/v}"` |
| Pre-release | Any version containing `-`; the Release is flagged `--prerelease` | `0.18.0-rc.1` | `case "$VERSION" in *-*)` |
| Gateway archive | `kelvran-gateway_<version>_<os>_<arch>.tar.gz` (`.zip` for windows) | `kelvran-gateway_0.18.0_linux_amd64.tar.gz` | `gateway/.goreleaser.yaml` `archives.name_template` |
| Gateway package | `kelvran-gateway_<version>_linux_<arch>.<deb\|rpm\|apk>` | `kelvran-gateway_0.18.0_linux_amd64.deb` | `release.yml` acceptance job |
| SBOM | `<asset filename>.sbom.cdx.json` | `kelvran-gateway_0.18.0_linux_amd64.deb.sbom.cdx.json` | `release.yml` publish job |
| Checksums | `checksums.txt` | | `gateway/.goreleaser.yaml` `checksum.name_template`; rewritten in `release.yml` |
| Sigstore bundle | `checksums.txt.sigstore.json` | | `cosign sign-blob --bundle` |
| Image tags | `:v<X.Y.Z>`, `:sha-<40-hex commit>`, `:latest` | `ghcr.io/kelvran/gateway:v0.17.0` | `ci.yml` publish-image job |
| Evals sdist and wheel | `kelvran_evals-<version>*` (the workflow checks this prefix); `kelvran-evals` is a pure-Python hatchling project, so the files are `kelvran_evals-<version>.tar.gz` and `kelvran_evals-<version>-py3-none-any.whl` | | `release-evals.yml` build step; `evals/pyproject.toml` |
| GitHub Release title | `gateway v<version>` or `evals v<version>` | `gateway v0.18.0` | `gh release create --title` |
| GitHub Release body | `gateway/changelog/<version>.md` or `evals/changelog/<version>.md` | | `--notes-file` |

Asset names drop the `gateway/` prefix and the `v`; image tags drop only `gateway/` and keep the `v` (`gateway/v0.17.0` becomes `:v0.17.0`). The binary's `-version` prints the bare version. The `gateway/` tag prefix is permanent: the Go module path is `github.com/kelvran/gateway/gateway` (`gateway/go.mod`), one directory below the repository root, and Go's nested-module rule requires the tag to carry that directory as a prefix.

## Gateway release: asset inventory

One `gateway/vX.Y.Z` tag produces the following set. Every file except `checksums.txt.sigstore.json` is a subject of the provenance attestation; every `kelvran-gateway_*` file is listed in `checksums.txt`.

| Asset | Count | Platforms | Contents |
|---|---|---|---|
| `kelvran-gateway_<version>_<os>_<arch>.tar.gz` | 4 | `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64` | `kelvran-gateway` binary, `config.example.yaml`, `LICENSE`, `NOTICE` |
| `kelvran-gateway_<version>_windows_amd64.zip` | 1 | `windows/amd64` | `kelvran-gateway.exe`, `config.example.yaml`, `LICENSE`, `NOTICE` |
| `kelvran-gateway_<version>_linux_<arch>.deb` | 2 | `linux/amd64`, `linux/arm64` | See "Package layout" below |
| `kelvran-gateway_<version>_linux_<arch>.rpm` | 2 | `linux/amd64`, `linux/arm64` | Same as the deb |
| `kelvran-gateway_<version>_linux_<arch>.apk` | 2 | `linux/amd64`, `linux/arm64` | Same as the deb |
| `<asset>.sbom.cdx.json` | 11 | One per archive and package | CycloneDX JSON, produced by `syft scan file:<asset>` |
| `checksums.txt` | 1 | | `sha256sum` lines for every `kelvran-gateway_*` file, SBOMs included |
| `checksums.txt.sigstore.json` | 1 | | Sigstore bundle from `cosign sign-blob` (keyless, GitHub OIDC) |

`windows/arm64` is excluded (`ignore` in `gateway/.goreleaser.yaml`). Only `./cmd/gateway` is built; `kelvran-bench` is not a release asset. The SLSA Build Level 2 provenance attestation produced by `actions/attest-build-provenance` is not a Release asset: GitHub stores it against the repository, keyed by each subject's digest, and `gh attestation verify <asset> -R kelvran/gateway` retrieves it.

### Binary build parameters

| Parameter | Value |
|---|---|
| Binary name | `kelvran-gateway` |
| Main package | `./cmd/gateway` (within `gateway/`) |
| `CGO_ENABLED` | `0` |
| Flags | `-trimpath` |
| ldflags | `-s -w -X main.version={{ .Version }} -X main.commit={{ .Commit }} -X main.date={{ .Date }}` |
| Go toolchain | `go 1.26.9`, the `go` directive of `gateway/go.mod`, resolved by `actions/setup-go` (`go-version-file`). The container image is built with `golang:1.27.1-alpine` instead (see "Image contents"), so the `<go version>` field of `-version` differs between a package and the image of the same release |
| GoReleaser mode | `release --snapshot --clean --skip=publish,validate`, `GORELEASER_CURRENT_TAG` set to the pushed tag |
| Version source in snapshot mode | `snapshot.version_template: '{{ trimprefix (trimprefix .Tag "gateway/") "v" }}'` |
| GoReleaser changelog | Disabled (`changelog.disable: true`); the Release body is the dated changelog file |

The three ldflags variable names (`main.version`, `main.commit`, `main.date`) are a contract between `gateway/cmd/gateway/version.go` and the build scripts; a plain `go build` leaves them at `dev`, `none`, `unknown`.

### Package layout (deb, rpm, apk)

| Path | Type | Source |
|---|---|---|
| `/usr/bin/kelvran-gateway` | binary | `bindir: /usr/bin` |
| `/usr/lib/systemd/system/kelvran-gateway.service` | file | [deploy/systemd/kelvran-gateway.service](../../deploy/systemd/kelvran-gateway.service) |
| `/etc/kelvran-gateway/config.example.yaml` | `config\|noreplace` (a package upgrade never overwrites a modified copy) | [gateway/config.example.yaml](../../gateway/config.example.yaml) |
| `/usr/share/doc/kelvran-gateway/LICENSE` | file | repository `LICENSE` (Apache-2.0) |
| `/usr/share/doc/kelvran-gateway/NOTICE` | file | repository `NOTICE` |

| Package metadata | Value |
|---|---|
| `package_name` | `kelvran-gateway` |
| `vendor` | `Kelvran` |
| `homepage` | `https://github.com/kelvran/gateway` |
| `maintainer` | `Kelvran maintainers <noreply@github.com>` |
| `description` | `Self-hosted Go LLM gateway with a risk-gated response cache.` |
| `license` | `Apache-2.0` |
| `section` / `priority` | `net` / `optional` |
| Post-install script | `deploy/nfpm/postinstall.sh` |
| Embedded package signature | None. `gateway/.goreleaser.yaml` configures no deb, rpm or apk signing; integrity comes from `checksums.txt`, its Sigstore bundle and the provenance attestation. `apk add` therefore needs `--allow-untrusted` |

The post-install script runs only when `systemctl` is present: `systemctl daemon-reload`, then `systemctl try-restart kelvran-gateway.service`. `try-restart` restarts a running service (an upgrade picks up the new binary) and does nothing to a stopped one. The package never enables or starts the service, because the gateway has no usable default config: the operator creates `/etc/kelvran-gateway/config.yaml` first. The acceptance job fails the release if the package enabled or started the unit.

### systemd unit (as shipped in the packages)

| Directive | Value |
|---|---|
| `Type` | `simple` |
| `DynamicUser` | `yes` |
| `StateDirectory` | `kelvran-gateway` (creates `/var/lib/kelvran-gateway`, the only writable location under `ProtectSystem=strict`) |
| `ConfigurationDirectory` | `kelvran-gateway` |
| `EnvironmentFile` | `-/etc/kelvran-gateway/env` (optional; provider credentials, one `KEY=value` per line) |
| `ExecStartPre` | `/usr/bin/kelvran-gateway -config /etc/kelvran-gateway/config.yaml -validate` |
| `ExecStart` | `/usr/bin/kelvran-gateway -config /etc/kelvran-gateway/config.yaml` |
| `Restart` / `RestartSec` | `on-failure` / `2s` |
| `KillSignal` | `SIGTERM` |
| `TimeoutStopSec` | `60s` (the acceptance job greps for exactly this line) |
| Hardening | `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `PrivateDevices`, `ProtectKernelTunables`, `ProtectKernelModules`, `ProtectKernelLogs`, `ProtectControlGroups`, `ProtectClock`, `ProtectHostname`, `RestrictSUIDSGID`, `RestrictRealtime`, `RestrictNamespaces`, `LockPersonality`, `MemoryDenyWriteExecute`, `SystemCallArchitectures=native`, `SystemCallFilter=@system-service` then `~@privileged`, empty `CapabilityBoundingSet` and `AmbientCapabilities`, `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX` |
| `WantedBy` | `multi-user.target` |

With an empty `CapabilityBoundingSet` the unit cannot bind a port below 1024. `listen_addr` is a required key with no default (`controlplane.Load` refuses a config without it); the shipped `config.example.yaml` sets `":8080"`, which is the port the acceptance job probes.

## Gateway release: pipeline

`.github/workflows/release.yml` runs four jobs in sequence. Top-level `permissions` is `{}`; each job grants itself only what it needs. The concurrency group is `release-<tag>` for a tag push or a dispatched tag, and `release-<branch>` for a dry run dispatched without a tag (`release-${{ inputs.tag || github.ref_name }}`); `cancel-in-progress: false` in both cases.

### Triggers and modes

| Event | Inputs | Mode | Version | Release created |
|---|---|---|---|---|
| `push` of a tag matching `gateway/v*` | | Real release | Tag minus `gateway/v` | Yes |
| `workflow_dispatch` | `tag` empty, `publish=false` (defaults) | Dry run of the dispatching ref | `0.0.0-dryrun.<first 12 hex of the commit>` | No; assets stay in the `gateway-release-assets` artifact |
| `workflow_dispatch` | `tag` set, `publish=false` | Dry run of that existing tag | Tag minus `gateway/v` | No |
| `workflow_dispatch` | `tag` set, `publish=true` | Build that existing tag and attach assets | Tag minus `gateway/v` | Yes, if the Release has zero assets or does not exist; the run fails if it already has assets |
| `workflow_dispatch` | `tag` empty, `publish=true` | Rejected (`publish=true needs a tag`) | | |

A dry run still signs and attests for real, so it leaves a public Rekor entry and an attestation for a build nobody publishes. A tag that does not match the regex fails the preflight job before anything is checked out.

### Jobs

| Job | Permissions | What it does |
|---|---|---|
| `preflight` | `contents: read` | Resolves ref, tag, version, `prerelease`, `dry_run`; checks out the ref; runs `scripts/release-preflight.sh gateway "$VERSION"` (`continue-on-error` on a dry run) |
| `build` | `contents: read` | Full-history checkout (`fetch-depth: 0`); on a real release, fails unless `HEAD` is the tag's commit; resolves the previous `gateway/v*` tag for `GORELEASER_PREVIOUS_TAG`; runs GoReleaser in snapshot mode from `gateway/`; deletes per-platform build directories and `config.yaml` from `gateway/dist/`; uploads the artifact `gateway-dist` (7 days) |
| `acceptance` | `{}` | Downloads `gateway-dist`; installs the amd64 deb and runs the checks in the next table |
| `publish` | `contents: write`, `id-token: write`, `attestations: write` | Generates SBOMs, rewrites `checksums.txt`, signs it, attests provenance, uploads the artifact `gateway-release-assets` (14 days), creates or updates the Release unless `dry_run` |

### Preflight checks (`scripts/release-preflight.sh <gateway|evals> <version>`)

| # | Check | Severity |
|---|---|---|
| 1 | `<deployable>/changelog/<version>.md` exists and is not empty | FAIL |
| 2 | `<deployable>/changelog/unreleased.md` holds no list entries (no line matching `^\s*[-*] `) | FAIL |
| 3 | Every `**BREAKING**` entry (matched case-insensitively) in the dated changelog has a table row in [UPGRADE.md](../../UPGRADE.md) of the form `| `<deployable>/v<version>` |` | FAIL |
| 4 | Evals only: `evals/pyproject.toml` declares exactly `<version>`, and `scripts/check_versions.py` passes | FAIL |
| 5 | The support-window table in [docs/VERSIONING.md](../VERSIONING.md) lists `<deployable>/v<version>` as the latest minor | WARN only (a `::warning::` annotation under GitHub Actions) |

Exit codes: `0` when every check passes; `1` with one `preflight FAIL:` line per failure; `2` on a usage error (wrong argument count, unknown deployable, or a version that is not SemVer without the `v`). On a dry run the step is advisory: the synthetic version has no changelog file, so check 1 fails without blocking the run.

### Acceptance checks (ubuntu-latest, PID 1 is systemd)

| Check | Command or condition |
|---|---|
| The amd64 deb installs | `sudo dpkg -i dist/kelvran-gateway_<version>_linux_amd64.deb` |
| The installed binary reports the tagged version | `kelvran-gateway -version` output starts with `kelvran-gateway <version> (` |
| The shipped example config validates | `kelvran-gateway -config /etc/kelvran-gateway/config.example.yaml -validate` |
| The unit file is well-formed | `/usr/lib/systemd/system/kelvran-gateway.service` exists; `systemd-analyze verify` passes; the file contains `TimeoutStopSec=60s`; `systemctl cat kelvran-gateway` succeeds |
| The package did not enable or start the service | `systemctl is-enabled` is not `enabled`; `systemctl is-active` is false |
| The unit runs under its hardening | The example config is copied to `/etc/kelvran-gateway/config.yaml`, `systemctl start`, `GET http://127.0.0.1:8080/healthz` answers within 15 s, `systemctl stop`, `systemctl show -p Result` is `success` |
| The tarball is complete | `kelvran-gateway_<version>_linux_amd64.tar.gz` extracts to `kelvran-gateway` (which runs `-version`), `LICENSE`, `NOTICE`, `config.example.yaml` |
| Checksums cover every asset | `sha256sum -c checksums.txt` in `dist/` |

Only the amd64 `.deb` is installed by the workflow. The `.rpm` and `.apk` come from the same `nfpms` block and are not installed or started by any CI job.

### Publish steps

| Step | Tool | Detail |
|---|---|---|
| SBOM per asset | `syft scan "file:<asset>" -o cyclonedx-json=<asset>.sbom.cdx.json` | For every `kelvran-gateway_*.tar.gz`, `.zip`, `.deb`, `.rpm`, `.apk` |
| Rewrite checksums | `sha256sum -- kelvran-gateway_* > checksums.txt`, then `sha256sum -c checksums.txt` | Replaces GoReleaser's file so the SBOMs are covered |
| Sign checksums | `cosign sign-blob --yes --bundle checksums.txt.sigstore.json checksums.txt` | Keyless, GitHub OIDC; the workflow immediately runs `cosign verify-blob` with the identity and issuer in "Verification identities" |
| Attest provenance | `actions/attest-build-provenance` with `subject-path` `dist/kelvran-gateway_*` and `dist/checksums.txt` | SLSA Build Level 2 |
| Keep a copy | Artifact `gateway-release-assets`, `retention-days: 14` | The only output of a dry run |
| Create or update the Release | `gh release create "$TAG" --verify-tag --title "gateway v<version>" --notes-file gateway/changelog/<version>.md [--prerelease] <assets>` | Assets: `dist/kelvran-gateway_*`, `dist/checksums.txt`, `dist/checksums.txt.sigstore.json`. If the Release exists with zero assets: `gh release upload` then `gh release edit --notes-file --title`. If it exists with assets: the run fails. Assets are never replaced, because the signed `checksums.txt` names the original bytes |

## Build identity

| Surface | Output |
|---|---|
| `kelvran-gateway -version` | One line: `kelvran-gateway <version> (<commit>, built <date>, <go version>, <os>/<arch>)`, then exit `0`. No config is read, no listener is started |
| Development build (`go build`, `go run`, `docker build` without build args) | `kelvran-gateway dev (none, built unknown, <go version>, <os>/<arch>)` |
| Startup log | One `build_info` record with fields `version`, `commit`, `date`, `go_version`, `platform` |
| Release archive or package | `<version>` is the bare version (`0.18.0`), `<commit>` the tag's commit, `<date>` GoReleaser's `.Date` |
| Image `:v<X.Y.Z>` | `<version>` is `X.Y.Z` |
| Image `:latest` or `:sha-<commit>` from a main push | `<version>` is `0.0.0-main.<first 12 hex of the commit>` |

`<go version>` is the Go runtime's own version string (`runtime.Version()`, of the form `go1.N.M`). The fields are stable; the exact spacing of the line is not part of the public surface (docs/VERSIONING.md section 2).

## Container image

Published by the `publish-image` job of `.github/workflows/ci.yml`, which runs on `push` events only (every push to `main` and every `gateway/v*` tag), after the `gateway` CI job passes. Permissions: `contents: read`, `packages: write`, `id-token: write`, `attestations: write`, `security-events: write`.

### Tags

| Tag | When | Build identity (`-version`) |
|---|---|---|
| `ghcr.io/kelvran/gateway:latest` | Every push to `main` and every `gateway/v*` tag; moves | `0.0.0-main.<sha12>` on a main push; `X.Y.Z` on a tag |
| `ghcr.io/kelvran/gateway:sha-<40-hex commit>` | Every push; immutable per commit | Same as above |
| `ghcr.io/kelvran/gateway:v<X.Y.Z>` | Only when the pushed ref is a tag matching `gateway/v*`; the tag step strips `gateway/` and keeps the `v` | `X.Y.Z` |

Build arguments passed to `gateway/Dockerfile`: `VERSION=<build identity>`, `COMMIT=<github.sha>`, `DATE=<UTC timestamp, %Y-%m-%dT%H:%M:%SZ>`.

### Platforms and index

| Property | Value |
|---|---|
| Platforms | `linux/amd64`, `linux/arm64` as one multi-platform index (on main since 2026-10-08, not in `gateway/v0.17.0`; `:v0.17.0` and earlier are `linux/amd64` only) |
| Index contents | Exactly the two platform manifests: buildx `provenance: false` and `sbom: false`, so no BuildKit `unknown/unknown` attestation manifests are added |
| Build method | The builder stage runs on the build platform and cross-compiles with `GOOS=$TARGETOS GOARCH=$TARGETARCH`; no QEMU |
| Digest to pin | The index digest, printed by `docker buildx imagetools inspect ghcr.io/kelvran/gateway:v<X.Y.Z>` |

### Image contents (`gateway/Dockerfile`)

| Property | Value |
|---|---|
| Base | `FROM scratch` |
| Files | `/gateway` (static binary, `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.version -X main.commit -X main.date"`) and `/etc/ssl/certs/ca-certificates.crt` |
| `EXPOSE` | `8080` |
| `USER` | `65532:65532` |
| `ENTRYPOINT` | `["/gateway"]` |
| `CMD` | `["-config", "/config.yaml"]` |
| Default build args | `VERSION=dev`, `COMMIT=none`, `DATE=unknown` |
| Builder base | `golang:1.27.1-alpine`, pinned by digest |

No shell, package manager or libc is present. The config is not baked in; mount it at `/config.yaml`.

### Labels and annotations

Set by `docker/metadata-action` with `DOCKER_METADATA_ANNOTATIONS_LEVELS: manifest,index`, so the same keys appear as image labels, as annotations on each platform manifest, and as annotations on the index (`docker buildx imagetools inspect --raw` shows them). `ci.yml` sets the keys below explicitly; the action adds its default `source`, `url`, `revision` and `created` keys as well.

| Key | Value |
|---|---|
| `org.opencontainers.image.title` | `kelvran-gateway` |
| `org.opencontainers.image.description` | `Self-hosted Go LLM gateway with a risk-gated response cache: an OpenAI-compatible API in front of Bedrock, Anthropic, OpenAI, Gemini and OpenAI-compatible providers.` |
| `org.opencontainers.image.documentation` | `https://github.com/kelvran/gateway/blob/main/docs/operations/DEPLOY.md` |
| `org.opencontainers.image.licenses` | `Apache-2.0` |
| `org.opencontainers.image.version` | The build identity (`X.Y.Z` or `0.0.0-main.<sha12>`) |
| `org.opencontainers.image.source` | `https://github.com/kelvran/gateway` (action default) |
| `org.opencontainers.image.url` | `https://github.com/kelvran/gateway` (action default) |
| `org.opencontainers.image.revision` | The 40-hex commit (action default) |
| `org.opencontainers.image.created` | Build timestamp, RFC 3339 (action default) |
| `io.artifacthub.package.readme-url` | `https://raw.githubusercontent.com/kelvran/gateway/main/docs/reference/container-image.md` |
| `io.artifacthub.package.license` | `Apache-2.0` |
| `io.artifacthub.package.category` | `ai-machine-learning` |
| `io.artifacthub.package.keywords` | `llm,gateway,openai,bedrock,anthropic,gemini,cache,ai-infrastructure` |

### Scanning, signing and attestations

| Step | Detail |
|---|---|
| Trivy scan, `linux/amd64` | `aquasecurity/trivy-action` on `ghcr.io/kelvran/gateway@<index digest>`, severity `CRITICAL,HIGH`, SARIF uploaded to code scanning under category `trivy-gateway-image` |
| Trivy scan, `linux/arm64` | Same, with `TRIVY_PLATFORM: linux/arm64`, category `trivy-gateway-image-arm64` |
| Scan gating | Report-only; no exit-code threshold. Findings appear on the repository's code-scanning dashboard |
| Signature | `cosign sign --yes --recursive ghcr.io/kelvran/gateway@<index digest>`: keyless via GitHub OIDC; signs the index and each per-platform manifest |
| SBOM | `anchore/sbom-action` produces one CycloneDX JSON for the index digest (syft describes the runner's platform, `linux/amd64`); `actions/attest-sbom` pushes the attestation to the registry against the index digest |
| Provenance | `actions/attest-build-provenance` (SLSA Build Level 2), `push-to-registry: true`, against the index digest |

## Evals release: asset inventory and pipeline

`.github/workflows/release-evals.yml` is the sibling of `release.yml` for the Python deployable. Triggers, `workflow_dispatch` inputs, dry-run semantics, the never-replace rule and the Release title/body pattern are the same as the gateway's, with `evals/` in place of `gateway/` (dry-run tag `evals/v0.0.0-dryrun.<sha12>`, version `0.0.0-dryrun.<sha12>`, preflight `scripts/release-preflight.sh evals "$VERSION"`).

| Asset | Detail |
|---|---|
| `kelvran_evals-<version>.tar.gz` | sdist from `uv build --out-dir dist` in `evals/`, Python `3.12` |
| `kelvran_evals-<version>-py3-none-any.whl` | wheel from the same build |
| `checksums.txt` | `sha256sum -- kelvran_evals-*` |
| `checksums.txt.sigstore.json` | `cosign sign-blob --yes --bundle`, keyless, GitHub OIDC |
| Provenance attestation | `actions/attest-build-provenance` over `evals/dist/kelvran_evals-*` and `evals/dist/checksums.txt` |
| Workflow artifact | `evals-release-assets`, `retention-days: 14` |

Differences from the gateway pipeline: there is a single `build-sign-release` job after `preflight`; on a real release the build fails unless a file matching `dist/kelvran_evals-<version>*` exists (the artefact version must be the tag's version, not a stale `pyproject.toml` one); there is no acceptance install and no SBOM step. An evals pre-release cannot be released by this workflow today: the tag regex only admits a pre-release suffix introduced by `-` (`evals/v0.11.0-rc.1`), preflight check 4 then requires `pyproject.toml` to declare exactly `0.11.0-rc.1`, but hatchling writes the PEP 440-normalised `0.11.0rc1` into the sdist and wheel names, so the build job's `dist/kelvran_evals-0.11.0-rc.1*` check never matches and the run fails before signing. Until that check compares normalised versions, an evals pre-release can only be exercised as a `workflow_dispatch` dry run (the check is skipped), which leaves a workflow artifact and no Release.

Not available today: PyPI publication of `kelvran-evals` is blocked on trademark clearance (RELEASE.md, Pre-flight Blockers). There is no evals container image; evals is a CLI.

## Verification identities

| Artifact | Tool | Certificate identity (regexp) | OIDC issuer |
|---|---|---|---|
| Gateway `checksums.txt` | `cosign verify-blob --bundle checksums.txt.sigstore.json` | `^https://github.com/kelvran/gateway/\.github/workflows/release\.yml@` | `https://token.actions.githubusercontent.com` |
| Evals `checksums.txt` | `cosign verify-blob --bundle checksums.txt.sigstore.json` | `^https://github.com/kelvran/gateway/\.github/workflows/release-evals\.yml@` | `https://token.actions.githubusercontent.com` |
| Image signature | `cosign verify` | `^https://github.com/kelvran/gateway/` | `https://token.actions.githubusercontent.com` |
| Image SBOM attestation | `cosign verify-attestation --type cyclonedx` | `^https://github.com/kelvran/gateway/` | `https://token.actions.githubusercontent.com` |
| Image provenance attestation | `cosign verify-attestation --type slsaprovenance1` | `^https://github.com/kelvran/gateway/` | `https://token.actions.githubusercontent.com` |
| Any asset or image (GitHub attestations) | `gh attestation verify <file> -R kelvran/gateway` or `gh attestation verify oci://ghcr.io/kelvran/gateway:<tag> -R kelvran/gateway` | | |

The gateway and evals release workflows run exactly the `cosign verify-blob` command from this table on their own output before attesting.

## Commands

### Verify a downloaded gateway release asset

```bash
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/kelvran/gateway/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum -c checksums.txt --ignore-missing
gh attestation verify kelvran-gateway_<version>_linux_amd64.tar.gz -R kelvran/gateway
```

For an evals asset, use the `release-evals\.yml@` identity and the `kelvran_evals-<version>…` file name.

### Install the deb and start the service

```bash
sudo dpkg -i kelvran-gateway_<version>_linux_amd64.deb          # or rpm -i / apk add --allow-untrusted
sudo cp /etc/kelvran-gateway/config.example.yaml /etc/kelvran-gateway/config.yaml
sudoedit /etc/kelvran-gateway/config.yaml
sudo install -m 0600 /dev/null /etc/kelvran-gateway/env
sudoedit /etc/kelvran-gateway/env                               # provider credentials, one KEY=value per line
kelvran-gateway -config /etc/kelvran-gateway/config.yaml -validate
sudo systemctl enable --now kelvran-gateway
kelvran-gateway -version
```

The package does not run `systemctl enable` or `start`; the operator does. The unit's `ExecStartPre` repeats `-validate` on every start. The step-by-step guide is [docs/how-to/deploy/systemd-package.md](../how-to/deploy/systemd-package.md); the operator context is [docs/operations/DEPLOY.md](../operations/DEPLOY.md).

### Verify the container image and pin its digest

```bash
cosign verify ghcr.io/kelvran/gateway:v<X.Y.Z> \
  --certificate-identity-regexp '^https://github.com/kelvran/gateway/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/kelvran/gateway:v<X.Y.Z> -R kelvran/gateway
docker buildx imagetools inspect ghcr.io/kelvran/gateway:v<X.Y.Z>   # prints the index digest to pin
```

`cosign tree ghcr.io/kelvran/gateway@<digest>` lists the signature and both attestations attached to a digest.

### Run the image

```bash
docker run --rm -p 8080:8080 -v "$PWD/config.yaml:/config.yaml:ro" -e OPENAI_API_KEY ghcr.io/kelvran/gateway:v<X.Y.Z>
docker run --rm ghcr.io/kelvran/gateway:v<X.Y.Z> -version
```

`OPENAI_API_KEY` is an example of a credential environment variable named by `api_key_env` in the config; the image carries no credentials.

### Run the preflight locally

```bash
scripts/release-preflight.sh gateway <version>   # from the repository root, version without the v
scripts/release-preflight.sh evals <version>
```

## Release history and support window

### Gateway releases

| Version | Date |
|---|---|
| `gateway/v0.1.0` | 2026-09-03 |
| `gateway/v0.2.0` | 2026-09-09 |
| `gateway/v0.3.0` | 2026-09-09 |
| `gateway/v0.4.0` | 2026-09-10 |
| `gateway/v0.6.0` | 2026-09-10 |
| `gateway/v0.7.0` | 2026-09-11 |
| `gateway/v0.8.0` | 2026-09-11 |
| `gateway/v0.9.0` | 2026-09-11 |
| `gateway/v0.10.0` | 2026-09-13 |
| `gateway/v0.10.1` | 2026-09-13 |
| `gateway/v0.5.0` | 2026-09-15 |
| `gateway/v0.11.0` | 2026-09-16 |
| `gateway/v0.12.0` | 2026-09-17 |
| `gateway/v0.13.0` | 2026-09-19 |
| `gateway/v0.14.0` | 2026-09-21 |
| `gateway/v0.14.1` | 2026-09-22 |
| `gateway/v0.14.2` | 2026-09-22 |
| `gateway/v0.15.0` | 2026-09-23 |
| `gateway/v0.16.0` | 2026-09-28 |
| `gateway/v0.17.0` | 2026-10-07 |

Dates are the first line of each `gateway/changelog/<version>.md`. None of these Releases carries the assets described on this page.

### Support window (docs/VERSIONING.md section 6)

| Rule | Scope |
|---|---|
| Latest minor | All fixes (bugs and security) as PATCH releases |
| Previous minor | Security fixes only, for 90 days after the newer minor shipped |
| Older | Unsupported |

| Deployable | Latest minor | Previous minor | Security fixes for the previous minor until |
|---|---|---|---|
| `gateway` | `gateway/v0.17.0` (2026-10-07) | `gateway/v0.16.0` (2026-09-28) | 2027-01-05 |
| `evals` | `evals/v0.10.1` (2026-09-22) | `evals/v0.10.0` (2026-09-21) | 2026-12-21 |

A pre-release (`-rc.N` or similar) has no support window and may change or disappear without an [UPGRADE.md](../../UPGRADE.md) row. Upgrade steps are in [docs/how-to/upgrade.md](../how-to/upgrade.md); the policy behind the numbers is in the [versioning explanation](../explanation/versioning.md) and the compatibility matrix in [compatibility.md](./compatibility.md).

## Not available today

- Homebrew tap or cask, Scoop, winget, Nix, hosted apt or rpm repositories. `gateway/.goreleaser.yaml` has no `brews` or `casks` stanza and no `kelvran/homebrew-tap` repository exists.
- A Helm chart, by recorded decision ([README.md](../../README.md), "Status and known limitations"). Kubernetes deployment uses the Kustomize base; see [docs/how-to/deploy/kubernetes-kustomize.md](../how-to/deploy/kubernetes-kustomize.md).
- `kelvran-bench` as a release asset. Only `./cmd/gateway` is built.
- PyPI publication of `kelvran-evals`. Blocked on trademark clearance.
- An Artifact Hub listing for the image. The `io.artifacthub.package.*` labels are in place; registering the repository on artifacthub.io is a one-time owner action that has not been taken.
- Any gateway or evals Release that carries signed assets, as of 2026-10-08. The newest tags (`gateway/v0.17.0`, `evals/v0.10.1`) predate both pipelines and cannot be rebuilt by them.
- Per-platform SBOM attestations for the image. One SBOM, generated against the index digest, is attested; the `ci.yml` comment records per-platform SBOMs as a follow-up.
- Embedded GPG signatures on the deb, rpm or apk packages.
- CI installation of the `.rpm` or `.apk`. Only the amd64 `.deb` is installed and started by the acceptance job.
- An SBOM for evals release assets.
- An evals container image.
- An evals pre-release Release: `release-evals.yml`'s artefact-version check cannot match a hyphenated pre-release version (see the evals section).

## Related pages

- [RELEASE.md](../../RELEASE.md): the release runbook, publish targets and rollback procedure.
- [docs/VERSIONING.md](../VERSIONING.md): version policy, public surface, support window, pre-releases.
- [UPGRADE.md](../../UPGRADE.md): breaking changes and migration steps per release.
- [container-image.md](./container-image.md): the image's reference card.
- [docs/operations/DEPLOY.md](../operations/DEPLOY.md): operator deployment guide.
- [docs/how-to/deploy/systemd-package.md](../how-to/deploy/systemd-package.md), [docs/how-to/deploy/docker-compose.md](../how-to/deploy/docker-compose.md), [docs/how-to/deploy/kubernetes-kustomize.md](../how-to/deploy/kubernetes-kustomize.md), [docs/how-to/deploy/ecs-fargate.md](../how-to/deploy/ecs-fargate.md): deployment how-tos.
- [docs/how-to/upgrade.md](../how-to/upgrade.md): upgrading between releases.
- [Versioning (explanation)](../explanation/versioning.md): why the two deployables are versioned as they are.
- [DECISIONS.md](../../DECISIONS.md): the 2026-10-08 entry on why GoReleaser runs in snapshot mode.
- [SECURITY.md](../../SECURITY.md): vulnerability reporting; points to the support window above.
