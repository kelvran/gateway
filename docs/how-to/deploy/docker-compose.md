# Deploy the gateway with Docker Compose

This page brings the Kelvran gateway up on one machine with the repository's `docker-compose.yml`, and shows how to opt in to Redis, a second gateway instance, the Grafana/Prometheus/Tempo bundle, the Vector S3 shipper and Toxiproxy. It is for operators and developers who want a local or single-host stack. The Compose file is a local/dev bring-up; the production targets are [Kubernetes](kubernetes-kustomize.md), [ECS/Fargate](ecs-fargate.md) and [the systemd package](systemd-package.md).

Use this when you want the gateway running in a container on your own host in a few minutes, with every optional dependency one `--profile` flag away.

## Prerequisites

- Docker Engine with the `docker compose` plugin. The file uses `profiles:`, so the `--profile` flag must be available.
- A clone of the repository: the Compose file builds the image from `./gateway/Dockerfile` and bind-mounts files from the checkout.
- Provider credentials for the deployments you configure, as environment-variable values you set out of band. See [Provider credentials](../provider-credentials.md).

## Steps

1. Create the gateway config. It is gitignored; only `config.example.yaml` is committed.

   ```bash
   cp gateway/config.example.yaml gateway/config.yaml
   ```

   Edit `virtual_keys` and `deployments`. Keep `listen_addr: ":8080"`: the key is required, and 8080 is the container port Compose publishes. Every key is documented in [Configuration reference](../../reference/config.md) and inline in [`gateway/config.example.yaml`](../../../gateway/config.example.yaml). Example secrets in that file carry `do-not-use` and must never be used for anything real.

2. Create the env file. The `gateway` service reads `.env` through `env_file`, so the file must exist. It is gitignored.

   ```bash
   cp .env.example .env
   ```

   `.env.example` ships two empty variables, `OPENAI_API_KEY=` and `ANTHROPIC_API_KEY=`, covering the example config's `gpt4o-primary` and `claude-opus-primary` deployments only. Its `gemini-flash-primary` and `claude-bedrock-primary` deployments name `GEMINI_API_KEY` and `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`: add those to `.env` or delete the two deployments from `config.yaml`, otherwise the gateway starts with one `… env var is not set; calls to this deployment will fail` warning per missing variable. Set one variable per `api_key_env` (or `access_key_id_env`, `secret_access_key_env`, `session_token_env`) that your `config.yaml` names. An unset variable is a startup warning, not an error: the process starts and that deployment fails at request time.

3. Start the gateway.

   ```bash
   docker compose up gateway
   ```

   A bare `up` starts only `gateway`; every other service is behind a profile (see Variants). Create `gateway/config.yaml` before the first `up`: Docker bind-mounts a missing host file as an empty directory, and the gateway exits with a `gateway exited` log record whose `error` is `loading config: controlplane: reading config /config.yaml: read /config.yaml: is a directory`.

## Verify it worked

Liveness. `GET /healthz` needs no auth and no provider:

```bash
curl -s http://127.0.0.1:8080/healthz
```

returns `200` with body `{"status":"ok"}` and `Content-Type: application/json`.

