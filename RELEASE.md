# Release Runbook

## Per-Deployable Release Steps

**`gateway`:**
1. Move `gateway/changelog/unreleased.md`'s content into a new `gateway/changelog/<version>.md` (e.g. `0.1.0.md`), dated, self-contained.
2. Reset `gateway/changelog/unreleased.md` to empty category headers.
3. Tag the release `gateway/v<version>` (SemVer — load-bearing for the Go module path: `gateway/go.mod`'s module directive is `github.com/kelvran/gateway/gateway`, not the bare `github.com/kelvran/gateway`, because `go.mod` lives one level below the repo root — the repo itself is named `kelvran/gateway` on GitHub, and Go's own subdirectory-module rule, go.dev/ref/mod's "Mapping versions to commits", requires the declared module path to carry the physical subdirectory as a literal suffix, with the tag prefixed to match. Verified empirically before this convention was adopted: tagging with the bare module path resolved to a synthesized empty stub `go.mod` via the proxy, not the real dependency graph — see `DECISIONS.md`.).
4. Build and publish the static binary; update the Go module proxy cache picks it up automatically once tagged.

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
| npm | `gateway` (client SDK, if/when one ships) | `@kelvran/gateway` |
| PyPI | `evals` | `kelvran-evals` |
| crates.io | reserved, not actively published yet | `kelvran` |
| Go module proxy | `gateway` | `github.com/kelvran/gateway/gateway` |

*(Homebrew formula and any other distribution channel: add here once actually adopted — not a v1 commitment.)*

## Pre-flight Blockers

- **USPTO TESS/WHOIS trademark clearance** (`DECISIONS.md`'s naming-clearance entry) is still open. Scope decision, made explicitly rather than left ambiguous: this blocks a first **PyPI** publish of `kelvran-evals` (a genuinely permanent, admin-unappealable public name/filename claim) but does **not** block pushing `gateway/v<version>`/`evals/v<version>` git tags or creating GitHub Releases on the already-public `github.com/kelvran/gateway` repo — that "GitHub org/repo" prong of the blocker was already crossed when the repo went public, and a git tag/Release on an already-public repo is low-stakes and fully reversible (delete the tag/release) in a way a PyPI publish is not. A caveat worth naming honestly: once a `gateway/v<version>` tag is pushed to the public repo, nothing prevents `proxy.golang.org`/`sum.golang.org` from durably caching it if anyone runs `go get` against it — a byproduct of the already-made repo-publication decision, not something newly incurred by tagging itself.

## Rollback Procedure

Both deployables are stateless at the request-handling layer (state lives in Redis/Postgres/ClickHouse, not in the binary/process itself), so rollback is: redeploy the previous tagged version. No database migration rollback is expected for a typical release — if a release does include a schema migration, that migration's own down-path must be verified *before* the release ships, not discovered during an incident.

*(This runbook will be revised once the project has a real deployment target and CI/CD pipeline — currently describes intent, not a tested procedure.)*
