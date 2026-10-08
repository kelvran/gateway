# Release Runbook

## Per-Deployable Release Steps

**`gateway`:**
1. Move `gateway/changelog/unreleased.md`'s content into a new `gateway/changelog/<version>.md` (e.g. `0.1.0.md`), dated, self-contained.
2. Reset `gateway/changelog/unreleased.md` to empty category headers.
   Update the support-window table in `docs/VERSIONING.md` § 6 (latest minor, previous minor, its 90-day security date). Then run `scripts/release-preflight.sh gateway <version>` from the repository root (added 2026-10-08): it checks that the dated file exists, that `unreleased.md` holds no entries, and that every `**BREAKING**` entry has an `UPGRADE.md` row. `.github/workflows/release.yml` runs the same script at the tag and refuses to build if it fails, so running it here saves a broken tag.
3. Tag the release `gateway/v<version>` (SemVer — load-bearing for the Go module path: `gateway/go.mod`'s module directive is `github.com/kelvran/gateway/gateway`, not the bare `github.com/kelvran/gateway`, because `go.mod` lives one level below the repo root — the repo itself is named `kelvran/gateway` on GitHub, and Go's own subdirectory-module rule, go.dev/ref/mod's "Mapping versions to commits", requires the declared module path to carry the physical subdirectory as a literal suffix, with the tag prefixed to match. Verified empirically before this convention was adopted: tagging with the bare module path resolved to a synthesized empty stub `go.mod` via the proxy, not the real dependency graph — see `DECISIONS.md`.).
4. **Do not build or upload release assets by hand, and do not create the GitHub Release by hand.** Pushing the `gateway/v<version>` tag triggers `.github/workflows/release.yml` (added 2026-10-08): it re-runs the preflight at the tag, builds the five binaries (linux/darwin × amd64/arm64, windows/amd64), the archives (with `LICENSE`, `NOTICE`, `config.example.yaml`), and the deb/rpm/apk packages (with the systemd unit, `deploy/systemd/kelvran-gateway.service`) via GoReleaser in snapshot mode (`gateway/.goreleaser.yaml`; see `DECISIONS.md` 2026-10-08 for why snapshot mode), writes a CycloneDX SBOM per archive and package with syft, installs the deb on the runner and verifies the unit and the `-version`/`-validate` output as an acceptance test, signs `checksums.txt` with cosign (keyless, GitHub OIDC → `checksums.txt.sigstore.json`), attests SLSA build provenance over every asset, and creates the Release with `gh release create --verify-tag`, using `gateway/changelog/<version>.md` as the body. A version with a `-` suffix is marked a pre-release. The Go module proxy picks the tag up on its own. To run the pipeline for an existing tag whose Release has no assets yet, dispatch the workflow with `tag` set and `publish: true` (a Release that already has assets makes the run fail: assets are never replaced); dispatching it with no `tag` and `publish: false` (the defaults) is a dry run of the dispatching ref under the synthetic version `0.0.0-dryrun.<sha>` — every stage runs except the Release creation, and the signed assets stay a workflow artifact. Tags that predate the pipeline (`gateway/v0.17.0` and earlier) have none of its files in their tree and cannot be rebuilt this way.
5. Pushing that same `gateway/v<version>` tag also triggers `.github/workflows/ci.yml`'s `publish-image` job automatically — real, shipped, no manual step needed: it re-runs the full build/test/lint suite against that exact tagged commit, then pushes `ghcr.io/kelvran/gateway:v<version>` (only the `gateway/` prefix is stripped, the `v` stays — `gateway/v0.17.0` became `:v0.17.0`; see the tag step of `ci.yml`'s `publish-image` job) alongside the always-present `:latest`/`:sha-<commit>` tags, signed and attested exactly like every other push.

