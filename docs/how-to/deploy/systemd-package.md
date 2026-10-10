# Install the gateway as a systemd service from the Linux package

This page shows an operator how to install `kelvran-gateway` on a Linux host from the deb, rpm or apk Release asset, create the config and credentials files the shipped systemd unit expects, start the service, and upgrade it later. It is for operators who run one gateway process directly on a VM or bare-metal host rather than in a container.

Use this when you want the gateway managed by systemd on a host you control, with no Docker or Kubernetes in the path.

## Before you start

- A Linux host with systemd as PID 1 and `sudo`. The apk installs on Alpine and lays down the same files (binary, unit, example config, LICENSE, NOTICE), but the post-install script skips its `systemctl` commands when `systemctl` is absent, so an OpenRC host gets no service registration; service management there is not documented here.
- The assets of one `gateway/v<version>` GitHub Release: the package for your architecture, `checksums.txt` and `checksums.txt.sigstore.json`. Package names are `kelvran-gateway_<version>_linux_{amd64,arm64}.{deb,rpm,apk}`. `<version>` is the tag without its `gateway/v` prefix: tag `gateway/vX.Y.Z` gives `X.Y.Z`.
- `cosign` and the `gh` CLI if you want to verify the download in step 1. Neither is needed for the install itself.
- Upstream provider credentials, for example the value you will store under `OPENAI_API_KEY`. See [Provider credentials](../provider-credentials.md).

Which releases have packages: the release pipeline (`.github/workflows/release.yml` and `gateway/.goreleaser.yaml`) is on main since 2026-10-08, not in `gateway/v0.17.0`. The `gateway/v0.17.0` Release carries no assets, and tags at or before it cannot be rebuilt by the pipeline. Packages exist from the first `gateway/v*` tag after that. Do not look for a `.deb` on `gateway/v0.17.0`. Asset names and the verification identities are listed in [Release artifacts](../../reference/release-artifacts.md).

## What the package installs

| Path | What it is |
|---|---|
| `/usr/bin/kelvran-gateway` | Static binary (`CGO_ENABLED=0`) built from `gateway/cmd/gateway` |
| `/usr/bin/kelvran` | The companion CLI (`kelvran init` and `kelvran doctor`; since 2026-10-10), built from `gateway/cmd/kelvran` — see [the CLI reference](../../reference/kelvran-cli.md) |
| `/usr/lib/systemd/system/kelvran-gateway.service` | The unit; source is [`deploy/systemd/kelvran-gateway.service`](../../../deploy/systemd/kelvran-gateway.service) |
| `/etc/kelvran-gateway/config.example.yaml` | The example config, marked `config|noreplace`; source is [`gateway/config.example.yaml`](../../../gateway/config.example.yaml) |
| `/usr/share/doc/kelvran-gateway/LICENSE`, `/usr/share/doc/kelvran-gateway/NOTICE` | License files |

The package installs no `config.yaml` and no `env` file; you create both in steps 3 and 4. Its post-install script (`deploy/nfpm/postinstall.sh`) runs `systemctl daemon-reload` and `systemctl try-restart kelvran-gateway.service`, then exits 0. It never enables or starts the service: the example config's two virtual keys hash the public example secrets `example-team-alpha-secret-do-not-use` and `example-team-beta-secret-do-not-use`, so the gateway must not come up on that file.

## Steps

### 1. Verify the download

Run these in the directory that holds the downloaded files. The signing identity is the release workflow; the OIDC issuer is GitHub Actions.

```bash
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/kelvran/gateway/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum -c checksums.txt --ignore-missing
gh attestation verify kelvran-gateway_<version>_linux_amd64.deb -R kelvran/gateway
```

`sha256sum -c` prints `kelvran-gateway_<version>_linux_amd64.deb: OK` for the file you have. The same three commands are described in [RELEASE.md](../../../RELEASE.md), "Verifying a release binary".

### 2. Install the package

```bash
sudo dpkg -i kelvran-gateway_<version>_linux_amd64.deb
kelvran-gateway -version
```

