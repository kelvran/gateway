# Rotate credentials

This page shows an operator how to replace every secret the gateway holds: upstream provider credentials (API keys, AWS access keys, STS session tokens), the AWS keys behind Bedrock Guardrails and embed-sim, client virtual keys, admin tokens, Redis passwords, signing secrets and TLS material. It says which rotations a running gateway picks up on its own, which need a restart, and what each one looks like in the logs. It is for whoever owns `config.yaml` and the secrets delivery around it (shell, systemd, Compose, Kubernetes, ECS).

Use this when a credential has expired, was exposed, or is due for scheduled replacement.

## Prerequisites

- gateway/v0.16.0 or later. The file-based hot-reload path for deployments and for `guardrails.bedrock_guardrails` / `guardrails.embed_sim` shipped in that release ([gateway/changelog/0.16.0.md](../../gateway/changelog/0.16.0.md)). Two behaviours on this page are on main only and in no tagged release yet: the empty-file handling noted under step 3, and the `GET /v1/models` check under Verify it worked; each is marked where it appears.
- Access to wherever the process gets its secrets: the host shell, the Kubernetes namespace, the Compose `.env`, or the ECS task definition.
- For a virtual-key rotation: the admin listener address and a token for the Admin or Operator tier. See [Admin API and RBAC](admin-api-rbac.md).
- Know how each credential reaches the gateway today, `*_env` or `*_file`. The config file tells you; the table below tells you what that means.

## What reloads live and what needs a restart

| Credential | Config key | Picked up without a restart? |
|---|---|---|
| Deployment credential from a file | `api_key_file`, `access_key_id_file`, `secret_access_key_file`, `session_token_file` | Yes. Polled every `credential_reload.interval_seconds` (default 60 s) |
| Deployment credential from the environment | `api_key_env`, `access_key_id_env`, `secret_access_key_env`, `session_token_env` | No. Read once at startup |
| Bedrock Guardrails / embed-sim AWS keys from a file | `guardrails.bedrock_guardrails.*_file`, `guardrails.embed_sim.*_file` | Yes. Same interval as above |
| Bedrock Guardrails / embed-sim AWS keys from the environment | the `*_env` siblings | No |
| Client virtual key | `virtual_keys.<name>.key_hash` | Yes. `POST /admin/virtual_keys/{name}/rotate` |
| Admin tokens | `admin.token_env`, `admin.viewer_token_env`, `admin.cost_viewer_token_env`, `admin.operator_token_env` | No |
| Redis passwords | `redis_password_env` in every Redis-backed section | No |
| Cross-replica signing secret | `config_propagation.signing_secret_env` | No |
| Alerting webhook and secret | `alerting.webhook_url_env`, `alerting.signing_secret_env` | No |
| Deployment client certificate | `tls.client_cert_path`, `tls.client_key_path` | No. Loaded once |
| Admin mTLS material | `admin.mtls.server_cert_path`, `admin.mtls.server_key_path`, `admin.mtls.ca_cert_path` | No. Loaded once |

A `*_env` value is read with one `os.Getenv` call when the pipeline is built. A process's environment is fixed when it starts, so rewriting a Kubernetes Secret behind an environment variable never reaches a running gateway. A `*_file` path is a real file, which is what a projected Secret volume or a rotation script rewrites in place; that is why the reload loop watches files and not environment variables. When a deployment sets both a `*_file` key and its `*_env` sibling, the file wins and the variable is never read for that credential.

Every key in the table is described in [the config reference](../reference/config.md).

## Rotate an upstream provider credential without a restart

This path needs the deployment to use `*_file` keys already. Moving a deployment from `*_env` to `*_file` is itself a config change and a restart; [Configure provider credentials](provider-credentials.md) covers that.