Readiness. `GET /readyz` returns `{"ready":<bool>,"models":{...}}`: `200` when every configured model has a healthy deployment, `503` when any model has none. Health here is the active health-probe loop's verdict, which runs only when `health_probe.interval_seconds` is set (the example config leaves `health_probe:` commented out). With it unset every deployment reads healthy and a fresh start returns `200` before any provider has been contacted; set `health_probe.interval_seconds` if you want `/readyz` to reflect upstream reachability.

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/readyz
```

Container state and logs: `docker compose ps` lists the running services; the gateway logs JSON to stdout, starting with one `build_info` record (`version`, `commit`, `date`, `go_version`, `platform`).

## What the Compose file sets for you

- Host port `127.0.0.1:8080` only (loopback, not `0.0.0.0`). Change the mapping to `"8080:8080"` in a local override (using `ports: !override`, as in [Change the host port](#change-the-host-port), so the loopback mapping is replaced rather than merged) if another device must reach it.
- `stop_grace_period: 60s` on `gateway` and `gateway2`, added to `docker-compose.yml` on 2026-10-08; `gateway/v0.17.0` and earlier checkouts lack it and get Compose's default, so add it in your `docker-compose.override.yml` there. The process drains for up to 30 s + 15 s + 5 s = 50 s after SIGTERM (`cmd/gateway/main.go`); Compose's default 10 s would kill it mid-drain.
- `OTEL_EXPORTER_OTLP_INSECURE=true` in the environment. A no-op under the default `telemetry.exporter: "stdout"`; required once you point `exporter: "otlp"` at the plain-HTTP `observability` receiver.
- `./gateway/config.yaml` mounted read-only at `/config.yaml`. This is the service's only volume, and the scratch image has no directory UID 65532 can write (it copies in only the CA bundle and `/gateway`, both root-owned), so a `budget.persist_path`, `admin.persist_path` or `prompt.persist_path` pointing inside the container fails at startup (`opening … store at …: … permission denied` or `… no such file or directory`) rather than silently losing data. Mount a volume UID 65532 can write (on Linux, a bind-mounted host directory you `chown 65532`) and point `persist_path` into it, or use Redis for state that must survive.
- No `depends_on`: a `config.yaml` without any `redis_addr` runs fully in memory.
- The image is `FROM scratch`: `/gateway`, the CA bundle, no shell. It runs as UID/GID `65532:65532`, `ENTRYPOINT ["/gateway"]`, `CMD ["-config", "/config.yaml"]`.
- Inside a cgroup memory limit the process sets `GOMEMLIMIT` to 90% of it; `AUTOMEMLIMIT=off` skips the probe.

## Variants

### Add Redis

Redis is used only if `config.yaml` sets one or more of `rate_limit.redis_addr`, `budget.redis_addr`, `admin.redis_addr`, `config_propagation.redis_addr`. Each is independently optional; unset means in-memory (or bbolt via `persist_path`). `config_propagation.redis_addr` also requires `config_propagation.signing_secret_env`. `admin.redis_addr` opens the Redis-backed virtual-key store and loads its keys at startup whether or not the admin API is enabled, and it is the one fail-closed knob: if Redis is unreachable the gateway logs `hydrating virtual keys from redis at …` and exits 1 (about 100 s later for a black-holed address) instead of starting, whereas `rate_limit`, `budget` and `config_propagation` dial lazily and the process starts. The writes to it come from the admin API, which needs `admin.token_env`.

```yaml
# gateway/config.yaml, any subset:
rate_limit:
  redis_addr: "redis:6379"
budget:
  redis_addr: "redis:6379"
```

```bash
docker compose --profile redis up
```

The `redis` service is `redis:7-alpine` with `--appendonly yes --appendfsync everysec` and a named `redis-data` volume, which survives container recreation. It publishes no host port on purpose: it has no AUTH, and the gateway reaches it at `redis:6379` on the Compose network. The `redis_password_env`, `redis_username` and `redis_tls` knobs exist in the config, but this dev service does not use them. See [Redis in FAILURE-MODES.md](../../operations/FAILURE-MODES.md) for what the gateway does while Redis is unreachable.

Verify: nothing dials Redis at startup and no startup log confirms the connection, so send one chat request through the gateway, then run `docker compose exec redis redis-cli --scan --pattern 'ratelimit:*'` (for `rate_limit.redis_addr`; use `'budget:*'` for `budget.redis_addr`, with a virtual key that has `budget_usd` set). It must list at least one key. If it lists nothing, either `config.yaml` never set that `redis_addr` (the subsystem is in memory) or Redis was unreachable and the request failed open: look for `ratelimit_backend_unavailable` / `budget_backend_unavailable` in the gateway log. `admin.redis_addr` needs no such check, since the gateway loads from it at startup and exits if it cannot (`identity:virtual_keys` is written only by admin mutations), and `config_propagation.redis_addr` is pub/sub only and leaves no key to scan.

### Two gateway instances sharing one Redis

```bash
docker compose --profile multi-instance --profile redis up
# second instance: http://127.0.0.1:8091
```

`gateway2` is a second build of the same image, mounting the same `./gateway/config.yaml`, on host port `127.0.0.1:8091`. Cross-instance behaviour only engages for the subsystems whose `redis_addr` you set.

### OTel backend: Grafana, Prometheus, Tempo, Alertmanager

```yaml
# gateway/config.yaml
telemetry:
  exporter: "otlp"
  otlp_endpoint: "observability:4318"
```

```bash
docker compose --profile observability up
```

`observability` is `grafana/otel-lgtm:0.33.0` on loopback ports 4317, 4318 (the OTLP HTTP endpoint the gateway sends to), 3000 (Grafana), 9090 (Prometheus) and 3200 (Tempo); it mounts the dashboard provisioning, Prometheus config and SLO rules from `docs/operations/grafana/`. `alertmanager` is `prom/alertmanager:v0.28.1` on `127.0.0.1:9093`, under the same profile. Exporter semantics are in [TELEMETRY.md](../../operations/TELEMETRY.md); metric and log names are in [Metrics and logs](../../reference/metrics-and-logs.md).

Verify: after one successful chat request, `curl -s 'http://127.0.0.1:9090/api/v1/label/__name__/values' | grep -c kelvran` should be non-zero (metrics leave the gateway on the SDK's periodic reader schedule, not per request, so allow one export interval), and Grafana at <http://127.0.0.1:3000> shows the provisioned "Kelvran Gateway Overview" dashboard. If nothing arrives, confirm `telemetry.exporter` is `"otlp"`: the default `"stdout"` sends nothing to the collector and raises no error.

### Vector S3 shipper

```bash
docker compose --profile vector-s3 up
```

`vector` is `timberio/vector:0.58.0-debian`. It mounts `docs/operations/vector-gatewayevents-s3-compose.yaml` as its config and the host Docker socket read-only (hard-coded as `/var/run/docker.sock`; on colima or rootless Docker confirm that path resolves on the host with `docker context inspect` before relying on the mount), and reads a separate `.env.vector` (gitignored by the `.env.*` pattern) that must define `KELVRAN_GATEWAYEVENTS_BUCKET`, `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and optionally `AWS_SESSION_TOKEN`. The bucket and a write-only credential are yours to provision, for example with `terraform/vector-s3-shipper/`; nothing in the repository records whether that module has been applied.

