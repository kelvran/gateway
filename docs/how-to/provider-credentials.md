# Configure provider credentials

This page shows an operator how to give each `deployments:` entry in `config.yaml` the upstream credential it needs, for each of the five providers the gateway accepts, and how to choose between environment-variable and file-based (hot-reloadable) credential sources. It is for whoever owns the gateway's config file and its secrets delivery (shell, systemd, Compose, Kubernetes, ECS).

Use this when you are adding a deployment for a new provider, moving a credential from an environment variable to a mounted file, or pointing a deployment at a self-hosted backend with its own TLS setup.

## Prerequisites

- A gateway binary or image and a `config.yaml` with at least one virtual key. See [the quickstart](../tutorials/quickstart.md).
- The provider credential itself, held outside the repository. Config holds only environment-variable names (`*_env`) or file paths (`*_file`); a secret value never belongs in `config.yaml`. See [SECURITY.md](../../SECURITY.md) and [the security model](../explanation/security-model.md).
- For Bedrock: an AWS access key ID and secret access key (plus a session token if the pair is STS-issued) and the signing region.

## How a credential reaches a running gateway

- `*_env` keys name an environment variable. The gateway reads it once at startup with `os.Getenv`. If it is unset or empty, startup continues and the log carries a `WARN` such as `deployment's upstream API key env var is not set; calls to this deployment will fail`. The value cannot change without a restart. The one exception is `session_token_env`: an unset session-token variable is not warned about at all; the gateway signs without a token, and an STS key pair is then rejected upstream with 401 or 403 (row C3). Check that variable yourself before starting.
- `*_file` keys name a file path. The gateway reads the file at startup, trims leading and trailing whitespace, and re-reads it on a timer for the life of the process (default every 60 s, set by `credential_reload.interval_seconds`; a value of 0 or less also means 60, not disabled). A changed value is swapped in atomically and logged as `credential_reload_rotated` with the deployment and field name only. A read error keeps the last-known-good value and logs `credential_reload_read_failed`.
- An empty or whitespace-only file counts as a read failure, never as the credential `""`. This rule first shipped in gateway/v0.18.0.
- When a deployment sets both a `*_file` key and its `*_env` counterpart, the file wins and the environment variable is never consulted for that credential.

## Steps

### 1. Pick the provider and its credential keys

Exactly five `provider:` values exist. Any other value fails at startup and under `-validate` with `no adapter registered for provider`.

| `provider:` | Credential keys (one of each pair) | Also required | What the gateway sends upstream |
|---|---|---|---|
| `openai` | `api_key_env` or `api_key_file` | | `Authorization: Bearer <key>` and an `Idempotency-Key` (SHA-256 of the body) |
| `anthropic` | `api_key_env` or `api_key_file` | | `x-api-key: <key>` and `anthropic-version: 2023-06-01` |
| `gemini` | `api_key_env` or `api_key_file` | `base_url` ends in `:generateContent` (streaming only; checked when a stream request is made, not at load) | `x-goog-api-key: <key>` |
| `bedrock` | `access_key_id_env` or `access_key_id_file`; `secret_access_key_env` or `secret_access_key_file`; optional `session_token_env` or `session_token_file` | `region` (load-checked); `base_url` ends in `/converse` (streaming only) | An AWS SigV4 signature (service `bedrock`, signing region = `region`) |
| `openaicompat` | `api_key_env` or `api_key_file` | `allow_insecure_http: true` for plain `http://` | `Authorization: Bearer <key>` |

`region` is a plain, non-secret value, not an environment-variable name. It is the SigV4 signing region and also what a virtual key's `allowed_regions` is matched against (see [virtual keys and budgets](virtual-keys-and-budgets.md)).

### 2. Set `base_url` to the full endpoint

`base_url` is the complete URL the gateway POSTs to. Buffered chat and embedding calls use it verbatim; nothing is appended. Each deployment must set `model`, `provider`, `upstream_model` and `base_url`, or loading fails with `is missing one of model/provider/upstream_model/base_url`.

- The scheme must be `https`. A plain `http://` URL fails to load with `is not https -- set allow_insecure_http: true if this is a deliberate localhost/dev endpoint` unless that deployment sets `allow_insecure_http: true`.
- For streaming, Gemini's path must end in `:generateContent` (the gateway derives `:streamGenerateContent?alt=sse`) and Bedrock's must end in `/converse` (the gateway derives `/converse-stream`). Other providers stream to `base_url` unchanged. These suffixes are checked only when a `stream: true` request is made (so the first streaming request surfaces the error), not at load and not by `-validate`. See [streaming](streaming.md).

### 3. Name the credential source in `config.yaml`

Environment-variable source, OpenAI:

```yaml
deployments:
  gpt4o-primary:
    model: "gpt-4o"
    provider: "openai"
    upstream_model: "gpt-4o"
    base_url: "https://api.openai.com/v1/chat/completions"
    api_key_env: "OPENAI_API_KEY"
```

Anthropic and Gemini use the same shape with their own `base_url` and key name:

```yaml
deployments:
  claude-opus-primary:
    model: "claude-opus-4"
    provider: "anthropic"
    upstream_model: "claude-opus-4-20250514"
    base_url: "https://api.anthropic.com/v1/messages"
    api_key_env: "ANTHROPIC_API_KEY"
  gemini-flash-primary:
    model: "gemini-2.5-flash"
    provider: "gemini"
    upstream_model: "gemini-2.5-flash"
    base_url: "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"
    api_key_env: "GEMINI_API_KEY"
```

Bedrock needs the SigV4 pair and `region`; the session token is optional and only matters for STS or other temporary credentials:

```yaml
deployments:
  claude-bedrock-primary:
    model: "claude-bedrock"
    provider: "bedrock"
    upstream_model: "anthropic.claude-3-5-sonnet-20241022-v2:0"
    base_url: "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20241022-v2:0/converse"
    access_key_id_env: "AWS_ACCESS_KEY_ID"
    secret_access_key_env: "AWS_SECRET_ACCESS_KEY"
    session_token_env: "AWS_SESSION_TOKEN"   # optional; STS/temporary credentials only
    region: "us-east-1"
```

A Bedrock deployment missing either key or `region` fails to load with `(provider bedrock) is missing one of access_key_id_env/secret_access_key_env/region`. Any other provider missing both `api_key_env` and `api_key_file` fails with `is missing api_key_env`.

### 4. Make the value available to the process

- Shell or source build: `export OPENAI_API_KEY="<your provider key>"` before starting the gateway.
- Docker Compose: the `gateway` service reads `env_file: .env`; `.env.example` lists the names and is the template. See [the Compose guide](deploy/docker-compose.md) and [docker-compose.yml](../../docker-compose.yml).
- systemd package: `EnvironmentFile=-/etc/kelvran-gateway/env`, one `KEY=value` per line. See [the systemd guide](deploy/systemd-package.md) and [the unit file](../../deploy/systemd/kelvran-gateway.service).
- Kubernetes: the base manifest injects `envFrom: secretRef: gateway-upstream-credentials`; the EKS overlay fills that Secret from AWS Secrets Manager with the keys `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`. See [the Kubernetes guide](deploy/kubernetes-kustomize.md) and [deploy/k8s/README.md](../../deploy/k8s/README.md).
- ECS/Fargate: the task definition's `secrets` block. See [the ECS guide](deploy/ecs-fargate.md).

### 5. Validate, then start

```bash
gateway -validate -config config.yaml
```

This prints `config is valid` and exits 0. It checks shape and provider names only. It never reads an environment variable, opens a credential file, dials a provider or opens a store, so a missing or wrong secret is not something it can report.

## Variants

### File-based credentials with hot reload

Point each credential at a file the gateway can read, for example a Kubernetes projected Secret volume, and the gateway picks up a rewritten file without a restart:

```yaml
deployments:
  claude-bedrock-primary:
    model: "claude-bedrock"
    provider: "bedrock"
    upstream_model: "anthropic.claude-3-5-sonnet-20241022-v2:0"
    base_url: "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20241022-v2:0/converse"
    access_key_id_file: "/var/run/secrets/kelvran/access-key-id"
    secret_access_key_file: "/var/run/secrets/kelvran/secret-access-key"
    session_token_file: "/var/run/secrets/kelvran/session-token"   # optional
    region: "us-east-1"

credential_reload:
  interval_seconds: 60   # default when omitted; <= 0 also means 60
```

`api_key_file` works the same way for `openai`, `anthropic`, `gemini` and `openaicompat`. Only deployments with at least one `*_file` key join the reload loop. An unreadable or empty file at startup logs `credential file could not be read or is empty; calls will fail` (on gateway/v0.17.0 the message is `credential file could not be read; calls will fail` and an empty file is not warned about at all), and the deployment starts with no credential until a later tick reads one. To stop sending a session token, remove `session_token_file` and restart; an emptied file is treated as a read failure and the old token stays in use. Rotation procedures are on [rotate credentials](rotate-credentials.md).

### Self-hosted OpenAI-compatible backend

Plain HTTP needs an explicit opt-in. A private CA or mutual TLS gets its own per-deployment transport through a `tls:` block; the PEM files are read once at startup.

```yaml
deployments:
  vllm-local:
    model: "llama-local"
    provider: "openaicompat"
    upstream_model: "meta-llama/Llama-3.1-8B-Instruct"
    base_url: "http://10.0.0.5:8000/v1/chat/completions"
    allow_insecure_http: true
    api_key_env: "VLLM_API_KEY"     # required even if the backend ignores it
  vllm-internal-ca:
    model: "llama-local"
    provider: "openaicompat"
    upstream_model: "meta-llama/Llama-3.1-8B-Instruct"
    base_url: "https://vllm.internal:8443/v1/chat/completions"
    api_key_env: "VLLM_API_KEY"
    tls:
      ca_cert_path: "/etc/kelvran/internal-ca.pem"
      client_cert_path: "/etc/kelvran/gateway-client.pem"   # both or neither
      client_key_path: "/etc/kelvran/gateway-client-key.pem"
```

`tls.client_cert_path` and `tls.client_key_path` must be set together or not at all. A `tls:` block that sets none of `ca_cert_path`, `client_cert_path` and `client_key_path` is a load error.

