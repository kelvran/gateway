# Upgrade the gateway to a new release

This page walks an operator through moving a running gateway from one release to a newer one: reading what the version number promises, picking and verifying the new image or package, backing up on-disk state, restarting inside the gateway's stop budget on systemd, Kubernetes or ECS, and confirming the new build is serving. It is for whoever runs `kelvran-gateway` in production. For the policy itself read [`docs/VERSIONING.md`](../VERSIONING.md); for cutting a release read [`RELEASE.md`](../../RELEASE.md).

**Use this when** a new `gateway/vX.Y.Z` tag exists and you want to run it without losing in-flight requests, persisted virtual keys or budgets.

## Prerequisites

- A running gateway and its `config.yaml`. The annotated key reference is [`gateway/config.example.yaml`](../../gateway/config.example.yaml); the generated page is [`../reference/config.md`](../reference/config.md).
- Access to the admin API if any store uses a bbolt `persist_path` (`admin.persist_path`, `budget.persist_path`, `prompt.persist_path`). The admin listener defaults to `127.0.0.1:8081`; the admin credential is the environment variable named by `admin.token_env` (`KELVRAN_ADMIN_TOKEN` in the commented-out `admin:` example in `config.example.yaml`).
- `cosign` and `gh` on the machine that downloads artifacts, and `docker buildx` if you deploy by image digest.

## What a version number promises

- Releases are git tags `gateway/vX.Y.Z`. The image tag drops the prefix: `ghcr.io/kelvran/gateway:vX.Y.Z`. Every push to `main` also publishes `:latest` and `:sha-<40-hex commit>`, whose binaries report version `0.0.0-main.<12-hex>`; do not treat those as releases.
- While the major version is 0: a PATCH never breaks a public surface. A MINOR may break one only with a `**BREAKING**` changelog entry and a matching row in [`UPGRADE.md`](../../UPGRADE.md). `scripts/release-preflight.sh` fails the tag build when the row is missing. The gateway has shipped no breaking change to a released surface so far; the only `UPGRADE.md` row is `evals/v0.2.0`.
- The public surface includes the data-plane routes and error `type`/`code` vocabulary, the admin routes and credential tiers, `config.yaml` keys, the CLI flags (`-config`, `-validate`, `-version`, `-restore-store`, `-restore-from`, `-restore-force`), the OpenTelemetry names and the container contract. Not covered: the bbolt on-disk format, cache key derivation, log line text and Go packages.
- Support window: the latest minor gets all fixes; the previous minor gets security fixes for 90 days. As of 2026-10-08, `gateway/v0.17.0` (2026-10-07) is latest and `gateway/v0.16.0` receives security fixes until 2027-01-05.
- Nothing is deprecated today (`DEPRECATED.md` has an empty table).

See [`../explanation/versioning.md`](../explanation/versioning.md) for the reasoning behind these rules.

## Steps

### 1. Read what the release changes

Read `gateway/changelog/<version>.md` for the target release, then check `UPGRADE.md` for a row naming that version and `DEPRECATED.md` for new rows. New config keys are called out in the changelog (`docs/reference/config.md` marks keys that are on `main` but not yet in the latest release), and the parser ignores top-level and section keys it does not know at load time (the one strict spot is a `models.<name>` entry, whose unknown fields are a load error), so a config written for a newer release loads on an older binary with the new key silently inactive, and `-validate` does not flag it.

### 2. Record what runs now

Record the image tag and digest, or the package version, of the running build. `kelvran-gateway -version` and the `build_info` log line are on main since 2026-10-08, not in `gateway/v0.17.0`; on a v0.17.0 install, record the image digest from your deployment manifest instead (no v0.17.0 package exists, so `dpkg -s kelvran-gateway` applies only from the first packaged release on).

### 3. Back up bbolt state

If any store has a `persist_path`, take a live backup before changing binaries. `POST /admin/backup` is always registered on the admin server; it writes files only when `admin.backup_dir` is set and answers 501 otherwise. If you get the 501, add `admin.backup_dir` to `config.yaml` and restart the current binary first (the key is read only at startup), then take the backup:

