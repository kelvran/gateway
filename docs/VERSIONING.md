# Versioning, compatibility and support policy

**Status**: policy in force from 2026-10-08 for every release after `gateway/v0.17.0` and `evals/v0.10.1`. This file says what a version number promises, which surfaces the promise covers, how long a release is supported, and how a surface is retired. The mechanics of cutting a release live in [`RELEASE.md`](../RELEASE.md); the records this policy relies on are [`UPGRADE.md`](../UPGRADE.md) (breaking changes with migration steps) and [`DEPRECATED.md`](../DEPRECATED.md) (surfaces scheduled for removal).

## 1. Scope: two deployables, two version lines

Kelvran ships two independently versioned deployables from one repository:

| Deployable | Tag | Published as | Current |
|---|---|---|---|
| `gateway` (Go) | `gateway/vX.Y.Z` | `ghcr.io/kelvran/gateway:vX.Y.Z` image, Go module `github.com/kelvran/gateway/gateway`, and — from the first release after `gateway/v0.17.0` — GitHub Release assets (archives, deb/rpm/apk): the release pipeline (`.github/workflows/release.yml`, `gateway/.goreleaser.yaml`) is on main since 2026-10-08, not in `gateway/v0.17.0`, whose GitHub Release carries no assets | `gateway/v0.17.0` (2026-10-07) |
| `evals` (Python) | `evals/vX.Y.Z` | GitHub Release assets (sdist, wheel); the intended PyPI name is `kelvran-evals`, and the first publish is blocked on trademark clearance (`RELEASE.md`, Pre-flight Blockers) | `evals/v0.10.1` (2026-09-22) |

