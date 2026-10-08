# Back up and restore the gateway's bbolt stores

This page shows an operator how to take a live backup of the gateway's bbolt-backed stores (virtual keys, budget spend, prompts) through the admin API, and how to put a backup file back in place with the offline `-restore-store` procedure after a corrupted or lost file. It is for whoever runs a single-process gateway with `persist_path` set. It also states what Redis-backed state is not covered.

Use this when you have set `admin.persist_path`, `budget.persist_path` or `prompt.persist_path` and need a recovery point, or when a gateway refuses to start because one of those files is corrupt.

Every route, flag and key on this page is in gateway/v0.17.0 unless marked otherwise.

## Prerequisites

- A gateway with at least one of `admin.persist_path`, `budget.persist_path`, `prompt.persist_path` set. A store with no `persist_path` is in-memory only and has nothing to back up.
- An admin server (`admin.token_env`) and the Admin-tier bearer token. `POST /admin/backup` accepts the Admin token only, never the Viewer, CostViewer or Operator token. See [Admin API roles](admin-api-rbac.md).
- Shell access to the host for the restore step. Restore is an offline command that runs where the `persist_path` file lives.
- Stores backed by Redis (`admin.redis_addr`, `budget.redis_addr`) are out of scope. See [Redis-backed stores](#redis-backed-stores) below.

## What a backup contains

| Store | Config key | Backup file | Contents |
|---|---|---|---|
| identity | `admin.persist_path` | `identity-<timestamp>.bbolt` | virtual keys: ids, key hashes, limits |
| budget | `budget.persist_path` | `budget-<timestamp>.bbolt` | per-key spend records |
| prompt | `prompt.persist_path` | `prompt-<timestamp>.bbolt` | prompt templates and versions |

Each file is a byte copy of the whole database, taken inside a bbolt read transaction. A key deleted later with `DELETE /admin/virtual_keys/{name}` also loses its persisted budget record, but a backup taken earlier still holds both (see [Data subject requests](../operations/DATA-SUBJECT-REQUESTS.md)).

Not in a backup: the admin audit JSONL at `admin.audit_log_path` (it is not bbolt), and anything held in Redis.

## Steps

### 1. Configure persistence and the backup directory

Add the keys below to `config.yaml`. The identity, budget and backup paths follow [gateway/config.example.yaml](../../gateway/config.example.yaml), which has no `prompt` example; `prompt.persist_path` and every other key are documented in [Configuration reference](../reference/config.md).

```yaml
admin:
  listen_addr: "127.0.0.1:8081"
  token_env: "KELVRAN_ADMIN_TOKEN"
  persist_path: "/var/lib/kelvran-gateway/identity.db"
  backup_dir: "/var/lib/kelvran-gateway/backups"
  on_corrupt_store: "fail"
budget:
  persist_path: "/var/lib/kelvran-gateway/budget.db"
prompt:
  persist_path: "/var/lib/kelvran-gateway/prompt.db"
```

- `admin.backup_dir` enables `POST /admin/backup`. When it is unset the route is still registered and answers `501 Not Implemented` with the body `admin.backup_dir is not configured`.
- `admin.on_corrupt_store` accepts `fail` (default) or `reset`. Any other value fails config load. Its behaviour is described in [Recover from a corrupt file](#recover-from-a-corrupt-file).
- Under the systemd package, `/var/lib/kelvran-gateway` is the only writable path (`StateDirectory=kelvran-gateway` plus `ProtectSystem=strict` in [deploy/systemd/kelvran-gateway.service](../../deploy/systemd/kelvran-gateway.service)). Both the `persist_path` files and `backup_dir` must live under it. The systemd package (the unit, `/usr/bin/kelvran-gateway`, `/etc/kelvran-gateway/`) is on main since 2026-10-08, not in gateway/v0.17.0; see [Deploy as a systemd package](deploy/systemd-package.md).

### 2. Create the backup directory

The gateway does not create `admin.backup_dir`. It must exist and be writable by the gateway's uid before the first backup, or the copy fails and the route answers `500`.

```sh
sudo mkdir -p /var/lib/kelvran-gateway/backups
```

Under systemd with `DynamicUser=yes`, ownership of a directory created by hand inside `StateDirectory` is not documented here. Check that the service can write to it with the verification step below.

### 3. Take a live backup

The gateway keeps serving traffic during a backup. Each store is copied in its own read transaction, which bbolt documents as safe to run alongside writers.

```sh
curl -sS -X POST \
  -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN" \
  http://127.0.0.1:8081/admin/backup
```

Responses:

- `200` with `{"files":["identity-20261008T120000Z.bbolt","budget-20261008T120000Z.bbolt","prompt-20261008T120000Z.bbolt"]}`. The list holds only the stores that have a `persist_path` and are bbolt-backed; others are skipped silently, not reported as errors. When no store qualifies the response is still `200` and the body is `{"files":null}`.
- `500` with the error text if any store's copy fails. One store's failure does not stop the others; the errors are joined into the body. Files that were copied successfully in the same request stay on disk but are not listed; a retry one second later writes new timestamps and never overwrites them.
- `501` with `admin.backup_dir is not configured`.

Filenames are `<kind>-<UTC time as YYYYMMDDTHHMMSSZ>.bbolt`, written with mode `0600`. An existing file at that path is never overwritten; the copy fails with `backup: <path> already exists, refusing to overwrite`. The timestamp has one-second resolution, so two backup requests within the same second collide on the second request.

The backup runs on the replica that received the request only. It is not propagated to other replicas.

The gateway logs `admin_backup_completed` with `files` and `authorized_by=admin` on success, provided `admin.enable_audit_log` is `true` (the default). With it set to `false` a successful backup leaves no log line at all; the `200` response is then the only confirmation.

### 4. Restore offline

Restore is a one-shot CLI mode of the gateway binary. It must run against a stopped gateway: every bbolt store holds an exclusive file lock while the process runs, and replacing the file under a live process is an unsupported concurrent-access hazard.

```
gateway -config <config.yaml> -restore-store <identity|budget|prompt> -restore-from <backup-file> [-restore-force]
```

- `-restore-store` must be exactly `identity`, `budget` or `prompt`. One store per invocation.
- `-restore-from` is required. The file must open as a bbolt database; Restore checks this read-only before it touches the destination.
- The destination is resolved from the same config file: `identity` to `admin.persist_path`, `budget` to `budget.persist_path`, `prompt` to `prompt.persist_path`. A store with no `persist_path` fails with `<kind> store has no persist_path configured -- nothing to restore into`.
- `-restore-force` is needed whenever the destination file exists. Without it Restore refuses with `backup: <dest> already exists, refusing to overwrite (pass force to override)`.
- The copy is atomic: the bytes go to a temp file named `<dest>.restoring-*` in the destination directory, are fsynced, set to mode `0600`, then renamed into place. The temp file is removed on every error path.
- The flag is evaluated before `-validate` and never starts a listener. On success it prints `restored <kind> store from "<path>"` and exits `0`; on failure it prints `restore error: ...` to stderr and exits `1`.

On a systemd install (package on main since 2026-10-08, not in gateway/v0.17.0; on a v0.17.0 host run the same flags against your own `gateway` binary and config path, with the process stopped):

```sh
sudo systemctl stop kelvran-gateway
sudo /usr/bin/kelvran-gateway -config /etc/kelvran-gateway/config.yaml \
  -restore-store identity \
  -restore-from /var/lib/kelvran-gateway/backups/identity-20261008T120000Z.bbolt \
  -restore-force
# prints: restored identity store from "/var/lib/kelvran-gateway/backups/identity-20261008T120000Z.bbolt"
sudo systemctl start kelvran-gateway
```

The next normal start loads the restored file through the store's regular constructor. There is no separate "restored" mode. Restore writes the file with mode `0600` owned by the user who ran the command; whether the service's dynamic uid can then open it is not documented here.

Before the stop step, check for `identity_persist_failed` or `budget_persist_failed` log lines (metric `kelvran.persistence.failed`). An admin mutation whose write failed is live in memory only and is lost on stop; re-drive it first (see row R12 in [Failure modes](../operations/FAILURE-MODES.md)). The prompt store has no such signal: a failed prompt write produces no log line and no metric, only a `400` whose body starts `prompt: Upsert: persisting` or a `500` whose body starts `prompt: Delete: persisting removal` on the admin request itself (row P6). If any such response was seen since the last start, re-POST the prompt (or re-POST then DELETE) before stopping.

## Variants

### Restore into a fresh host

If the destination `persist_path` does not exist yet, omit `-restore-force`. The destination directory must already exist: Restore does not create it and fails with `restore error: restoring <kind> store: backup: creating a temp file next to <dest>: ... no such file or directory`. Under the systemd package `/var/lib/kelvran-gateway` is created by `StateDirectory=` on the first service start, so on a never-started host start and stop the service once (`sudo systemctl start kelvran-gateway && sudo systemctl stop kelvran-gateway`) before restoring; that first start also creates each configured `persist_path` file empty, so the restore that follows then needs `-restore-force`. A missing file is otherwise created empty with its bucket at first start, so a fresh install without a restore needs no action.

### Recover from a corrupt file

`admin.on_corrupt_store` decides what happens when bbolt returns `ErrInvalid`, `ErrVersionMismatch` or `ErrChecksum` at open:

- `fail` (default): the gateway exits `1`. Restore from a backup with `-restore-force`, since the corrupt file still occupies the path. Move it aside first if you want to keep it; Restore replaces the destination.
- `reset`: the file is renamed to `<path>.corrupt-<unix-seconds>-<nanoseconds>` (never deleted), the gateway logs `persist_store_open_failed` then `persist_store_reset`, and a fresh empty store opens. If the rename itself fails, the gateway logs `persist_store_corrupt_backup_failed` and exits `1` with the original open error; nothing is reset and the corrupt file stays in place. To get the data back, stop the gateway and restore from a backup with `-restore-force`. A `.corrupt-*` file is a valid `-restore-from` source only if bbolt can still open it; Restore's validation step decides.

`reset` never acts on a locked file, a permissions error or a disk-full error; those exit `1` unchanged. A corrupt JSON value inside an otherwise healthy file is a hydration failure (`hydrating virtual keys from "<path>"`, `hydrating budget tracker from "<path>"`, `hydrating prompt store from "<path>"`), also exits `1`, and `reset` does not rescue it. Restore from backup is the remedy.

### Kubernetes

The shipped base in [deploy/k8s/README.md](../../deploy/k8s/README.md) mounts no PVC, runs with `readOnlyRootFilesystem: true` and `replicas: 2`. Adding any `persist_path` needs a writable volume, `replicas: 1` and `strategy: { type: Recreate }`, because an RWO volume cannot be shared across a rolling update. See [Deploy with Kustomize](deploy/kubernetes-kustomize.md). The restore command then runs inside a pod or a job that mounts the same volume while no gateway pod holds the file.

### Redis-backed stores

When `admin.redis_addr` is set, the identity store lives in Redis; when `budget.redis_addr` is set, the budget tracker does. If both `redis_addr` and `persist_path` are set for the same store, Redis wins and the gateway logs `identity_redis_addr_and_persist_path_both_set` or `budget_redis_addr_and_persist_path_both_set`. The bbolt file is then neither read nor backed up, and a `-restore-store` into it has no effect on the running gateway.

Kelvran ships no backup or restore for Redis-backed state. The only in-repo Redis durability setting is the local development profile in [docker-compose.yml](../../docker-compose.yml): `redis-server --appendonly yes --appendfsync everysec` with a named volume, started with `docker compose --profile redis up`. Production Redis durability is the operator's responsibility; [Deploy](../operations/DEPLOY.md) records targets, not provisioned capability.

## Verify it worked

Backup:

```sh
ls -l /var/lib/kelvran-gateway/backups/
# -rw------- 1 <uid> <gid> <size> Oct  8 12:00 identity-20261008T120000Z.bbolt
```

Each file listed in the `200` response is present with mode `-rw-------`, and, with `admin.enable_audit_log` on, the gateway log contains `admin_backup_completed`.

Restore:

```sh
sudo /usr/bin/kelvran-gateway -config /etc/kelvran-gateway/config.yaml \
  -restore-store identity -restore-from <backup-file> -restore-force; echo "exit=$?"
# restored identity store from "<backup-file>"
# exit=0
ls -l /var/lib/kelvran-gateway/identity.db <backup-file>
```

The destination and the backup have the same byte size, and no `identity.db.restoring-*` file remains. After `systemctl start`, the gateway log has no `persist_store_open_failed` and no `hydrating ...` error, and the process stays up.

## Troubleshooting

- `restore error: -restore-from is required when -restore-store is set`: pass both flags.
- `restore error: unknown -restore-store "..." (must be one of: identity, budget, prompt)`: fix the store name.
- `restore error: restoring <kind> store: backup: <file> is not a valid, openable bbolt database, refusing to restore from it: <bbolt error>` (for example `: timeout` when another process still holds the file): the source is not a bbolt file, or another process still holds it open (the validation open waits 2 s, then fails).
- Gateway fails to start with `boltstore: opening <path>: another process holds the file lock (waited 1s): timeout`: a second process has the file. Stop it, then start again. This one-second failure is on main since 2026-10-08, not in gateway/v0.17.0; in gateway/v0.17.0 the open blocks with no log line (row P2 in [Failure modes](../operations/FAILURE-MODES.md)).
- `POST /admin/backup` answers `500` with `already exists, refusing to overwrite`: two requests in one second; retry after a second.

## Not available today

- No scheduled or automatic backup, no retention or pruning of `admin.backup_dir`, and no metric for backup success or failure. The `admin_backup_completed` log line (emitted only with `admin.enable_audit_log` on) is the only signal.
- No `GET /admin/backup` to list existing backups. Only `POST` is registered.
- No online or live restore, and no `-restore-store all`. One store per invocation, gateway stopped.
- No backup or restore of Redis-backed stores or of the audit JSONL.
- The gateway does not create `admin.backup_dir`.
- No backup verification command beyond Restore's own read-only open check, and no checksum manifest.

## Related

- [Configuration reference](../reference/config.md): `admin.persist_path`, `admin.backup_dir`, `admin.on_corrupt_store`, `admin.redis_addr`, `budget.persist_path`, `budget.redis_addr`, `prompt.persist_path`, `admin.audit_log_path`
- [Admin API reference](../reference/admin-api.md): `POST /admin/backup`
- [Metrics and logs reference](../reference/metrics-and-logs.md): `kelvran.persistence.failed`, `admin_backup_completed`, `persist_store_*`
- [Admin API roles](admin-api-rbac.md)
- [Virtual keys and budgets](virtual-keys-and-budgets.md) and [Prompt management](prompt-management.md): what the identity, budget and prompt stores hold
- [Deploy as a systemd package](deploy/systemd-package.md) and [Deploy with Kustomize](deploy/kubernetes-kustomize.md)
- [Upgrade](upgrade.md): take a backup before changing versions
- [Failure modes](../operations/FAILURE-MODES.md): rows P1 to P7 and R12