`-version` prints one line, `kelvran-gateway <version> (<commit>, built <date>, go<ver>, linux/<arch>)`, where `<version>` is the release you installed. For rpm and apk see the variant below.

### 3. Create `config.yaml`

```bash
sudo cp /etc/kelvran-gateway/config.example.yaml /etc/kelvran-gateway/config.yaml
sudoedit /etc/kelvran-gateway/config.yaml
```

In the copy:

- Replace the `virtual_keys` entries with your own key hashes. See [Virtual keys and budgets](../virtual-keys-and-budgets.md).
- Set your `deployments`, each with an `api_key_env` (or the other `*_env` names) that names an environment variable, never a value.
- Keep `listen_addr: ":8080"` unless you also apply the port-below-1024 variant.
- Put every writable path under `/var/lib/kelvran-gateway`. The unit sets `ProtectSystem=strict`, so that directory (created by `StateDirectory=kelvran-gateway`) is the only writable location that survives a restart (`PrivateTmp=yes` gives the process a throwaway private `/tmp`). The example config's commented blocks already use it:

```yaml
budget:
  persist_path: "/var/lib/kelvran-gateway/budget.db"
admin:
  token_env: "KELVRAN_ADMIN_TOKEN"
  backup_dir: "/var/lib/kelvran-gateway/backups"
  audit_log_path: "/var/lib/kelvran-gateway/admin-audit.jsonl"
  persist_path: "/var/lib/kelvran-gateway/identity.db"
```

- Leave the file world-readable (0644). The service runs as a `DynamicUser` whose uid is allocated at start, so a root-only file is unreadable to `ExecStartPre`. The file holds key hashes and environment-variable names, never a secret.

Every key is described in the [configuration reference](../../reference/config.md).

### 4. Create the credentials file

```bash
sudo install -m 0600 /dev/null /etc/kelvran-gateway/env
sudoedit /etc/kelvran-gateway/env
```

Write one `KEY=value` line per credential, using the names your `config.yaml` references:

```
OPENAI_API_KEY=<your provider key>
KELVRAN_ADMIN_TOKEN=<your admin token>
```

The unit loads this file with `EnvironmentFile=-/etc/kelvran-gateway/env`; the leading `-` makes a missing file non-fatal. systemd reads it, not the gateway, so the 0600 root-only mode is correct. At startup the gateway resolves each name with `os.Getenv`. For a deployment credential (`api_key_env`, `access_key_id_env`, `secret_access_key_env`) a missing variable produces a warning that calls to that deployment will fail, not a startup error. `admin.token_env` (and `viewer_token_env`, `cost_viewer_token_env`, `operator_token_env`) and `config_propagation.signing_secret_env` are stricter: if the named variable is unset or empty the gateway refuses to start, the journal shows a `gateway exited` record with `admin.token_env "KELVRAN_ADMIN_TOKEN" is set but resolves to an empty environment variable — refusing to start an unauthenticated admin server`, and `Restart=on-failure` schedules another attempt after `RestartSec=2s`; fix this file, then run `sudo systemctl restart kelvran-gateway`. If step 3 set `admin.token_env`, `KELVRAN_ADMIN_TOKEN` must be present here.

### 5. Validate the config

```bash
kelvran-gateway -config /etc/kelvran-gateway/config.yaml -validate
```

Prints `config is valid` and exits 0. On a problem it prints `config error: ...` and exits 1. No `sudo` is needed: `-validate` reads the config file, checks that every deployment names a registered provider and that every fallback chain targets an existing deployment, and starts nothing. It reads no environment variables, so it passes before the `env` file exists. The unit's `ExecStartPre` runs this same command before every start.

`kelvran doctor` (on `main` since 2026-10-10, not in `gateway/v0.17.0`) goes further than `-validate`: `sudo kelvran doctor --config /etc/kelvran-gateway/config.yaml` also reads `/etc/kelvran-gateway/env` (the file the unit loads with `EnvironmentFile=`) and reports every credential variable the config names that is unset there, a credential file the unit's `DynamicUser` could not read (not world-readable), a persist path outside `/var/lib/kelvran-gateway/` or under `/home`/`/tmp` (which the unit hides), a config that is not world-readable (`0644` is the expected mode), an unpriced model and an invalid telemetry exporter — the things `-validate` deliberately cannot see. Without `sudo` the env file is unreadable and `doctor` says so; pass `--env-file` instead. See [the CLI reference](../../reference/kelvran-cli.md#kelvran-doctor).