```bash
curl -sS -X POST -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN" http://127.0.0.1:8081/admin/backup
# 200 {"files":["/var/lib/kelvran-gateway/backups/..."]}
# 501 "admin.backup_dir is not configured" when backup_dir is unset
```

A store written by a newer binary is not guaranteed readable by an older one: the on-disk format is not a public surface. The backup is what makes a rollback possible. Redis-backed stores (`admin.redis_addr`, `budget.redis_addr`) outlive the process and need no backup for an upgrade. Full procedure: [`backup-and-restore.md`](backup-and-restore.md).

### 4. Verify the artifact

For an image, verify the signature on the tag before pinning it:

```bash
cosign verify ghcr.io/kelvran/gateway:v<X.Y.Z> \
  --certificate-identity-regexp '^https://github.com/kelvran/gateway/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
docker buildx imagetools inspect ghcr.io/kelvran/gateway:v<X.Y.Z>   # copy the top-level Digest: sha256:...
```

Images built on or after 2026-10-08 are one index covering `linux/amd64` and `linux/arm64`, signed with `cosign sign --recursive`. Pin the index digest, not a per-platform digest. The `v0.17.0` image predates this and is `linux/amd64` only.

For a release archive or package, verify the signed checksums file, then the asset:

```bash
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/kelvran/gateway/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum -c checksums.txt --ignore-missing
gh attestation verify kelvran-gateway_<version>_linux_amd64.tar.gz -R kelvran/gateway
```

Releases up to and including `gateway/v0.17.0` have no GitHub Release assets and cannot be rebuilt by the pipeline; the first packaged release is the next one. Asset names and tags: [`../reference/release-artifacts.md`](../reference/release-artifacts.md).

### 5. Validate your config against the new binary

`kelvran-gateway -config <path> -validate` prints `config is valid` and exits 0, or prints `config error: ...` and exits 1. The archive carries the binary, so you can run the new build's `-validate` against the current config before installing anything:

```bash
mkdir -p /tmp/kelvran-new && tar -xzf kelvran-gateway_<version>_linux_amd64.tar.gz -C /tmp/kelvran-new
/tmp/kelvran-new/kelvran-gateway -config /etc/kelvran-gateway/config.yaml -validate
```

### 6. Roll the restart

The gateway begins shutting down on `SIGTERM` or `SIGINT` and logs `gateway shutting down`. Worst case it needs 50 s: 30 s for the client and admin listeners to drain, 15 s more for handlers still running, 5 s for the final telemetry flush. A second signal during the drain force-kills the process. If the drain gives up it logs `gateway_shutdown_forced_with_requests_still_in_flight`; a flush that gives up logs `telemetry_shutdown_failed`. The 5 s telemetry bound is on main since 2026-10-08, not in `gateway/v0.17.0`, where the final flush had no deadline and a collector that accepted connections but never answered could hold the process past the supervisor's stop timeout.

Two facts shape the restart on every platform:

- Mixed versions during a rollout are tolerated. A replica that receives a config-propagation event type it does not know skips it instead of failing. That tolerance covers unknown event *types*, not new *fields* on `virtual_key_upsert`: a replica built before a field re-persists, without it, every key it mutates and every key whose `virtual_key_upsert` event it receives, and the receive side's carry-forward (on `main` since 2026-10-10, for `expires_at`) protects only replicas that already have the field. Finish the rollout on every replica sharing `admin.redis_addr` / `config_propagation` before setting `expires_at` on any key, and re-upsert any key given an expiry during a mixed window once the last old replica is gone.
- A bbolt `persist_path` file belongs to one process. A second process opening it fails with `another process holds the file lock (waited 1s)`; that 1 s timeout is on main since 2026-10-08, not in `gateway/v0.17.0`, which blocks forever with no log line. Either way the restart on a host with `persist_path` must be stop-then-start.