### Network fault injection (manual)

```bash
docker compose --profile chaos up
curl -X POST http://127.0.0.1:8474/proxies -d '{"name":"openai","listen":"0.0.0.0:8666","upstream":"api.openai.com:443"}'
# then point one deployment's base_url at http://toxiproxy:8666
```

`toxiproxy` is `ghcr.io/shopify/toxiproxy:2.12.0` on `127.0.0.1:8474`, for manual use; it is not wired into CI.

### Validate a config without starting anything

```bash
docker run --rm -v "$PWD/gateway/config.yaml:/config.yaml:ro" ghcr.io/kelvran/gateway:latest -validate -config /config.yaml
```

prints `config is valid` and exits 0. `-validate` checks that each deployment's provider has an adapter and that fallback-chain targets exist; it does not resolve environment variables.

### Which build is running

```bash
docker run --rm ghcr.io/kelvran/gateway:<tag> -version
# kelvran-gateway <version> (<commit>, built <date>, go<ver>, linux/<arch>)
```

A locally built image (`docker compose build gateway`) reports `kelvran-gateway dev (none, built unknown, ...)`: only CI passes real `VERSION`/`COMMIT`/`DATE` build-args. Published tags are `:latest` and `:sha-<commit>` on every push to `main`, plus `:v<X.Y.Z>` for a `gateway/v<X.Y.Z>` tag. Images built on or after 2026-10-08 are `linux/amd64` + `linux/arm64`; `gateway/v0.17.0` and earlier are `linux/amd64` only. See [Container image](../../reference/container-image.md).

### Inspect the resolved Compose config without leaking secrets

```bash
make config-safe
```

This pipes `docker compose config` through a filter that redacts values whose variable name contains `KEY`, `SECRET`, `TOKEN`, `PASSWORD`, `PASS`, `CREDENTIAL`, `DSN`, `URI`, `URL` or `CONNECTION`, and any `user:pass@host` substring. Do not run the bare `docker compose config`: it prints every interpolated secret in cleartext.

### Change the host port

Put a `docker-compose.override.yml` next to the Compose file; it is gitignored and local to your machine. Use `!override` so the `ports` list is replaced instead of merged with the base file's `8080` mapping:

```yaml
services:
  gateway:
    ports: !override
      - "127.0.0.1:8090:8080"
```

### Reach the admin API

The admin server starts only when `admin.token_env` is set, on `admin.listen_addr` or the default `127.0.0.1:8081` inside the container. Compose publishes only 8080, so the admin API is not reachable from the host unless you set `admin.listen_addr` to a non-loopback address and add a port mapping in your override. See [Admin API and RBAC](../admin-api-rbac.md) and [Admin API reference](../../reference/admin-api.md).

## Not available today

- No `evals` Compose service and no evals Dockerfile. `evals` is a CLI: `cd evals && uv run evals --help`.
- No `postgres` service and no ClickHouse sink. Both are future targets only.
- No `vector-gcs` profile; the name is reserved in a comment only.
- No Compose `healthcheck:` on `gateway` (the image has no curl or wget). Check `/healthz` from the host.
- No `restart:` policy on any service.
- No Redis AUTH or TLS on the Compose `redis` service, and no production Redis (MemoryDB, ElastiCache) provisioned by any module in the repository.
- No volume for `persist_path` files on the `gateway` service.

## Related

- [Deployment guide](../../operations/DEPLOY.md) and [`docker-compose.yml`](../../../docker-compose.yml)
- [Virtual keys and budgets](../virtual-keys-and-budgets.md), [Backup and restore](../backup-and-restore.md), [Troubleshooting](../troubleshooting.md)
- [Quickstart](../../tutorials/quickstart.md) for the non-Compose first run
- [SECURITY.md](../../../SECURITY.md) for the never-commit-secrets rule that `.env` and `config.yaml` follow
