# Manage prompt templates in the gateway

This page shows an operator how to store versioned prompt templates in the Kelvran gateway over the Admin API, promote a version with a label, and let clients call the template by `prompt_id` instead of sending `messages`. It is for the person who holds the Admin token and for the application developer who calls the data plane.

Use this when you want one server-side copy of a system prompt that every client picks up without a redeploy, with version history and a one-call rollback.

## Prerequisites

- A running gateway with the `admin:` section configured. The Admin token is the environment variable named by `admin.token_env`; the optional read-only token is named by `admin.viewer_token_env`. The examples below use the names from [gateway/config.example.yaml](../../gateway/config.example.yaml): `KELVRAN_ADMIN_TOKEN` and `KELVRAN_ADMIN_VIEWER_TOKEN`. See [Admin API roles](admin-api-rbac.md).
- The admin listener. When `admin.listen_addr` is unset it binds to `127.0.0.1:8081`; the examples use that address.
- A virtual key for the data-plane call, held in an environment variable (`KELVRAN_KEY` below). See [Virtual keys and budgets](virtual-keys-and-budgets.md).
- A model name that exists in your `deployments` list. The examples use `gpt-4o`.

Prompts are global. Any virtual key may resolve any `prompt_id`; there is no per-key ownership. Treat template text as visible to every tenant of the gateway. See [SECURITY.md](../../SECURITY.md).

## Steps

### 1. Make templates survive a restart

Without this step templates live in memory only and vanish when the process exits. Add a `prompt:` section to `config.yaml`:

```yaml
prompt:
  persist_path: "/var/lib/kelvran-gateway/prompts.db"
```

`prompt.persist_path` names a bbolt file. The store is loaded at startup, honours `admin.on_corrupt_store`, is included in `POST /admin/backup`, and can be restored offline. [gateway/config.example.yaml](../../gateway/config.example.yaml) has no `prompt:` section today; the key is documented in [config reference](../reference/config.md) and [DEPLOY.md](../operations/DEPLOY.md). Restart the gateway after the change.

### 2. Create version 1

```bash
curl -X POST http://127.0.0.1:8081/admin/prompts/greeting \
  -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"system","content":"You are {{persona}}."},{"role":"user","content":"Say hi to {{name}}."}]}'
```

Response: `200` with `{"id":"greeting","version":1,"messages":[...],"created_at":"<RFC 3339>"}`.

Rules the handler enforces, each a `400` with a plain-text body: the id is at most 256 bytes; `messages` is non-empty and has at most 2000 entries; inline multimodal parts must pass the MIME check.

### 3. Publish a new version

Send the same `POST` again with the new content. Versions are append-only: every `POST /admin/prompts/{id}` creates version `len(existing)+1` and never edits an earlier version. The second call returns `"version":2`.

### 4. Promote a version with a label

```bash
curl -X PUT http://127.0.0.1:8081/admin/prompts/greeting/labels/production \
  -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"version":2}'
```

Response: `200` with `{"prompt_id":"greeting","label":"production","version":2,"updated_at":"<RFC 3339>"}`. A body of `{"version":0}` points the label at whatever is latest right now and stores that number. An unknown id or version is `404`.

### 5. Call the template from a client

Omit `messages`. Send `prompt_id`, the label, and the variables:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $KELVRAN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","prompt_id":"greeting","prompt_label":"production","prompt_variables":{"persona":"a pirate","name":"Ada"}}'
```

The gateway replaces the request's messages with the stored template. Nothing is merged. `{{persona}}` becomes `a pirate` and `{{name}}` becomes `Ada`. The same resolution runs on the streaming path (`"stream": true`).

Variable rules: a placeholder is `{{name}}` where `name` matches `[A-Za-z0-9_]+`; inner whitespace such as `{{ name }}` is accepted. Substitution applies to each message's `content` and to `parts[].text`. A placeholder with no matching variable stays in the text as written; it is never an error and never dropped.

## Variants

### Pin a version instead of a label

Send `"prompt_version": 1` in place of `prompt_label`. `prompt_version` is an integer; `0`, a negative value or absence means the latest version at resolution time, and no value is rejected on its own. Sending a positive `prompt_version` together with `prompt_label` is a `400`; a `prompt_version` of `0` or less alongside `prompt_label` is not rejected and the label is used.

### Roll back

Rollback is the same `PUT` as step 4 with an older number, for example `{"version":1}`. Every client that requests `prompt_label: "production"` serves version 1 on its next call. No client change is needed.

### Remove a label or a prompt

- `DELETE /admin/prompts/{id}/labels/{label}` returns `204`; `404` if the label does not exist.
- `DELETE /admin/prompts/{id}` removes every version and clears the prompt's labels; returns `204`, or `404` for an unknown id. A later `POST` to the same id starts again at version 1, so clients pinned with `prompt_version` resolve the new content under the old number.

Both need the Admin token.

### Read history with the Viewer token

```bash
curl http://127.0.0.1:8081/admin/prompts/greeting/versions/1 \
  -H "Authorization: Bearer $KELVRAN_ADMIN_VIEWER_TOKEN"