**`evals`:**
1. Bump `version` in `evals/pyproject.toml` to `<version>` and refresh `evals/uv.lock` (`cd evals && uv lock`); `python3 -I scripts/check_versions.py` must pass — CI runs it on every push, and it exists because the version sat at `0.8.0` through three releases (`v0.9.0`–`v0.10.1`) when this step was missing.
2. Move `evals/changelog/unreleased.md`'s content into a new `evals/changelog/<version>.md`, dated, self-contained.
3. Reset `evals/changelog/unreleased.md` to empty category headers, and update the support-window table in `docs/VERSIONING.md` § 6.
4. Tag the release `evals/v<version>` (SemVer by default; revisit CalVer per the note in `evals/changelog/unreleased.md` once shipping continuously). Run `scripts/release-preflight.sh evals <version>` first: it also checks the `pyproject.toml` version. Pushing the tag triggers `.github/workflows/release-evals.yml` (added 2026-10-08), which builds the sdist and wheel with `uv build`, signs `checksums.txt` with cosign, attests provenance and creates the GitHub Release with those assets — so every Release of either deployable created from this pipeline onward carries signed assets (the OpenSSF Scorecard Signed-Releases check reads the last five Releases of any kind). Do not create the Release by hand.
5. Publish to PyPI as `kelvran-evals` (PyPI has no scoping, hence the prefixed name — see `ai-infra-research/naming-and-docs-plan.md`'s naming section, "Immediate next actions").

## Contract-Version Bump-and-Validate Procedure

Any release that includes a change to `api/` (the shared OTel/proto contract) must, before either deployable is tagged (and every release follows `docs/VERSIONING.md` § 3 Rules while 0.x for the two deployables' own public surfaces):
1. Run `buf breaking` against the previous published contract version — a breaking change is not disqualifying, but it must be intentional and documented.
2. If breaking: add an entry to `UPGRADE.md` describing the migration, and bump the contract's own version identifier independent of either deployable's version.
3. Confirm both `gateway` and `evals` have been updated to generate bindings from the new contract version before either ships — never let one deployable release against a contract version the other hasn't caught up to.

## Publish Targets

| Target | Deployable | Package name |
|---|---|---|
| GitHub | both | `github.com/kelvran/gateway` (and/or a monorepo-wide org page) |
| GitHub Releases (gateway) — pipeline **on `main` since 2026-10-08** via `release.yml` (one successful dry run; not in `gateway/v0.17.0`, whose Release, like every earlier one, has zero assets — the first `gateway/v*` tag pushed after that date is the first Release that will carry them) | `gateway` | `kelvran-gateway_<version>_<os>_<arch>.tar.gz` / `.zip`, `.deb`/`.rpm`/`.apk` for linux amd64+arm64, one `.sbom.cdx.json` per archive and package, `checksums.txt`, `checksums.txt.sigstore.json` |
| GitHub Releases (evals) — pipeline **on `main` since 2026-10-08** via `release-evals.yml` (no run yet as of 2026-10-08, not even a dry run; `evals/v0.10.1` and every earlier Release has zero assets) | `evals` | `kelvran_evals-<version>.tar.gz`, `kelvran_evals-<version>-py3-none-any.whl`, `checksums.txt`, `checksums.txt.sigstore.json` |
| GHCR (container image) | `gateway` | `ghcr.io/kelvran/gateway` — multi-platform (`linux/amd64`, `linux/arm64`) since 2026-10-08, with OCI + Artifact Hub labels |
| Artifact Hub (container-image listing) | `gateway` | labels in place (`io.artifacthub.package.*`, reference card `docs/reference/container-image.md`); registering the repository on artifacthub.io is a one-time owner action, **not done** |
| npm | `gateway` (client SDK, if/when one ships) | `@kelvran/gateway` |
| PyPI | `evals` | `kelvran-evals` |
| crates.io | reserved, not actively published yet | `kelvran` |
| Go module proxy | `gateway` | `github.com/kelvran/gateway/gateway` |

*(Homebrew — which would be a cask in a tap, not a formula: GoReleaser deprecated its `brews` (Homebrew formula) stanza in favour of `homebrew_casks` in v2.10, per `docs/upgrade-research/kelvran-deep-research-round4-discoverability-2026-10-08.md` § 1 — and any other distribution channel: add here once actually adopted — not a v1 commitment. Nothing is adopted yet: `gateway/.goreleaser.yaml` has no brews/casks stanza and no `kelvran/homebrew-tap` repository exists.)*

**GHCR is real, live, and shipped (2026-09-13)** — unlike every other row above, this one is no longer intent. **Corrected 2026-10-08**: no longer the only one — the Go module proxy row needs no publish step of its own and already serves every `gateway/v*` tag (`proxy.golang.org/github.com/kelvran/gateway/gateway/@latest` resolves to `v0.17.0`), and the two GitHub Releases pipelines are on `main` since 2026-10-08, though no Release has carried their assets yet — see their rows. `.github/workflows/ci.yml`'s `publish-image` job builds `gateway/Dockerfile` and pushes `ghcr.io/kelvran/gateway:latest` + `:sha-<commit>` on every push to `main`, with a real cosign keyless signature, a CycloneDX SBOM, and SLSA Build Level 2 provenance attached to the exact build digest — all independently re-verified against the real published image (`docker pull` + `cosign verify` / `cosign verify-attestation`), not just trusted because CI didn't error. `evals/` still has no Dockerfile (it's a CLI, not a service) — nothing to publish there.

### Verifying the published gateway image

First confirm which build the tag resolves to — the binary reports the version, commit and build date the publish job injected (`-ldflags -X main.version=…`; a value of `dev` means the image was built outside CI):

```bash
docker run --rm ghcr.io/kelvran/gateway:latest -version
```

Any consumer of `ghcr.io/kelvran/gateway` can independently verify its signature and attestations — no special access needed, the package is public:

```bash
# Pull and note the real digest (never trust a mutable tag alone for verification)
docker pull ghcr.io/kelvran/gateway:latest
DIGEST=$(docker inspect ghcr.io/kelvran/gateway:latest --format '{{index .RepoDigests 0}}')
# Since 2026-10-08 the tag resolves to a multi-platform index (linux/amd64 + linux/arm64); the publish job signs
# the index and each platform manifest (cosign sign --recursive), so either digest verifies. The SBOM and
# provenance attestations are attached to the index digest, which is what `docker pull` records here.

# Verify the base-image signature (keyless, via GitHub Actions OIDC)
cosign verify "$DIGEST" \
  --certificate-identity-regexp "^https://github.com/kelvran/gateway/" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"

# Verify the SBOM attestation (CycloneDX)
cosign verify-attestation "$DIGEST" \
  --type cyclonedx \
  --certificate-identity-regexp "^https://github.com/kelvran/gateway/" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"

# Verify the SLSA build-provenance attestation
cosign verify-attestation "$DIGEST" \
  --type slsaprovenance1 \
  --certificate-identity-regexp "^https://github.com/kelvran/gateway/" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
```

Each command exits `0` only if the signature/attestation is real, was produced by the `kelvran/gateway` repo's own GitHub Actions workflow (not any other identity), and has a corresponding entry in the public Sigstore Rekor transparency log. `cosign tree "$DIGEST"` shows the full artifact graph (signature + both attestations) if you want to see what exists before verifying each one.

### Verifying a release binary (added 2026-10-08)

Every asset on a `gateway/v*` or `evals/v*` Release created by the workflows above is covered by the signed `checksums.txt` and by a SLSA build-provenance attestation:

```bash
# 1. The checksums file was signed by this repository's release workflow (keyless; identity = the workflow, issuer = GitHub Actions)
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/kelvran/gateway/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
# 2. The asset you downloaded is the one the checksums file names
sha256sum -c checksums.txt --ignore-missing
# 3. Build provenance for one asset (who built it, from which commit, with which workflow)
gh attestation verify kelvran-gateway_<version>_linux_amd64.tar.gz -R kelvran/gateway
```

## Pre-flight Blockers

- **USPTO TESS/WHOIS trademark clearance** (`DECISIONS.md`'s naming-clearance entry) is still open. Scope decision, made explicitly rather than left ambiguous: this blocks a first **PyPI** publish of `kelvran-evals` (a public name/filename claim that isn't cleanly reversible: PyPI won't pre-screen a first upload, but a later trademark complaint gives PSF an explicit discretionary removal/reassignment path, and the exact version file itself can never be re-uploaded once deleted) but does **not** block pushing `gateway/v<version>`/`evals/v<version>` git tags or creating GitHub Releases on the already-public `github.com/kelvran/gateway` repo — that "GitHub org/repo" prong of the blocker was already crossed when the repo went public, and a git tag/Release on an already-public repo is low-stakes and fully reversible (delete the tag/release) in a way a PyPI publish is not. A caveat worth naming honestly: once a `gateway/v<version>` tag is pushed to the public repo, nothing prevents `proxy.golang.org`/`sum.golang.org` from durably caching it if anyone runs `go get` against it — a byproduct of the already-made repo-publication decision, not something newly incurred by tagging itself. A second caveat, real since the `publish-image` job started reacting to this same tag (2026-09-13): a `gateway/v<version>` push now also publishes a real, signed, publicly-pullable `ghcr.io/kelvran/gateway:v<version>` image tag — deletable from GHCR directly (unlike the Go proxy's own durable cache), but any consumer who has already pulled it can retain a local copy regardless.

## Rollback Procedure

Neither deployable keeps state inside the binary itself, so rollback is: redeploy the previous tagged version. The gateway's only durable state is what `config.yaml` opts into — virtual keys, budgets and prompt templates are in-memory by default and can persist either to a local bbolt file (`admin.persist_path`/`budget.persist_path`/`prompt.persist_path`), which lives beside the process and must stay in place for the replacement binary, or to Redis (`admin.redis_addr`/`budget.redis_addr`), which outlives it; admin-API deployment-weight changes are never persisted (`docs/reference/admin-api.md`, `docs/operations/DEPLOY.md`'s Redis-outage runbook; the persistence keys themselves are in `gateway/internal/gateway/controlplane/config.go`). Postgres and ClickHouse are `gateway/ARCHITECTURE.md` Tech Stack future targets used by no shipped code. `evals` is a CLI, not a service, and holds nothing between invocations beyond the files it reads and writes. For a binary or package install, download the previous Release's asset and install it; never re-upload or replace an asset under an existing name, because the signed `checksums.txt` of that Release names the original bytes. No database migration rollback is expected for a typical release — if a release does include a schema migration, that migration's own down-path must be verified *before* the release ships, not discovered during an incident.

*(Corrected 2026-09-13: this line previously said the whole runbook "describes intent, not a tested procedure" — no longer true for the GHCR container-image publish path specifically, which is real, shipped, and independently verified end to end (see the Verifying section above). The PyPI/npm/crates.io/Go-module-proxy rows above remain intent-only pending their own real triggers — this note now applies to those, not to the runbook as a whole.)*

*(Corrected 2026-10-08: the Go module proxy row no longer belongs in that intent-only list — it has no publish step to trigger, and `proxy.golang.org` already serves every `gateway/v*` tag with `@latest` resolving to `v0.17.0` (see the GHCR paragraph above). PyPI, npm and crates.io remain intent-only.)*