1. Confirm the deployment reads from files. A Bedrock deployment with a short-lived STS triple looks like this (the `upstream_model` and `base_url` values are placeholders for your own model):

   ```yaml
   deployments:
     claude-bedrock-primary:
       model: "claude-bedrock"
       provider: "bedrock"
       upstream_model: "anthropic.claude-3-5-sonnet-20241022-v2:0"
       base_url: "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20241022-v2:0/converse"
       region: "us-east-1"
       access_key_id_file: "/var/run/secrets/kelvran/access-key-id"
       secret_access_key_file: "/var/run/secrets/kelvran/secret-access-key"
       session_token_file: "/var/run/secrets/kelvran/session-token"
   credential_reload:
     interval_seconds: 30
   ```

   `api_key_file` is the single-file equivalent for `openai`, `anthropic`, `gemini` and `openaicompat`. Omitting `credential_reload`, or setting `interval_seconds` to 0 or below, means the 60 s default; there is no value that disables the loop.

2. Write the new value atomically: write a temporary file, then rename it over the real one. The loop reads whatever bytes are on disk at each tick, so a truncate-then-write sequence can be caught half done.

   ```bash
   umask 077
   printf '%s' "$NEW_SECRET" > /var/run/secrets/kelvran/.secret-access-key.tmp
   mv -f /var/run/secrets/kelvran/.secret-access-key.tmp /var/run/secrets/kelvran/secret-access-key
   ```

   A trailing newline is fine: the file's contents are trimmed of leading and trailing whitespace before use. For an STS triple, write all three files before the next tick. The loop re-reads every configured file on one tick and publishes them together as one snapshot, so a tick that lands between your writes publishes a mixed pair for one interval.

3. Wait up to one `credential_reload.interval_seconds`. The log shows one `INFO` line per changed field:

   ```text
   {"msg":"credential_reload_rotated","deployment":"claude-bedrock-primary","field":"secret_access_key"}
   ```

   `field` is one of `api_key`, `access_key_id`, `secret_access_key`, `session_token`. The value is never logged. A tick that reads the same bytes as before logs nothing.

If a tick cannot read a file (missing, permission denied, volume mid-update), the log shows `WARN credential_reload_read_failed` with `deployment`, `field`, `path` and `error`, and the deployment keeps its last-known-good value; the next tick retries. Fix the file and no restart is needed.

On main since 2026-10-08, not in gateway/v0.17.0: an empty or whitespace-only file is also a read failure, so the last-known-good value is kept. In gateway/v0.16.0 and v0.17.0 an empty file is stored as an empty credential and logged as `credential_reload_rotated`, and every request to that deployment then goes upstream with no credential until the next tick reads a value. The atomic write in step 2 is what prevents this.

Do not use an empty file to drop a session token: on main it is a read failure and the old token is kept; in gateway/v0.16.0 and v0.17.0 it only works because of the bug above. On either, to stop sending one, remove `session_token_file` from the config and restart.

A rotation reaches the gateway within one interval after the file changes. For an STS triple, write the new triple at least one interval before the old session expires; the gateway places no bound of its own on the relationship between the interval and the session lifetime.

### Kubernetes

The shipped manifest in `deploy/k8s/base/deployment.yaml` injects credentials through `envFrom.secretRef`, which is the restart-only path. To use the file path, mount the Secret as a volume and point the `*_file` keys at the mounted paths, for example `/var/run/secrets/kelvran/access-key-id`. A projected Secret volume updates its files atomically, which is what the loop expects. See [deploy/k8s/README.md](../../deploy/k8s/README.md) and [Deploy with Kustomize](deploy/kubernetes-kustomize.md).

### systemd

The packaged unit, [deploy/systemd/kelvran-gateway.service](../../deploy/systemd/kelvran-gateway.service), reads `EnvironmentFile=-/etc/kelvran-gateway/env`, which is the restart-only path. The unit runs with `DynamicUser=yes`, so the service has no fixed UID and cannot read the root-only `0600` file that step 2's `umask 077` produces. Give the service a static group and make the files group-readable: create a group for the purpose (`kelvran-secrets` in this example), add a drop-in with `SupplementaryGroups=kelvran-secrets`, then write each file with `umask 027` and `chgrp kelvran-secrets` before the `mv` (or run the rotation script as that group). Keep the files under `/var/run/secrets/kelvran` or another path outside `/home` and `/tmp`, which `ProtectHome=yes` and `PrivateTmp=yes` hide from the service. After the first rotation, `journalctl -u kelvran-gateway | grep credential_reload_read_failed` must show no `permission denied`. See [Deploy the systemd package](deploy/systemd-package.md).

