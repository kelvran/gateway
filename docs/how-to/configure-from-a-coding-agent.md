# Configure the gateway from a coding agent

This page is for a developer who drives a coding agent (Claude Code, Codex, Cursor, Aider or similar) and wants the agent to write, validate and run a Kelvran gateway `config.yaml` without guessing. It names the files the agent should read, the fast validate loop and its exit codes, the health endpoints and what they do and do not mean, how credentials reach the process, the Redis keys for more than one replica, and the mistakes agents make most often.

**Use this when** an agent is producing or editing a gateway configuration and you want a loop it can run on its own: write, validate, start, probe, read the error envelope, fix.

## Prerequisites

- A gateway binary (`go install`, a source build or the image `ghcr.io/kelvran/gateway`). See the [quickstart](../tutorials/quickstart.md).
- One provider credential, exported in the shell or written to a file. The config never holds it. See [Provider credentials](provider-credentials.md).
- `curl` on the machine the agent runs on; `openssl` (or any SHA-256 tool) only for the by-hand key recipe below — `kelvran init` (since `gateway/v0.18.0`) needs neither.

## Steps

### 1. Point the agent at the sources of truth

Have the agent read these, in this order, before it writes a key (`docs/llms.txt` is the index that links all of them):

1. [`gateway/config.example.yaml`](../../gateway/config.example.yaml): every key, commented. It is the annotated reference ([`docs/VERSIONING.md`](../VERSIONING.md) names it as such) alongside the hand-written [config reference](../reference/config.md).
2. `gateway/internal/gateway/controlplane/config.go`: the parser. `Load` is where a key becomes real; a key that no `getString`/`getInt`/`getFloat`/`getDecimal`/`getBool`/`getMap` call reads is ignored.
3. [`docs/operations/FAILURE-MODES.md`](../operations/FAILURE-MODES.md): what refuses to start and what `-validate` cannot see.
4. [`docs/reference/container-image.md`](../reference/container-image.md) and [`docs/operations/DEPLOY.md`](../operations/DEPLOY.md) for the container and deployment contract.

The repository is <https://github.com/kelvran/gateway>. Do not let the agent copy keys from any other project's gateway config; the names differ.

### 2. Write a minimal `config.yaml`

The smallest file that loads has `listen_addr`, one virtual key with a `key_hash`, and one deployment with `model`, `provider`, `upstream_model`, `base_url` and a credential reference. Everything else is optional.

```yaml
listen_addr: ":8080"
telemetry:
  exporter: "none"                  # default "stdout" prints every span and a metrics dump every 60 s
virtual_keys:
  dev:
    # sha256 of "example-team-alpha-secret-do-not-use" (the public example from config.example.yaml)
    key_hash: "6701a1ecc6b08958fa24e13f267aac7233d47f390e92e71f8cc8fb3144672cf1"
    budget_usd: 25.0
deployments:
  gpt4o-primary:
    model: "gpt-4o"
    provider: "openai"
    upstream_model: "gpt-4o"
    base_url: "https://api.openai.com/v1/chat/completions"
    api_key_env: "OPENAI_API_KEY"   # the NAME of the variable, never its value
price_table:
  gpt-4o:
    prompt_per_token: 0.0000025
    completion_per_token: 0.00001
```

Rules the agent must follow when it writes this file:

- **Format is a YAML subset, not YAML.** `key: value` scalars and 2-space-indented mappings only; `#` starts a comment anywhere on the line, even inside quotes, so no value may contain `#`. A line without a `key:` shape (for example a `- item` list entry) fails with `expected "key: value" or "key:"`; a tab in the indentation and a duplicate key at the same level also fail. Numbers stay as text until a field parses them, so write money as plain decimals.
- **Unknown keys are silently ignored** at every level, with two exceptions that are load errors: an unknown field under a `models:` entry, and an unknown error-class key under a deployment's `fallback_chains:` (only `generic`, `content_policy`, `context_window_exceeded` are accepted). A misspelled key loads fine and does nothing; `-validate` does not catch it. Have the agent diff its keys against `config.example.yaml` before trusting a green validate.
- **`base_url` must be `https://`** unless the deployment sets `allow_insecure_http: true`.
- **Secrets never go in the file.** Only env var names (`*_env`), file paths (`*_file`) and SHA-256 key hashes. `config.yaml` and `.env` are gitignored; `gateway/config.example.yaml` is the only committed config.
- `kelvran keys create <name> --config config.yaml` (since `gateway/v0.18.0`, with no admin token in the environment) generates a secret and writes its hash into the file; by hand, a real key hash comes from `printf '%s' "$KELVRAN_KEY" | shasum -a 256 | cut -d' ' -f1` (`sha256sum` on Linux). Clients send the raw secret as `Authorization: Bearer <secret>`, never the hash. See [Virtual keys and budgets](virtual-keys-and-budgets.md).