Pick the variant below for your platform.

#### systemd package (deb, rpm, apk)

The package installs `/usr/bin/kelvran-gateway`, `/usr/bin/kelvran` (the companion CLI, since 2026-10-10), the unit at `/usr/lib/systemd/system/kelvran-gateway.service` and `/etc/kelvran-gateway/config.example.yaml`. Its post-install runs `systemctl daemon-reload` and `systemctl try-restart kelvran-gateway.service`: a running service picks up the new binary; a stopped one stays stopped. The unit's `ExecStartPre` runs `-validate` with the new binary, `TimeoutStopSec=60s` exceeds the 50 s budget, `KillSignal=SIGTERM`, and `Restart=on-failure` with `RestartSec=2s` retries a failed start.

```bash
sudo dpkg -i kelvran-gateway_<version>_linux_amd64.deb      # or rpm -U / apk add --allow-untrusted
kelvran-gateway -version
journalctl -u kelvran-gateway --since '-5 min' | grep -E 'build_info|gateway listening'
curl -s http://127.0.0.1:8080/healthz
```

The unit itself is [`deploy/systemd/kelvran-gateway.service`](../../deploy/systemd/kelvran-gateway.service); setup is in [`deploy/systemd-package.md`](deploy/systemd-package.md).

#### Kubernetes (Kustomize base)

`deploy/k8s/base/deployment.yaml` runs `replicas: 2` with `terminationGracePeriodSeconds: 60` (above the 50 s budget plus the native 5 s `preStop` sleep), no explicit `strategy` (the Kubernetes default rolling update), and a PodDisruptionBudget with `minAvailable: 1`. Both probes point at `/healthz`. The image is pinned as `ghcr.io/kelvran/gateway:v0.17.0@sha256:...` and goes stale with each release; edit that `image:` value to the new tag plus its index digest, apply, and watch the rollout:

```bash
kubectl apply -k deploy/k8s/base
kubectl -n kelvran rollout status deployment/gateway
kubectl -n kelvran logs -l app.kubernetes.io/name=kelvran-gateway --tail=-1 --prefix | grep build_info
```

Compare the `build_info` lines across pods to detect version skew. If you mounted a PVC for a bbolt `persist_path`, the manifest's own comment applies: run `replicas: 1` with `strategy: { type: Recreate }`, because a rolling update would wait on the file lock. Details: [`deploy/k8s/README.md`](../../deploy/k8s/README.md) and [`deploy/kubernetes-kustomize.md`](deploy/kubernetes-kustomize.md).

#### Plain Docker

Stop the old container, start the new one from the pinned digest. The repository's `docker-compose.yml` builds the gateway from source (`build:`), so it is a development setup, not a published-image upgrade path. The image contract (`ENTRYPOINT ["/gateway"]`, `CMD ["-config", "/config.yaml"]`, port `8080`, UID `65532`) is in [`../reference/container-image.md`](../reference/container-image.md).

```bash
docker run --rm ghcr.io/kelvran/gateway:v<X.Y.Z>@sha256:<index-digest> -version
```

#### ECS/Fargate

`deploy/ecs/main.tf` is a task-definition-only module: set `var.image` to the new tag plus index digest and roll the service. The module sets no container `healthCheck` on purpose (the image has no shell), so health comes from an ALB target-group check on `/healthz`. See [`deploy/ecs/README.md`](../../deploy/ecs/README.md) and [`deploy/ecs-fargate.md`](deploy/ecs-fargate.md), and the stop-timeout gap under "Not available today".

### 7. Confirm the new version

On the new build, `kelvran-gateway -version` prints `kelvran-gateway <version> (<commit>, built <date>, <go version>, <os>/<arch>)`; a source build prints `kelvran-gateway dev (none, built unknown, ...)`. Every start logs the same fields once as `build_info` with keys `version`, `commit`, `date`, `go_version`, `platform`.

## Rolling back