```

`GET /admin/prompts` lists the latest version of every prompt, sorted by id. `GET /admin/prompts/{id}` returns the latest version. `GET /admin/prompts/{id}/versions/{version}` returns one version; a non-positive or non-numeric version is `400`, an unknown one is `404`. Reads accept the Admin or the Viewer token. Operator and CostViewer tokens are rejected with `401` on every prompt route.

### Erase a cached answer created through a template

`POST /admin/cache/erase` accepts the same `prompt_id`, `prompt_version`, `prompt_label` and `prompt_variables` fields as the original request, so you can target the entry that request created. See [Caching](caching.md) and the [Admin API reference](../reference/admin-api.md).

## Verify it worked

1. Read the template back:

   ```bash
   curl -s http://127.0.0.1:8081/admin/prompts/greeting \
     -H "Authorization: Bearer $KELVRAN_ADMIN_VIEWER_TOKEN"
   ```

   Returns `200` and a JSON object whose `"version"` is the number of `POST` calls you made (2 after steps 2 and 3).

2. Send the step 5 request. The upstream model receives `You are a pirate.` and `Say hi to Ada.`; the completion reads accordingly.

3. Send the step 5 request with `"prompt_label":"does-not-exist"`. You get `400`. On main since 2026-10-08 the body is a JSON envelope whose `error.type` is `invalid_request_error` and `error.code` is `invalid_prompt_reference`; gateway/v0.17.0 returns the same `400` with a `text/plain` body `dataplane: failed to resolve prompt_id: ...` and no `error` object.

4. If the admin audit log is on (`admin.enable_audit_log`, default true when the `admin` section is present), the log holds `admin_prompt_upserted` lines with `id` and `version`, and an `admin_prompt_label_set` line with `id`, `label` and `version`. Template content is never logged. Reads appear as `admin_prompts_read` with `authorized_by` set to `admin` or `viewer`. See [Metrics and logs](../reference/metrics-and-logs.md).

5. Each resolved request carries span attributes `kelvran.prompt.id` and `kelvran.prompt.version`. The version is the one actually resolved, so a label request reports the concrete number. Requests without `prompt_id` emit neither attribute.

6. If you set `prompt.persist_path` in step 1, restart the gateway and repeat item 1 of this list: `version` is still 2, proving the file was written and reloaded. Then send the step 5 request: it returns the same `400` as item 3, because the `production` label did not survive the restart. Re-run step 4 and the step 5 request succeeds again.

## Errors on the data plane

All of these are `400`. On main since 2026-10-08 the body is a JSON envelope with `error.type` `invalid_request_error` and the `error.code` below; gateway/v0.17.0 returns the same `400` with a `text/plain` body and no `error` object.

| Condition | `error.code` |
|---|---|
| `prompt_id` and a non-empty `messages` in one request | `invalid_prompt_reference` |
| A positive `prompt_version` and `prompt_label` in one request | `invalid_prompt_reference` |
| Unknown `prompt_id`, version or label | `invalid_prompt_reference` |
| Resolved template fails the MIME check on inline parts | `invalid_prompt_reference` |
| Template resolves to zero messages | `empty_messages` |

An unknown prompt is never a `404` or `502` on `/v1` routes. See [Error codes](../reference/error-codes.md).

## Caching behaviour

Every resolved request folds a fingerprint `<id>:v<N>:<sha256 of the stored messages>` into the cache key. A new version never serves an answer cached for an old one, even when the resolved text is byte-identical. See [Caching](caching.md).

## Durability notes

- Labels are held in memory only. Even with `prompt.persist_path` set, a restart loses every label assignment while version history survives. Re-run step 4 after every restart, or pin versions with `prompt_version` where a restart gap is unacceptable.
- Backup: `POST /admin/backup` writes a `prompt-<UTC timestamp>.bbolt` file to `admin.backup_dir` when `prompt.persist_path` is set. Restore is offline: stop the gateway, then run the binary with `-restore-store prompt -restore-from <backup-file>` against the same config file. See [Backup and restore](backup-and-restore.md) and [DEPLOY.md](../operations/DEPLOY.md).
- A write to the persist file can fail after the in-memory change applied. `POST` then answers `400` and `DELETE` answers `500`, each with a body starting `prompt: Upsert: persisting` or `prompt: Delete: persisting removal`. Treat that body as a store fault, not a client error; there is no log line or metric for it. Row P6 in [FAILURE-MODES.md](../operations/FAILURE-MODES.md) has the recovery steps.
- A second process opening the same `prompt.persist_path` fails within one second with `another process holds the file lock`. This is on main since 2026-10-08, not in gateway/v0.17.0; gateway/v0.17.0 waits indefinitely.

## Not available today

- No persistence for labels; only version history is written to `prompt.persist_path`.
- No route to list or read labels. `GET /admin/prompts` and `GET /admin/prompts/{id}` return `{id, version, messages, created_at}` with no labels field. The only record of a label assignment is the `admin_prompt_label_set` audit line.
- No static YAML definition of templates. The Admin API is the only write path; there is no `prompt.templates` key.
- No tenant scoping, ownership or per-key allowlist of prompts.
- No templating beyond literal `{{name}}` substitution: no conditionals, loops, defaults or escaping.
- No `prompt:` example block in `gateway/config.example.yaml`.
- No Operator-tier or Viewer-tier write access to prompts or labels.
- No log line or metric when a prompt persistence write fails.

## Version history

- gateway/v0.5.0 (2026-09-15): `prompt_id`, `prompt_version`, `prompt_variables` and the Admin CRUD routes.
- gateway/v0.11.0 (2026-09-16): `prompt.persist_path` bbolt persistence.
- gateway/v0.13.0 (2026-09-19): `prompt_label` and the `PUT`/`DELETE /admin/prompts/{id}/labels/{label}` routes.

## Related

- [Data-plane API reference](../reference/data-plane-api.md): `prompt_id`, `prompt_version`, `prompt_label`, `prompt_variables`.
- [Admin API reference](../reference/admin-api.md): every `/admin/prompts` route, body and status code.
- [Config reference](../reference/config.md): `prompt.persist_path`, `admin.on_corrupt_store`, `admin.backup_dir`, `admin.enable_audit_log`.
- [Design RFC](../rfcs/2026-09-13-gateway-prompt-management.md): rationale for global, replace-not-merge templates. Its statements that no real persister exists predate gateway/v0.11.0.