## Rotate an environment-based credential (restart)

1. Put the new value where the process gets its environment: the Kubernetes Secret named by `envFrom.secretRef`, `/etc/kelvran-gateway/env` on a systemd host, `.env` for Compose, or the ECS task definition's `secrets` block.
2. Restart the process so it reads the new environment:
   - Kubernetes: `kubectl rollout restart deployment/gateway -n kelvran`
   - systemd: `systemctl restart kelvran-gateway`
   - Compose: recreate the `gateway` service; see [Deploy with Docker Compose](deploy/docker-compose.md).
   - ECS: deploy a new task revision; see [deploy/ecs/README.md](../../deploy/ecs/README.md) and [Deploy on ECS Fargate](deploy/ecs-fargate.md).
3. Read the startup log. A deployment whose variable is unset or empty starts anyway and warns, for example `deployment's upstream API key env var is not set; calls to this deployment will fail` (the AWS variants name the access key ID and secret access key). Calls to that deployment then fail until you fix the value and restart again.

Admin tokens are stricter. If `admin.token_env` names a variable that resolves empty, the gateway refuses to start with `admin.token_env "X" is set but resolves to an empty environment variable — refusing to start an unauthenticated admin server`; the viewer, cost-viewer and operator tokens have the same rule. An admin token has no grace period: every admin client must switch to the new token at the restart. `config_propagation.signing_secret_env` has the same refuse-to-start rule, and because it signs the cross-replica channel, rotate it on every replica in one rollout.

`gateway -validate` checks the config shape only. It never reads an environment variable and never opens a credential file, so it passes a config whose secrets are wrong or missing.

## Rotate a client virtual key (live)

A virtual key is a bearer secret the client holds; `config.yaml` stores only its SHA-256 hash in `key_hash`. Rotation issues a new hash and keeps the old secret valid for a grace period.

1. Generate the new secret and hash it. Hash the raw secret with no trailing newline.

   ```bash
   NEW_KEY=$(openssl rand -hex 32)
   NEW_HASH=$(printf '%s' "$NEW_KEY" | sha256sum | cut -d' ' -f1)   # shasum -a 256 on macOS
   ```

2. Call the rotate route with an Admin or Operator tier token. The example uses the `team-alpha` key from [gateway/config.example.yaml](../../gateway/config.example.yaml) and the admin listener at `127.0.0.1:8081`:

   ```bash
   curl -sS -o /dev/null -w '%{http_code}\n' -X POST \
     -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN" -H 'Content-Type: application/json' \
     -d "{\"new_key_hash\":\"$NEW_HASH\",\"grace_period_seconds\":600}" \
     http://127.0.0.1:8081/admin/virtual_keys/team-alpha/rotate
   ```

   Responses: `204` on success; `404` if no key has that name; `400` if `new_key_hash` is missing, is not the hex encoding of a 32-byte SHA-256 digest, or the body is not valid JSON. The audit log records `admin_virtual_key_rotated` with `name` and `grace_period_seconds`, never a hash.

3. Hand `$NEW_KEY` to the client. For `grace_period_seconds` the old secret keeps working; `0` or a negative value ends it immediately. A second rotation during the grace period replaces the one remembered old hash, so only the most recent previous secret is ever accepted.

With more than one replica, the rotation reaches the other running replicas only when `config_propagation.redis_addr` is set together with its required `signing_secret_env`; it is published as the same upsert event a `POST /admin/virtual_keys/{name}` publishes. `admin.redis_addr` on its own decides only what a restarted replica loads. The rotation survives a restart only if `admin.persist_path` or `admin.redis_addr` is set; without either, the next restart loads `config.yaml`'s `key_hash` again. If the persistence write fails, the admin call still returns `204` and the new hash still propagates in memory, but the next restart resurrects the old secret; the log shows `identity_persist_failed`. Fix the store and repeat the rotate call before any restart. See row R12 in [FAILURE-MODES.md](../operations/FAILURE-MODES.md) and [Configure virtual keys and budgets](virtual-keys-and-budgets.md).

## Rotate the Bedrock Guardrails or embed-sim AWS credential