### 6. Enable and start

```bash
sudo systemctl enable --now kelvran-gateway
```

## Verify it worked

```bash
systemctl is-active kelvran-gateway            # prints: active
curl -s http://127.0.0.1:8080/healthz          # prints: {"status":"ok"}
journalctl -u kelvran-gateway -n 30 --no-pager
```

`/healthz` answers `200` with `{"status":"ok"}` when the process is up and serving. It reports nothing about upstream providers. The journal holds `ExecStartPre`'s `config is valid` line followed by the gateway's JSON log lines; the gateway's first record is `build_info` with `version`, `commit`, `date`, `go_version` and `platform`. Log fields are listed in [Metrics and logs](../../reference/metrics-and-logs.md).

If the service does not reach `active`, `journalctl -u kelvran-gateway` shows the `ExecStartPre` output; a `config error:` line there is the same message step 5 prints. See [Troubleshooting](../troubleshooting.md).

## Variants

### rpm and apk

```bash
sudo rpm -i kelvran-gateway_<version>_linux_amd64.rpm
sudo apk add --allow-untrusted kelvran-gateway_<version>_linux_amd64.apk
```

The `nfpms` block of `gateway/.goreleaser.yaml` has no signature configuration, so none of the three packages is signed. `dpkg -i` and `rpm -i` install an unsigned package without an override; apk refuses one, hence `--allow-untrusted`. The release workflow's acceptance job installs only the amd64 deb; the rpm, apk and arm64 packages are built from the same block but are not install-tested there. Rely on step 1 (`checksums.txt` plus the cosign bundle) for integrity.

### Bind a port below 1024

The unit ships with `CapabilityBoundingSet=` and `AmbientCapabilities=` empty, so the process cannot bind a privileged port. The default `listen_addr: ":8080"` needs nothing. For a lower port, either front the gateway with a reverse proxy or add a drop-in:

```bash
sudo systemctl edit kelvran-gateway
```

```ini
[Service]
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE
```

Then `sudo systemctl restart kelvran-gateway`.

### Tarball install (no package manager)

```bash
tar -xzf kelvran-gateway_<version>_linux_amd64.tar.gz
./kelvran-gateway -version
```

The archive holds `kelvran-gateway`, `LICENSE`, `NOTICE` and `config.example.yaml`. It carries no unit file and runs no post-install; the systemd setup above is not part of this path.

### Build from source with `go install`

```bash
go install github.com/kelvran/gateway/gateway/cmd/gateway@latest
```

The binary lands at `$(go env GOPATH)/bin/gateway`, named `gateway` rather than `kelvran-gateway`. Until the first `gateway/v*` release after `gateway/v0.17.0` is tagged, `@latest` resolves to v0.17.0, which predates the `-version` flag (added 2026-10-08): `gateway -version` there exits 2 with `flag provided but not defined: -version`. From that next release on (or with `@main`), `-version` reports `dev` because no release ldflags are applied. See [README.md](../../../README.md).

## Upgrade

Read the release's notes in [UPGRADE.md](../../../UPGRADE.md) and [How to upgrade](../upgrade.md) first, then install the new package over the old one:

```bash
sudo dpkg -i kelvran-gateway_<new-version>_linux_amd64.deb
kelvran-gateway -version
systemctl status kelvran-gateway
```

The post-install script runs `systemctl daemon-reload` and `systemctl try-restart kelvran-gateway.service`: a running service restarts onto the new binary; a stopped or never-enabled one stays as it is. `config.yaml` and `env` are not package files and are left untouched; `config.example.yaml` is marked `config|noreplace`. On stop, systemd waits `TimeoutStopSec=60s` before escalating to SIGKILL, which covers the gateway's 50 s worst case after SIGTERM (30 s graceful shutdown, 15 s post-shutdown drain grace, 5 s telemetry flush).