### 3. Run the fast validate loop

```bash
gateway -validate -config config.yaml
echo "exit=$?"
```

- Exit 0 prints `config is valid` on stdout.
- Exit 1 prints `config error: <reason>` on stderr. The reason names the key or deployment, for example `config error: controlplane: deployment "gpt4o-primary" is missing api_key_env`; every reason from the parser carries the `controlplane: ` prefix, the adapter check's `deployment "<name>": no adapter registered for provider "<provider>"` and `deployment "<name>" fallback_chains.<class> names "<target>", which is not a configured deployment` — the two checks `validateConfig` runs after `Load` — do not.
- `-config` defaults to `config.yaml` in the current directory. Flag precedence is `-version` (reads no config), then `-restore-store`, then `-validate`, then a normal start.

What `-validate` checks: file shape, required keys, per-deployment credential presence, the `https` rule, the `models:` section, that every `provider` has a registered adapter, and that every `fallback_chains` target names a configured deployment.

What it does not check: it reads no environment variable, opens no store, credential file or audit log path, dials no Redis, and does not inspect the Gemini `:generateContent` or Bedrock `/converse` `base_url` suffixes. A valid config can still refuse to start (an admin token env that resolves empty) or fail on the first request (an unset provider key, a wrong suffix). Treat exit 0 as "the shape is right", nothing more.

### 4. Supply credentials out of band

Two mechanisms, chosen per deployment:

| Key | Read when | Rotation | Missing value |
|---|---|---|---|
| `api_key_env`, `access_key_id_env`, `secret_access_key_env`, `session_token_env` | once, at startup | restart only | WARN at start, the gateway still starts: `deployment's upstream API key env var is not set; calls to this deployment will fail` for `api_key_env`; `deployment's AWS access key ID env var is not set; ...` and `deployment's AWS secret access key env var is not set; ...` for the two Bedrock keys; an unset `session_token_env` is silent (the token is optional) |
| `api_key_file`, `access_key_id_file`, `secret_access_key_file`, `session_token_file` | at startup, then re-read every `credential_reload.interval_seconds` (default 60) | picked up without a restart | logged as a warning per file |

When both are set for one credential the file wins. Bedrock deployments need `access_key_id_*`, `secret_access_key_*` and `region`; every other provider needs `api_key_*`. Details in [Provider credentials](provider-credentials.md) and [Rotate credentials](rotate-credentials.md).

An agent that runs inside Docker Compose must never print the resolved environment with `docker compose config`; `make config-safe` prints the same structure with secret-looking values redacted. This rule and its history are in [`AGENTS.md`](../../AGENTS.md).

### 5. Start and watch for two log lines

```bash
export OPENAI_API_KEY="<your provider key>"
gateway -config config.yaml
```

Logs are JSON on stdout. A successful start logs `build_info` and then `gateway listening` with `addr`. With the admin API on it also logs `admin server listening` with `addr` and `mtls`. Anything that refuses to start exits 1 before the listener binds; the list is in [`docs/operations/FAILURE-MODES.md`](../operations/FAILURE-MODES.md) under "What refuses to start".

### 6. Smoke-test and read the envelope

```bash
KELVRAN_KEY="example-team-alpha-secret-do-not-use"   # matches the key_hash above; replace for anything real
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $KELVRAN_KEY" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"ping"}]}'
```

Every error is `{"error":{"message","type","param","code"}}` with `Content-Type: application/json`, so an agent can branch on `type` and `code`. The envelope ships since gateway/v0.18.0; gateway/v0.17.0 and earlier return the same status codes and the same message text for every row below (the full exception list is in [Error codes](../reference/error-codes.md)) as a `text/plain` body with no `type` or `code`, so against the `v0.17.0` image branch on the status and the message text only:

| Status | `type` / `code` | Meaning for the agent |
|---|---|---|
| 200 | OpenAI-shaped body with `choices[0].message.content` | the config works end to end |
| 401 | `authentication_error`, `code` `invalid_api_key` or absent | header missing, or its SHA-256 matches no `key_hash` |
| 400 | `invalid_request_error` / `model_not_found` | no deployment has this `model:` |
| 429 | `rate_limit_error` / `rate_limit_exceeded` or `concurrency_limit_exceeded` (`Retry-After` set) | the key's limits; see [Virtual keys and budgets](virtual-keys-and-budgets.md) |
| 429 | `insufficient_quota` (no `Retry-After`) | `budget_usd` spent |
| 502 | `server_error` / `upstream_error`, message `upstream provider returned status N` | the request reached the provider; `N=401` is almost always an unset or wrong provider key |
| 502 | `server_error` / `upstream_error`, message `upstream call failed for model "<model>"` | the request never reached the provider: wrong `base_url` host, refused connection or timeout; the dial error is in the `chat_completion` log line. Message text since gateway/v0.18.0; gateway/v0.17.0 and earlier return the raw transport error |

The full list is in [Error codes](../reference/error-codes.md). The provider's own error text is only in the `chat_completion` log line, never in the response.

`GET /v1/models` with the same bearer lists the canonical models the key may use, each with `kind` (`chat` or `embedding`); it is a cheap way for an agent to confirm the deployments it wrote. First shipped in gateway/v0.18.0.

## Variants

### Validate inside the published image

The image is `FROM scratch` (no shell), `ENTRYPOINT ["/gateway"]`, `CMD ["-config", "/config.yaml"]`, runs as `65532:65532` and listens on 8080. Extra arguments replace `CMD`, so validation needs no port and no secret:

```bash
docker run --rm -v "$PWD/config.yaml:/config.yaml:ro" ghcr.io/kelvran/gateway:v0.18.0 -validate -config /config.yaml
docker run --rm ghcr.io/kelvran/gateway:v0.18.0 -version
```

Create `config.yaml` before the first run: Docker bind-mounts a missing host path as an empty directory and the gateway exits with `read /config.yaml: is a directory`.

### Enable the admin API

```yaml
admin:
  listen_addr: "127.0.0.1:8081"      # the default when token_env is set; loopback only
  token_env: "KELVRAN_ADMIN_TOKEN"
  viewer_token_env: "KELVRAN_ADMIN_VIEWER_TOKEN"
```

The admin server exists only when `admin.token_env` is set. Every named variable (`token_env`, and `viewer_token_env`, `cost_viewer_token_env`, `operator_token_env` if present) must resolve to a non-empty value or the gateway refuses to start. Auth is `Authorization: Bearer <token>`; failures are plain-text 401. `GET /admin/config` accepts the admin or viewer token. Routes and tiers: [Admin API](../reference/admin-api.md) and [Admin API RBAC](admin-api-rbac.md).

```bash
export KELVRAN_ADMIN_TOKEN=$(openssl rand -hex 32)
export KELVRAN_ADMIN_VIEWER_TOKEN=$(openssl rand -hex 32)
curl -s -H "Authorization: Bearer $KELVRAN_ADMIN_VIEWER_TOKEN" http://127.0.0.1:8081/admin/config
```

### Two or more replicas

Four independent sections each take a `redis_addr`, plus optional `redis_password_env`, `redis_username` and `redis_tls`:

```yaml
budget:
  redis_addr: "redis:6379"           # cross-replica spend enforcement
  redis_password_env: "REDIS_PASSWORD"
admin:
  token_env: "KELVRAN_ADMIN_TOKEN"
  redis_addr: "redis:6379"           # shared virtual-key store, loaded at start
  redis_password_env: "REDIS_PASSWORD"
rate_limit:
  redis_addr: "redis:6379"           # distributed per-key RPM/TPM
  redis_password_env: "REDIS_PASSWORD"
config_propagation:
  redis_addr: "redis:6379"           # live admin mutations reach other replicas
  signing_secret_env: "KELVRAN_CONFIG_PROPAGATION_SIGNING_SECRET"   # required; missing or empty refuses to start
  redis_password_env: "REDIS_PASSWORD"
```

Setting `redis_addr` next to the matching `persist_path` makes Redis win and logs a warning. Per-key concurrency, deployment ceilings, the response cache, prompts and idempotency stay per replica whatever you set. Why all four matter: [`deploy/k8s/README.md`](../../deploy/k8s/README.md).

### Turn on readiness probing