`guardrails.bedrock_guardrails` and `guardrails.embed_sim` take `access_key_id_file`, `secret_access_key_file` and `session_token_file` exactly like a Bedrock deployment, and each runs its own reload loop on the same `credential_reload.interval_seconds`. Rotate the files as in the deployment procedure above. The log lines are `bedrockguard_credential_reload_rotated` and `embedsim_credential_reload_rotated`, with `field` only; read failures are `bedrockguard_credential_reload_read_failed` and `embedsim_credential_reload_read_failed`, with `field`, `path` and `error`, and the last-known-good value is kept. The `*_env` siblings need a restart.

An expired credential at request time is a detector error, handled by the category's error action: a warn-tier category fails open and counts `kelvran.guardrail.fail_open`; a block-tier category returns `400` `content_policy_violation`. embed-sim embeds its corpus when it is constructed, so a bad or expired credential at startup is fatal for the whole process. Row G1 in [FAILURE-MODES.md](../operations/FAILURE-MODES.md) has the full table.

## Verify it worked

File-based deployment credential:

```bash
journalctl -u kelvran-gateway | grep credential_reload_rotated | tail -3          # systemd
kubectl logs deployment/gateway -n kelvran | grep credential_reload_rotated | tail -3   # Kubernetes
```

Expect one line per rotated field within one interval, and no `credential_reload_read_failed` after it. Then send a request to a model served by that deployment. A wrong or expired upstream credential comes back as `502` with the message `upstream provider returned status 401` (or `403`) and `upstream_status` in the `chat_completion` log line; a configured `generic` fallback chain can absorb it silently, so check the deployment's own log line, not only the client status.

Virtual key. On main (not in gateway/v0.17.0) `GET /v1/models` lists the models the key may use and is the cheapest check:

```bash
curl -sS -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $NEW_KEY" http://127.0.0.1:8080/v1/models
```

On gateway/v0.17.0 that route does not exist and returns `404` for every key; send a minimal `POST /v1/chat/completions` to a model the key may use instead. Either way, expect `200` with the new secret. The old secret returns `200` until the grace period ends and `401` after it. A `401` for the new secret means the `new_key_hash` you sent is not the SHA-256 of the secret you handed out.

Environment-based credential after a restart: the startup log has no `env var is not set` warning for the deployment, and for an admin token the process starts at all (a refused start exits before binding a listener).

There is no metric for credential reload outcomes; the log lines above are the only signal, and [metrics and logs](../reference/metrics-and-logs.md) lists what the gateway does emit. Status codes and messages are in [error codes](../reference/error-codes.md).

## After an exposure

Treat any credential that appeared in a terminal, a CI log, a screen share or a coding agent's transcript as compromised and rotate it at once. For Bedrock, prefer a short-lived STS session triple over a long-lived IAM key pair; `session_token_env` / `session_token_file` exist for this. [SECURITY.md](../../SECURITY.md) has the operator guidance and the project's own incident history.

## Not available today

- Hot reload for anything other than deployment and guardrail `*_file` credentials. Admin tokens, Redis passwords, the config-propagation and alerting secrets, deployment client certificates and admin mTLS material are loaded once and need a restart.
- A `SIGHUP`, an admin route, or inotify-style change detection for credential files. Reload is interval polling only.
- A metric for `credential_reload_rotated` or `credential_reload_read_failed`.
- The AWS default credential chain, IRSA or an instance profile for Bedrock deployments or guardrails. A static access key ID and secret access key, from `*_env` or `*_file`, are required.
- An admin route that lists a key's rotation state, or that rotates an admin token with a grace period. Admin tokens are single static values.
- A per-deployment reload interval. `credential_reload.interval_seconds` is one global knob.

## Related

- [Configure provider credentials](provider-credentials.md), [Configure virtual keys and budgets](virtual-keys-and-budgets.md), [Admin API and RBAC](admin-api-rbac.md)
- [Config reference](../reference/config.md), [Admin API reference](../reference/admin-api.md)
- [FAILURE-MODES.md](../operations/FAILURE-MODES.md) rows C1 to C3, G1 and R12
- [Security model](../explanation/security-model.md), [THREAT_MODEL.md](../../THREAT_MODEL.md)