To roll back, install the previous Release's package the same way. Assets are never replaced under an existing name, so the signed `checksums.txt` of each Release keeps naming the bytes you download; see the Rollback Procedure in [RELEASE.md](../../../RELEASE.md). Local bbolt state under `/var/lib/kelvran-gateway` stays in place for the replacement binary; see [Backup and restore](../backup-and-restore.md).

## How the unit confines the process

The unit runs the gateway as `DynamicUser=yes` with `StateDirectory=kelvran-gateway` and `ConfigurationDirectory=kelvran-gateway`, `Restart=on-failure`, `RestartSec=2s` and `KillSignal=SIGTERM`. Hardening on top of that: `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `PrivateDevices`, `ProtectKernelTunables`, `ProtectKernelModules`, `ProtectKernelLogs`, `ProtectControlGroups`, `ProtectClock`, `ProtectHostname`, `RestrictSUIDSGID`, `RestrictRealtime`, `RestrictNamespaces`, `LockPersonality`, `MemoryDenyWriteExecute`, `SystemCallArchitectures=native`, `SystemCallFilter=@system-service` followed by `SystemCallFilter=~@privileged` (`@resources` stays allowed because the Go runtime raises its own `RLIMIT_NOFILE` at startup), empty `CapabilityBoundingSet=` and `AmbientCapabilities=`, and `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`.

Per [docs/VERSIONING.md](../../VERSIONING.md), the package layout (`/usr/bin/kelvran-gateway`, `/usr/bin/kelvran` since 2026-10-10, `/usr/lib/systemd/system/kelvran-gateway.service`, `/etc/kelvran-gateway/`), the release-asset names and verification identities, and the CLI flags are part of the gateway's SemVer-covered surface. The unit's hardening directives are not; they only tighten between releases.

## Not available today

- A hosted apt or rpm repository. Packages are GitHub Release assets only; there is no `apt install kelvran-gateway`.
- A Homebrew tap or cask.
- Signed deb, rpm or apk packages (hence `--allow-untrusted` for apk).
- CI install tests for the rpm, apk and arm64 packages; only the amd64 deb is installed in the release workflow.
- A Windows service wrapper or a macOS launchd plist. Only a `windows_amd64.zip` archive and `darwin_{amd64,arm64}.tar.gz` archives are published.
- Scoop, winget or Nix packaging.
- A self-upgrade subcommand.
- Log-file or logrotate configuration. The gateway writes JSON to stdout, so journald is the log store under systemd.
- A shipped `/etc/kelvran-gateway/config.yaml` or `env` file; the operator creates both.
- Live rotation of anything in `/etc/kelvran-gateway/env`. systemd reads the file at start and the gateway reads each `*_env` name once, so a changed value needs `sudo systemctl restart kelvran-gateway`. Only `*_file` credential keys reload without a restart; see [rotate credentials](../rotate-credentials.md).
- Enabling or starting the service on install.
- An OpenRC service script for Alpine.

## Related pages

- Reference: [configuration](../../reference/config.md), [release artifacts](../../reference/release-artifacts.md), [metrics and logs](../../reference/metrics-and-logs.md), [admin API](../../reference/admin-api.md).
- How-to: [provider credentials](../provider-credentials.md), [virtual keys and budgets](../virtual-keys-and-budgets.md), [rotate credentials](../rotate-credentials.md), [backup and restore](../backup-and-restore.md), [upgrade](../upgrade.md), [troubleshooting](../troubleshooting.md).
- Other deploy targets: [Docker Compose](docker-compose.md), [Kubernetes with Kustomize](kubernetes-kustomize.md), [ECS Fargate](ecs-fargate.md).
- Background: [docs/operations/DEPLOY.md](../../operations/DEPLOY.md), [docs/operations/FAILURE-MODES.md](../../operations/FAILURE-MODES.md), [SECURITY.md](../../../SECURITY.md), [versioning explained](../../explanation/versioning.md).