Rollback is redeploying the previous tag or package. Release assets are never replaced under an existing name, because the signed `checksums.txt` names the original bytes, so the previous asset is still the one whose checksum and provenance you checked in step 4. For a bbolt store, stop the process, then restore the step-3 backup with `-restore-store <identity|budget|prompt> -restore-from <file>` (add `-restore-force` to overwrite an existing file); the gateway must be stopped for the restore. Response caches are in-process and empty on every restart, so nothing needs flushing in either direction.

## Verify it worked

```bash
curl -s http://127.0.0.1:8080/healthz
# {"status":"ok"}                              200 while the process serves; no auth, no dependencies
curl -s -o /dev/stdout -w '\n%{http_code}\n' http://127.0.0.1:8080/readyz
# {"models":{"gpt-4o":true},"ready":true}      200; 503 when any model has no healthy deployment
```

Two caveats on `/readyz`. With `health_probe.interval_seconds` unset or 0, probing is off, every deployment counts as healthy, and `/readyz` is 200 regardless. In `gateway/v0.17.0`, with `health_probe` configured and any `kind: embedding` deployment, `/readyz` returns 503 permanently because the probe was chat-shaped; this is fixed on main since 2026-10-08. Do not gate an upgrade on `/readyz` from a v0.17.0 replica in that configuration. Keep Kubernetes probes on `/healthz`; point external monitors at `/readyz`. Route reference: [`../reference/data-plane-api.md`](../reference/data-plane-api.md). Log and metric names: [`../reference/metrics-and-logs.md`](../reference/metrics-and-logs.md).

## What the next release brings

These are on main after `gateway/v0.17.0`, not in `gateway/v0.17.0`: `-version` and `build_info`; the OpenAI-shaped JSON error envelope on data-plane errors (status codes unchanged, `text/plain` bodies become `application/json`); `GET /v1/models`; cache keys that include `tools` and `tool_choice` (since 2026-10-08, so entries written by an earlier build are unreachable after upgrading, which is invisible because every cache layer `cmd/gateway` builds is in-process and empty on each restart; no remote cache exists that could hold the old entries); OpenAI `tool_choice` wire forms; kind-aware health probes; `id`/`object`/`created` on every completion; an empty credential file treated as a read failure; the bounded telemetry flush and the bbolt lock timeout described above. Read `gateway/changelog/unreleased.md` for the full list until the release is cut.

## Not available today

- ECS: `deploy/ecs/main.tf` sets no `stopTimeout`, and the ECS default is below the gateway's 50 s drain. Add a `stopTimeout` of at least 60 s to your task definition yourself.
- No `schema_version` in `config.yaml`. `-validate` cannot tell a key from a newer release apart from a typo; both are ignored.
- No runtime deprecation signal. The `deprecated_surface_used` log line and `kelvran.deprecation.used` counter do not exist yet; the first deprecation adds them.
- No Helm chart. The Kubernetes image re-pin in `deployment.yaml` is a manual edit.
- No HTTP endpoint exposes the running version. Only `-version` and the `build_info` log line do.
- No migration tooling for bbolt stores between releases; move them only via backup and restore.
- No canary or progressive rollout by gateway version. `POST /admin/deployments/{name}/weight` shifts traffic between upstream deployments, not between gateway builds.

## Related

- [`UPGRADE.md`](../../UPGRADE.md), [`docs/VERSIONING.md`](../VERSIONING.md), [`RELEASE.md`](../../RELEASE.md), [`SECURITY.md`](../../SECURITY.md)
- [`docs/operations/DEPLOY.md`](../operations/DEPLOY.md), [`docs/operations/FAILURE-MODES.md`](../operations/FAILURE-MODES.md) (rows P2 and I4)
- [`../reference/admin-api.md`](../reference/admin-api.md), [`../reference/compatibility.md`](../reference/compatibility.md), [`rotate-credentials.md`](rotate-credentials.md), [`troubleshooting.md`](troubleshooting.md)