Both follow [Semantic Versioning 2.0.0](https://semver.org/). The tag prefixes are permanent: `gateway/` is load-bearing for Go, whose nested-module rule requires a `gateway/v*` tag for `go install github.com/kelvran/gateway/gateway/cmd/gateway@latest` to resolve, and `evals/` disambiguates the second line. Image tags and release-asset names drop the prefix (`v0.17.0`, `kelvran-gateway_0.17.0_linux_amd64.tar.gz`); the binary's `-version` prints the bare version.

The cross-language contract in [`api/`](../api/README.md) (protobuf) is versioned on its own and gated by `buf breaking` in CI; a change to it is governed by `RELEASE.md`'s Contract-Version procedure, not by this file.

## 2. Public surface: what a version number covers

A change to anything in the left column is a **breaking change** if an existing, documented use stops working. Anything in the right column may change in any release without notice.

### `gateway`

| Public (covered by this policy) | Not public (may change any time) |
|---|---|
| The data-plane HTTP routes and their request/response shapes: `POST /v1/chat/completions` (buffered and SSE), `POST /v1/embeddings`, `GET /v1/models`, `GET /healthz`, `GET /readyz`; the OpenAI-shaped error envelope and its `type`/`code` vocabulary; the `id`/`object`/`created` envelope on completions and chunks | Error **message** text (only `type` and `code` are stable) |
| The admin API routes and their JSON bodies (`/admin/virtual_keys…`, `/admin/deployments/{name}/weight`, `/admin/prompts…`, `/admin/cache/erase`, `/admin/backup`, `/admin/config`, `/admin/audit`, `/admin/debug/pprof/`) and their four credential tiers (Admin, Viewer, CostViewer, Operator) | Log line messages and field names (best effort, not a contract) |
| Request and response headers: `Authorization: Bearer`, `Idempotency-Key`, `X-Kelvran-End-User-Id`, `X-Kelvran-Overhead-Duration-Ms`, `Retry-After` on 429, `Allow` on 405 from the three `/v1/*` routes (`/healthz` and `/readyz` answer a wrong method with a plain-text 405 and no `Allow`; the `Allow` header and the JSON 405 envelope are on main since 2026-10-08, not in `gateway/v0.17.0`, which has two `/v1/*` routes — no `GET /v1/models` — and answers a wrong method on all of them with a plain-text 405 and no `Allow`); the SSE framing (`data: {…}` frames, the in-band `data: {"error":…}` frame after a mid-stream failure — on main since 2026-10-08, not in `gateway/v0.17.0` — and the `data: [DONE]` sentinel) | Every Go package under `gateway/internal/` (there is no `pkg/`; nothing is importable) |
| `config.yaml` keys and their semantics — the annotated reference is [`gateway/config.example.yaml`](../gateway/config.example.yaml), but the parser in `gateway/internal/gateway/controlplane/config.go` is the authoritative key list: the example omits several keys it accepts (`deployments.*.kind`, `deployments.*.tls`, `deployments.*.sticky`, `deployments.*.allow_insecure_http`, `virtual_keys.*.allowed_regions`, `virtual_keys.*.billing_subject_id`, `guardrails.embed_sim.corpus_path`, `prompt.persist_path`) and shows `deployments.*.weight`/`cost_tier`, `admin.cost_viewer_token_env` and `admin.audit_log_path` only in comments; the key-by-key reference page is `docs/reference/config.md` | The on-disk format of the bbolt stores (`persist_path` files); move them only with `POST /admin/backup` and `-restore-store` |
| CLI flags: `-config`, `-validate`, `-version`, `-restore-store`, `-restore-from`, `-restore-force` | The `-version` line's exact spacing (its fields — version, commit, date, Go version, platform — are stable) |
| OpenTelemetry metric names, attributes and span attributes — the covered set is what the code registers (`gateway/internal/telemetry/telemetry.go` for instruments, `gateway/internal/telemetry/result.go` for span attributes); [`docs/operations/TELEMETRY.md`](operations/TELEMETRY.md) describes them but, as of 2026-10-08, has no rows for `gen_ai.client.operation.duration`, the four non-reasoning `gen_ai.client.inference.usage.*` counters, the two `gen_ai.client.inference.operation.*` histograms, the span attributes `kelvran.prompt.id`, `kelvran.prompt.version` and `kelvran.response_format.requested_not_enforced`, or the `fallback_hop` span event and its `kelvran.fallback.hop.*` attributes; the complete reference page is `docs/reference/metrics-and-logs.md` | Cache key derivation (keys change between releases; a cache is never carried across an upgrade) |
| The container contract: `ENTRYPOINT ["/gateway"]`, `CMD ["-config", "/config.yaml"]`, listening on `8080`, UID/GID `65532`, the CA bundle at `/etc/ssl/certs/ca-certificates.crt`, `linux/amd64` + `linux/arm64` | Build-time details: base image digest, Go toolchain version |
| Release-asset names and verification identities (`RELEASE.md`, "Verifying a release binary"); the package layout (`/usr/bin/kelvran-gateway`, `/usr/lib/systemd/system/kelvran-gateway.service`, `/etc/kelvran-gateway/`) | The systemd unit's hardening directives (they only tighten; a loosening would be a changelog `## Changed`) |

### `evals`

| Public | Not public |
|---|---|
| The CLI commands and their options: `evals run`, `promote`, `flag-candidates`, `ingest`, `cost-report`, `rollout`, `report`, `audit-corpus`, `check-corpus-staleness`, `trend show`, `trend alert` | Judge prompt text (versioned separately by `JUDGE_PROMPT_VERSION`; a bump makes scores non-comparable and is always a changelog entry) |
| The persisted `Run` and `Score` JSONL schemas (`evals/evals/models.py`) | Test fixtures and the regression corpus's internal layout |
| Nothing importable yet: the CLI is the only supported interface. A module becomes public when `evals/ARCHITECTURE.md` names it as such (none is named today) | Every module under `evals/evals/` |

## 3. Rules while the major version is 0

- **PATCH** (`0.Y.Z` → `0.Y.Z+1`) never breaks a public surface. Fixes and additive changes only.
- **MINOR** (`0.Y` → `0.Y+1`) may add surface freely and may break a public surface **only** when all of the following hold: the dated changelog entry is marked `**BREAKING**`, [`UPGRADE.md`](../UPGRADE.md) has a row for that version with migration steps, and — where the old surface can be kept alive meanwhile — a [`DEPRECATED.md`](../DEPRECATED.md) row announced it with the lead time section 5 requires (two minors or 90 days, whichever is later).
- `scripts/release-preflight.sh` enforces the first two mechanically at the tag (a `**BREAKING**` entry — matched case-insensitively on the bold marker, so write it exactly like that — without an `UPGRADE.md` table row for the release fails the build) and `.github/workflows/release.yml` runs it before building anything. The third is a review expectation, checked by the pull-request template.
- A security fix may break a public surface in a PATCH if the surface itself is the vulnerability; the changelog entry says so under `## Security` and `UPGRADE.md` gets its row.

## 4. The 1.0 promise

`gateway/v1.0.0` will be cut when all of these are true, not before and not for marketing: the OpenAPI document for the public routes (planned, [plan item 14](upgrade-research/kelvran-deep-research-round4-discoverability-2026-10-08.md)) has been frozen across two consecutive minors; `config.yaml` carries a `schema_version`; the support window below has been operated for two minors; the OpenSSF Best Practices badge is at Passing; and five consecutive releases have carried signed assets. From 1.0 on, a public surface changes incompatibly only in a MAJOR release, after at least six months in `DEPRECATED.md`. `evals/v1.0.0` follows its own copy of the same conditions, minus the OpenAPI and `schema_version` items.

## 5. Deprecation mechanics

1. A row in [`DEPRECATED.md`](../DEPRECATED.md): surface, version that deprecated it, planned removal (a version, at least two minors or 90 days later, whichever is later), replacement.
2. A `## Deprecated` changelog entry in the release that adds the row.
3. Where the gateway can detect use of the deprecated surface at runtime, it logs `deprecated_surface_used` at `WARN` once per process per surface and increments the `kelvran.deprecation.used` counter with the surface name as attribute. Nothing is deprecated today, so neither the log line nor the counter exists yet; the first deprecation adds both.
4. Removal happens in the planned release, with a `**BREAKING**` entry and an `UPGRADE.md` row, and the `DEPRECATED.md` row is kept with its removal version filled in.

## 6. Support window

- **Latest minor**: all fixes (bugs and security) as PATCH releases.
- **Previous minor**: security fixes only, for 90 days after the newer minor shipped.
- Anything older: unsupported; upgrade.

| Deployable | Latest minor | Previous minor | Security fixes for the previous minor until |
|---|---|---|---|
| `gateway` | `gateway/v0.17.0` (2026-10-07) | `gateway/v0.16.0` (2026-09-28) | 2027-01-05 |
| `evals` | `evals/v0.10.1` (2026-09-22) | `evals/v0.10.0` (2026-09-21) | 2026-12-21 |

Each release updates this table (`RELEASE.md` step); `scripts/release-preflight.sh` warns — as a GitHub Actions warning annotation in the release workflow — when the "Latest minor" column does not name the version being released. [`SECURITY.md`](../SECURITY.md) points here instead of carrying its own copy.

## 7. Pre-releases

A `-rc.N` or similar suffix marks a pre-release: the GitHub Release is flagged pre-release, the image tag and assets exist, and nothing in sections 3 or 6 applies to it — a pre-release may change or disappear without an `UPGRADE.md` row and has no support window. For `evals`, a pre-release tag must use a form that is also a valid PEP 440 version once the hyphen is dropped (see `.github/workflows/release-evals.yml`).

## 8. Finding out which version is running

- `kelvran-gateway -version` (or `gateway -version` from a source build) prints `kelvran-gateway <version> (<commit>, built <date>, <go>, <os/arch>)`; a source build says `dev`.
- Every start logs the same fields once as `build_info`.
- The published image carries `org.opencontainers.image.version` (and `revision`) as a label and, from 2026-10-08 on, as an index annotation: `docker buildx imagetools inspect --raw ghcr.io/kelvran/gateway:<tag>`.
- `evals`: `python -c 'import importlib.metadata as m; print(m.version("kelvran-evals"))'` in the environment that has it installed.

## 9. How the records stay current

- Pull requests: the template asks whether the change breaks or deprecates a public surface and, if so, for the `UPGRADE.md` / `DEPRECATED.md` rows.
- Release: `RELEASE.md`'s per-deployable steps move the changelog, bump versions, update the support-window table above, and run `scripts/release-preflight.sh`, which fails on a `**BREAKING**` entry without an `UPGRADE.md` row and warns when this file's table does not name the release.
- This file changes by pull request like any other; a change to sections 3–6 is itself a changelog `## Changed` entry for both deployables.