### Embedding deployments

`kind: embedding` is accepted only when `provider` is `openai` or `bedrock`; other providers are rejected at load. Every embedding deployment that shares one `model` must use the same `provider` and `upstream_model`. The credential keys are the same as for chat. See the [data-plane API reference](../reference/data-plane-api.md).

### One credential shared by several tenants

When several tenants' virtual keys resolve to the same deployment and therefore the same provider credential, set `shared_across_tenants: true` on that deployment. On `anthropic` and `bedrock` deployments this turns provider-side `cache_control` auto-population off; `disable_cache_control_auto_populate: true` does the same directly. Other providers ignore both keys. See [the cache gate explanation](../explanation/cache-gate.md) and [caching](caching.md).

### Guardrail detectors

The optional `guardrails.bedrock_guardrails` and `guardrails.embed_sim` sections take the same `region`, `access_key_id_env`/`secret_access_key_env`/`session_token_env` keys and the same `*_file` alternatives, and reuse the single `credential_reload.interval_seconds` value. Both are annotated in [gateway/config.example.yaml](../../gateway/config.example.yaml). One difference from deployments: `embed_sim` embeds its detection corpus through Bedrock while the gateway is starting, so an unreadable file, an unset variable or an expired credential there is a fatal startup error (`constructing embedsim detector`), not a warning. Hot reload covers rotation of a process that is already running. Their rotation events are `bedrockguard_credential_reload_rotated` / `embedsim_credential_reload_rotated` and the file warning's `subsystem` is `guardrails.bedrock_guardrails` or `guardrails.embed_sim`.

## Verify it worked

1. Run `gateway -validate -config config.yaml`. Expected: `config is valid`, exit code 0. Any failure prints one line starting `config error:` and exits 1; a deployment-level failure names the deployment.
2. Start the gateway and read the first log lines. Expected: the line `gateway listening` whose `addr` field is your `listen_addr` value, and no `WARN` containing `env var is not set` or `credential file could not be read`. The env-var warning carries `deployment` and `env_var`; the file warning carries `subsystem` (the deployment name, or `guardrails.bedrock_guardrails` / `guardrails.embed_sim`), `field` and `path`.
3. Send one request through the deployment. With `gateway/config.example.yaml`'s `team-alpha` key the bearer is the public example secret `example-team-alpha-secret-do-not-use`; with your own key use its raw secret:

   ```bash
   curl -s http://localhost:8080/v1/chat/completions \
     -H "Authorization: Bearer $KELVRAN_KEY" -H "Content-Type: application/json" \
     -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Say hello in five words."}]}'
   ```

   Expected: HTTP 200 with an OpenAI-shaped body (`id`, `model`, `choices[0].message.content`, `usage`). A `502` whose `error.message` is `upstream provider returned status 401` (or `403`) means the virtual key was accepted and the request reached the provider, which rejected the credential; the provider's own error text is only in the gateway's `chat_completion` log line. Row C3 of [FAILURE-MODES.md](../operations/FAILURE-MODES.md) and the [error codes reference](../reference/error-codes.md) cover the full mapping.
4. For a `*_file` deployment, rewrite the file with a new value and wait one `credential_reload.interval_seconds`. Expected: an `INFO` line `credential_reload_rotated` with `deployment` and `field` (never the value). A `WARN` `credential_reload_read_failed` means the file was missing or unreadable (or, since gateway/v0.18.0, empty) and the previous value is still in use.

## Not available today

- AWS role assumption, IRSA, web identity or an instance-profile credential chain for `bedrock` deployments. Only a static access key pair (plus optional session token) from `*_env` or `*_file`.
- Vertex AI or a GCP service account for `gemini`. Only a Generative Language API key sent as `x-goog-api-key`.
- Hot reload of `*_env` credentials. A rotated environment value needs a restart; switch to `*_file` to avoid that.
- Any load-time or `-validate` check of the Gemini `:generateContent` or Bedrock `/converse` suffix. The first streaming request reports it.
- A "no auth" mode for `openaicompat`. `api_key_env` or `api_key_file` is mandatory; an unset value only warns at startup and the gateway then sends `Authorization: Bearer ` with an empty token.
- Providers beyond the five above (no Azure OpenAI, Cohere or Mistral adapter).
- A custom CA or client certificate for the Redis connection; Redis has only a TLS on/off toggle using the system trust store.

## Reference

- Every key on this page: [configuration reference](../reference/config.md) and the annotated [gateway/config.example.yaml](../../gateway/config.example.yaml).
- Environment variables the process reads: [DEPLOY.md](../operations/DEPLOY.md) and the repository's `.env.example`.
- Failover between deployments once credentials work: [routing and failover](routing-and-failover.md).
- Why Bedrock and Gemini use these shapes: [docs/rfcs/2026-09-04-bedrock-adapter.md](../rfcs/2026-09-04-bedrock-adapter.md), [docs/rfcs/2026-09-04-gemini-adapter.md](../rfcs/2026-09-04-gemini-adapter.md).
- Threats these choices address: [THREAT_MODEL.md](../../THREAT_MODEL.md).