```yaml
health_probe:
  interval_seconds: 30               # unset or 0 (the default) disables probing
  # unhealthy_threshold: 3   healthy_threshold: 2   recovery_ramp_steps: 4   recovery_ramp_initial_percent: 20
```

The first probe runs one interval after start. See [Routing and failover](routing-and-failover.md).

## Verify it worked

```bash
gateway -validate -config config.yaml; echo "exit=$?"
```

prints `config is valid` and `exit=0`. With the gateway running:

```bash
curl -s http://127.0.0.1:8080/healthz
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/readyz
```

`/healthz` returns `200` with `{"status":"ok"}` whenever the process serves; it is `GET` only, needs no auth and checks nothing upstream. `/readyz` returns `{"ready":<bool>,"models":{"<model>":<bool>}}` with `200`, or `503` if any configured model has no deployment the probe loop considers healthy. It reads probe state and makes no network call.

Read `/readyz` with care:

- With `health_probe.interval_seconds` unset or 0, nothing is probed, every deployment counts as healthy, and `/readyz` is `200` for any config. A `200` here says nothing about provider reachability unless probing is on.
- In gateway/v0.17.0 with probing on, a `kind: embedding` deployment is probed with a chat request and reads unhealthy, so `/readyz` is `503` for a working embedding deployment. Embedding-shaped probes first shipped in gateway/v0.18.0.
- A `503` does not stop traffic: the router fails open to the last candidate.
- Keep Kubernetes liveness and readiness probes on `/healthz`; point external monitors at `/readyz` ([`docs/operations/DEPLOY.md`](../operations/DEPLOY.md) explains why).

## Pitfalls agents fall into

- **Image tag prefix.** The git tag is `gateway/v0.18.0`; the image tag is `v0.18.0`. `:latest` and `:sha-<40-hex>` track main and move; pin an index digest for anything deployed. Images at v0.17.0 and earlier are `linux/amd64` only; images from v0.18.0 on are amd64 and arm64 indexes.
- **Vacuous `/readyz`.** See above. An agent that reports "ready: true" as proof of a working provider key is wrong unless probing is on.
- **Silently ignored keys.** A green `-validate` plus a typo gives a gateway that behaves as if the key were absent. Compare every key against `config.example.yaml`.
- **`-validate` is not a reachability check.** It will not notice an unset env var, an unreadable `*_file`, an unreachable Redis or a wrong Gemini/Bedrock suffix.
- **Noisy default telemetry.** Without a `telemetry:` section that sets `exporter: "none"` (or `exporter: "otlp"` plus `otlp_endpoint`), spans and a metrics dump interleave with the logs on stdout. Write it as two lines, as in step 2; the parser has no `{ }` flow syntax. See [`docs/operations/TELEMETRY.md`](../operations/TELEMETRY.md).
- **Missing config file under Docker** mounts as a directory; see the image variant above.

## Not available today

- No JSON Schema or machine-generated reference for `config.yaml`; the annotated `gateway/config.example.yaml` is the reference.
- No OpenAPI document for `/v1/*` or the admin API.
- No `connect --write` for `codex`, `aider` or `continue` (print-only until their documentation is archived); the `kelvran` CLI itself (`init`, `doctor`, `keys`, `status`, `spend`, `connect`) first shipped in `gateway/v0.18.0` ([reference](../reference/kelvran-cli.md)).
- No exact token counts yet: `POST /v1/messages/count_tokens` answers `404` until the passthrough leg adds the `anthropic` branch (Claude Code's `/context` shows a character-based estimate). `POST /v1/messages` is served since gateway/v0.19.0, so Claude Code's native Anthropic mode can target Kelvran ([how-to](clients/claude-code.md)) and `kelvran connect claude --check` turns green against it (an allowlisted key's `403` is read as a proven credential too). See [Compatibility](../reference/compatibility.md).
- No `-validate` strict mode, unknown-key detection, JSON output, or env-var, file or Redis reachability checks (`kelvran doctor` covers the env-var and file checks, with `--json`).
- No `${VAR}` interpolation inside `config.yaml`; the parser is a literal `key: value` subset.
- No `schema_version` key in `config.yaml`.

## Related

- [Config reference](../reference/config.md), [Data-plane API](../reference/data-plane-api.md), [Metrics and logs](../reference/metrics-and-logs.md)
- [Troubleshooting](troubleshooting.md), [Docker Compose](deploy/docker-compose.md), [curl client](clients/curl.md)
