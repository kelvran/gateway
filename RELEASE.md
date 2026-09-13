# Release Runbook

## Per-Deployable Release Steps

**`gateway`:**
1. Move `gateway/changelog/unreleased.md`'s content into a new `gateway/changelog/<version>.md` (e.g. `0.1.0.md`), dated, self-contained.
2. Reset `gateway/changelog/unreleased.md` to empty category headers.
3. Tag the release `gateway/v<version>` (SemVer — load-bearing for the Go module path: `gateway/go.mod`'s module directive is `github.com/kelvran/gateway/gateway`, not the bare `github.com/kelvran/gateway`, because `go.mod` lives one level below the repo root — the repo itself is named `kelvran/gateway` on GitHub, and Go's own subdirectory-module rule, go.dev/ref/mod's "Mapping versions to commits", requires the declared module path to carry the physical subdirectory as a literal suffix, with the tag prefixed to match. Verified empirically before this convention was adopted: tagging with the bare module path resolved to a synthesized empty stub `go.mod` via the proxy, not the real dependency graph — see `DECISIONS.md`.).
4. Build and publish the static binary; update the Go module proxy cache picks it up automatically once tagged.
5. Pushing that same `gateway/v<version>` tag also triggers `.github/workflows/ci.yml`'s `publish-image` job automatically — real, shipped, no manual step needed: it re-runs the full build/test/lint suite against that exact tagged commit, then pushes `ghcr.io/kelvran/gateway:<version>` (the `gateway/` prefix stripped) alongside the always-present `:latest`/`:sha-<commit>` tags, signed and attested exactly like every other push.

**`evals`:**
1. Move `evals/changelog/unreleased.md`'s content into a new `evals/changelog/<version>.md`, dated, self-contained.
2. Reset `evals/changelog/unreleased.md` to empty category headers.
3. Tag the release `evals/v<version>` (SemVer by default; revisit CalVer per the note in `evals/changelog/unreleased.md` once shipping continuously).
4. Publish to PyPI as `kelvran-evals` (PyPI has no scoping, hence the prefixed name — see `ai-infra-research/naming-and-docs-plan.md`'s naming section, "Immediate next actions").

## Contract-Version Bump-and-Validate Procedure

Any release that includes a change to `api/` (the shared OTel/proto contract) must, before either deployable is tagged:
1. Run `buf breaking` against the previous published contract version — a breaking change is not disqualifying, but it must be intentional and documented.
2. If breaking: add an entry to `UPGRADE.md` describing the migration, and bump the contract's own version identifier independent of either deployable's version.
3. Confirm both `gateway` and `evals` have been updated to generate bindings from the new contract version before either ships — never let one deployable release against a contract version the other hasn't caught up to.

## Publish Targets

| Target | Deployable | Package name |
|---|---|---|
| GitHub | both | `github.com/kelvran/gateway` (and/or a monorepo-wide org page) |
| GHCR (container image) | `gateway` | `ghcr.io/kelvran/gateway` |
| npm | `gateway` (client SDK, if/when one ships) | `@kelvran/gateway` |
| PyPI | `evals` | `kelvran-evals` |
| crates.io | reserved, not actively published yet | `kelvran` |
| Go module proxy | `gateway` | `github.com/kelvran/gateway/gateway` |

*(Homebrew formula and any other distribution channel: add here once actually adopted — not a v1 commitment.)*

**GHCR is real, live, and shipped (2026-09-13)** — unlike every other row above, this one is no longer intent. `.github/workflows/ci.yml`'s `publish-image` job builds `gateway/Dockerfile` and pushes `ghcr.io/kelvran/gateway:latest` + `:sha-<commit>` on every push to `main`, with a real cosign keyless signature, a CycloneDX SBOM, and SLSA Build Level 2 provenance attached to the exact build digest — all independently re-verified against the real published image (`docker pull` + `cosign verify` / `cosign verify-attestation`), not just trusted because CI didn't error. `evals/` still has no Dockerfile (it's a CLI, not a service) — nothing to publish there.

### Verifying the published gateway image

Any consumer of `ghcr.io/kelvran/gateway` can independently verify its signature and attestations — no special access needed, the package is public:

```bash
# Pull and note the real digest (never trust a mutable tag alone for verification)
docker pull ghcr.io/kelvran/gateway:latest
DIGEST=$(docker inspect ghcr.io/kelvran/gateway:latest --format '{{index .RepoDigests 0}}')

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

## Pre-flight Blockers

- **USPTO TESS/WHOIS trademark clearance** (`DECISIONS.md`'s naming-clearance entry) is still open. Scope decision, made explicitly rather than left ambiguous: this blocks a first **PyPI** publish of `kelvran-evals` (a genuinely permanent, admin-unappealable public name/filename claim) but does **not** block pushing `gateway/v<version>`/`evals/v<version>` git tags or creating GitHub Releases on the already-public `github.com/kelvran/gateway` repo — that "GitHub org/repo" prong of the blocker was already crossed when the repo went public, and a git tag/Release on an already-public repo is low-stakes and fully reversible (delete the tag/release) in a way a PyPI publish is not. A caveat worth naming honestly: once a `gateway/v<version>` tag is pushed to the public repo, nothing prevents `proxy.golang.org`/`sum.golang.org` from durably caching it if anyone runs `go get` against it — a byproduct of the already-made repo-publication decision, not something newly incurred by tagging itself. A second caveat, real since the `publish-image` job started reacting to this same tag (2026-09-13): a `gateway/v<version>` push now also publishes a real, signed, publicly-pullable `ghcr.io/kelvran/gateway:<version>` image tag — deletable from GHCR directly (unlike the Go proxy's own durable cache), but any consumer who has already pulled it can retain a local copy regardless.

## Rollback Procedure

Both deployables are stateless at the request-handling layer (state lives in Redis/Postgres/ClickHouse, not in the binary/process itself), so rollback is: redeploy the previous tagged version. No database migration rollback is expected for a typical release — if a release does include a schema migration, that migration's own down-path must be verified *before* the release ships, not discovered during an incident.

*(Corrected 2026-09-13: this line previously said the whole runbook "describes intent, not a tested procedure" — no longer true for the GHCR container-image publish path specifically, which is real, shipped, and independently verified end to end (see the Verifying section above). The PyPI/npm/crates.io/Go-module-proxy rows above remain intent-only pending their own real triggers — this note now applies to those, not to the runbook as a whole.)*
