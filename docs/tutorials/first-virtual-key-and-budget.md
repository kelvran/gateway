# Tutorial: your first virtual key with a budget and a rate limit

This tutorial walks you through issuing a second tenant credential on a running Kelvran gateway, giving it a spending cap and a request rate limit, and then watching the gateway enforce both. It is for someone who has finished the [quickstart](quickstart.md) and wants to see, end to end, what a virtual key does. You do not need to know the admin API or the budget model beforehand; both are introduced as you go.

By the end you have:

- enabled the gateway's admin API on loopback,
- created a key named `team-beta` live, without a restart, with a budget of 0.00001 USD and a limit of one request per second,
- seen a `429` with `Retry-After` for the rate limit and a `429 insufficient_quota` for the budget,
- read the key's spend back, and deleted the key.

The gateway makes one real upstream call during this tutorial: the request that is served in step 6. Against the quickstart's `price_table` it costs about a hundredth of a cent.

## Before you start

You need the state the quickstart leaves behind:

- A `config.yaml` in your current directory with the `quickstart` key, the `gpt4o-primary` deployment for model `gpt-4o`, and the `price_table` entry for `gpt-4o`. The price table matters: a model with no `price_table` entry is billed as 0 USD, and a budget never decrements for it.
- The gateway binary built to `/tmp/kelvran-gateway`, running directly on your machine (not in a container), and `OPENAI_API_KEY` exported in the shell you start it from.
- `curl`, `openssl` and `shasum` on your PATH.

Keep two terminals open: one where the gateway runs and prints its logs, one where you type the commands below. Every `export` in this tutorial must happen in the terminal that runs the command that uses it.

## Step 1: generate the second secret and its hash

A virtual key is a random secret that clients send as a bearer token. The gateway never stores the secret, only its SHA-256 digest, so the first thing you make is the pair. (After Step 3, with `KELVRAN_ADMIN_TOKEN` exported, `kelvran keys create team-beta --budget 0.00001 --reset monthly --warn 0.8 --models gpt-4o` — on `main` since 2026-10-10, not in `gateway/v0.17.0` — does Steps 1 and 4 in one command and prints the secret once; it has no flag for Step 4's `rate_limit` block, whose `burst: 1` the Retry-After demonstration below relies on, so this tutorial keeps the manual path.)

```bash
export TEAM_BETA_KEY=$(openssl rand -hex 32)
export TEAM_BETA_KEY_HASH=$(printf '%s' "$TEAM_BETA_KEY" | shasum -a 256 | cut -d' ' -f1)
echo "$TEAM_BETA_KEY_HASH"
```

What you should see: one line of 64 lowercase hex characters. That is the only thing about this key the gateway ever sees in configuration. `printf '%s'` matters: `echo` would append a newline and hash a different string than the one clients send.

## Step 2: enable the admin API in `config.yaml`

The admin API is off unless `admin.token_env` names an environment variable. Open `config.yaml` and append this block at the end, at the left margin (it is a top-level section, like `virtual_keys`):

```yaml
admin:
  listen_addr: "127.0.0.1:8081"
  token_env: "KELVRAN_ADMIN_TOKEN"
```

`127.0.0.1:8081` is also the default when `listen_addr` is omitted; it is written out here so the port you call in later steps is visible. `token_env` holds the name of the variable, never the token itself. Now create the token in the terminal where you will start the gateway:

```bash
export KELVRAN_ADMIN_TOKEN=$(openssl rand -hex 32)
```

If the named variable is unset or empty in the environment the gateway starts from, the gateway refuses to start with `admin.token_env "KELVRAN_ADMIN_TOKEN" is set but resolves to an empty environment variable`, rather than run an unauthenticated admin server. Forgetting the `export` in the gateway's terminal produces exactly this error.

Check the file still parses:

```bash
/tmp/kelvran-gateway -validate -config config.yaml
```

What you should see: `config is valid` and exit code 0. `-validate` does not read environment variables, so it does not check that `KELVRAN_ADMIN_TOKEN` is set; the next step does.

## Step 3: restart the gateway and confirm the admin API answers

The admin section is read at startup, so the gateway has to be started once more; this is the only restart in the tutorial. The quickstart's last step already stopped its gateway (if yours is still running, `kill "$GATEWAY_PID"` or press Ctrl-C). In the terminal you reserved for the gateway, make sure `OPENAI_API_KEY` and `KELVRAN_ADMIN_TOKEN` are both exported, then start it in the foreground so its log is visible:

```bash
/tmp/kelvran-gateway -config config.yaml
```

What you should see: the same JSON log lines as in the quickstart, including `gateway listening` with `addr` `:8080`, plus one new line whose `msg` is `admin server listening`, with `addr` `127.0.0.1:8081` and `mtls` `false`. That line is the confirmation that the `admin` section was read; without it, check that the block from step 2 is at the left margin of `config.yaml`.

Back in your command terminal, copy the admin token across if it is a different shell, then list the keys the gateway knows about:

```bash
curl -s -i http://127.0.0.1:8081/admin/virtual_keys \
  -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN"
```

What you should see: `HTTP/1.1 200 OK` and a JSON array with one entry:

```json
[{"id":"quickstart","budget_usd":"25","budget_reset_interval_seconds":0,"budget_warn_percent":0,"rate_limit_burst":20,"rate_limit_refill_per_second":10}]
```

Two things to notice. The entry never includes `key_hash`; the list and spend routes are written never to return it. (`GET /admin/config` is different: it dumps the loaded configuration verbatim, including every statically configured key's `KeyHash`, to any admin or viewer token, so treat that route as secret-bearing.) And `quickstart` already has a rate limit although the quickstart never set one: a key with no `rate_limit` section gets the gateway default of burst 20 and refill 10 per second.

If the token is wrong or missing, the answer is `401` with the plain-text body `invalid admin token` (or `missing or malformed Authorization header`). The admin API answers errors as plain text, not with the JSON error envelope the data plane uses on port 8080.

## Step 4: create `team-beta` live

Now create the second key through the admin API. The request body uses the same field names as the `virtual_keys` section of `config.yaml`, so nothing you learn here is specific to the API.

```bash
curl -s -i -X POST http://127.0.0.1:8081/admin/virtual_keys/team-beta \
  -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN" -H "Content-Type: application/json" \
  --data-binary @- <<EOF
{
  "key_hash": "$TEAM_BETA_KEY_HASH",
  "budget_usd": 0.00001,
  "budget_reset_interval_seconds": 2592000,
  "budget_warn_percent": 0.8,
  "allowed_models": ["gpt-4o"],
  "rate_limit": {"burst": 1, "refill_per_second": 1}
}
EOF
```

What you should see: `HTTP/1.1 204 No Content` and no body. The key is usable immediately; no restart.

What each field does:

- `key_hash` is the digest from step 1. It is required, and two keys with the same hash are rejected.
- `budget_usd: 0.00001` is a cumulative spending cap. It is deliberately tiny so that one real call crosses it. Zero or omitted means unlimited.
- `budget_reset_interval_seconds: 2592000` turns the cap into a rolling 30-day window. Zero means the cap is for the lifetime of the process.
- `budget_warn_percent: 0.8` is a fraction: at 80 percent of `budget_usd` the gateway logs a warning on every billable completion. It is log-only and never rejects a request.
- `allowed_models` restricts the key to `gpt-4o`. A request for any other model gets `403` with code `model_not_allowed`.
- `rate_limit` is a token bucket: `burst` 1 and `refill_per_second` 1 allow one request, then one more each second. Always send both: `config.yaml` rejects one without the other, but the admin API does not check the pair, and a `burst` with no `refill_per_second` is accepted as a bucket that never refills.

In the gateway terminal a log line `admin_virtual_key_upserted` with `name` `team-beta` appears. The audit line records the name, never the hash.

## Step 5: read the spend before any call

```bash
curl -s http://127.0.0.1:8081/admin/virtual_keys/team-beta/spend \
  -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN"
```

What you should see:

```json
{"spent_usd":"0","budget_usd":"0.00001","budget_reset_interval_seconds":2592000,"percent_used":0}
```

`spent_usd` and `budget_usd` are decimal strings, not floats. `percent_used` is `spent_usd` divided by `budget_usd` as a plain ratio: 1 means the cap is reached.

## Step 6: trigger the rate limit

The bucket holds one token, so two requests that arrive within the same second produce one success and one rejection. A human cannot paste two commands fast enough, so the block below starts the first request in the background, waits 200 ms, and sends the second in the foreground.

```bash
curl -s -o /dev/null -w 'first request: %{http_code}\n' http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $TEAM_BETA_KEY" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}' &
sleep 0.2
curl -s -i http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $TEAM_BETA_KEY" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
wait
```

What you should see, in this order. The second request is rejected at once, before the first has finished its upstream call:

```http
HTTP/1.1 429 Too Many Requests
Content-Type: application/json; charset=utf-8
Retry-After: 1

{"error":{"message":"dataplane: rate limit exceeded","type":"rate_limit_error","param":null,"code":"rate_limit_exceeded"}}
```

Then, when the upstream answers, the background request prints:

```text
first request: 200
```

The `Retry-After` value is a whole number of seconds and starts at 1. It grows per key while rejections stay consecutive and resets on the next success. OpenAI-compatible clients retry on `rate_limit_error` and honour this header.

The rate limit is checked before the budget. Per request the gateway checks, in order: the bearer token, the key's source-IP allowlist, its model allowlist, its rate limit, its concurrency cap, and only then its budget. So the rejected request above consumed no budget.

## Step 7: trigger the budget

The first request in step 6 was served and billed at the quickstart's `price_table` rates. For `gpt-4o`, the prompt tokens of even a one-word message cost more than 0.00001 USD, so the key is now over its cap. The rule is: a request is admitted while spend is below the cap, and rejected once spend is at or above it. The call that crosses the cap is served; the next one is not.

Wait two seconds first, so the rate-limit bucket has refilled and the rejection you see is the budget's, not the rate limit's:

```bash
sleep 2
curl -s -i http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $TEAM_BETA_KEY" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

What you should see:

```http
HTTP/1.1 429 Too Many Requests
Content-Type: application/json; charset=utf-8

{"error":{"message":"dataplane: budget exceeded","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}
```

Same status code as the rate limit, different `type` and `code`, and no `Retry-After` header. A budget rejection is not a "try again shortly" signal, so the gateway never attaches one. Note that the official OpenAI SDKs retry every 429 a couple of times with their own backoff before raising, whatever the `type`; a client that wants to stop at once on a budget rejection has to look at `error.code` and treat `insufficient_quota` as final itself.

## Step 8: read the spend back

```bash
curl -s http://127.0.0.1:8081/admin/virtual_keys/team-beta/spend \
  -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN"
```

What you should see (example values; yours differ with the exact token counts):

```json
{"spent_usd":"0.000115","budget_usd":"0.00001","budget_reset_interval_seconds":2592000,"percent_used":11.5}
```

`spent_usd` is the real cost of the one served call, and `percent_used` is above 1 because that call overshot the cap. Only the served call is counted; the two rejected requests added nothing.

Look at the gateway terminal as well. After the served call settled, the gateway logged a `budget_warn_threshold_crossed` warning with `key_id` `team-beta`, `spent_usd`, `budget_usd` and `warn_percent` 0.8, because spend is at or above 80 percent of the budget. Next to it is a second warning, `budget_threshold_crossed` with `percent_bucket` 1, from the gateway's fixed ladder of 0.5, 0.75, 0.9 and 1 (50, 75, 90 and 100 percent of the cap); that one fires once per rung per budget window, the `budget_warn_threshold_crossed` line repeats on every further billable completion while spend stays over the threshold, and neither affects the request itself.

## Step 9: delete the key

```bash
curl -s -i -X DELETE http://127.0.0.1:8081/admin/virtual_keys/team-beta \
  -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN"
```

What you should see: `HTTP/1.1 204 No Content`. Deleting an unknown name answers `404`; deleting the last remaining key answers `409`, because the gateway never lets itself end up with no key that can authenticate.

Confirm the secret is dead:

```bash
curl -s -i http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $TEAM_BETA_KEY" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

What you should see:

```http
HTTP/1.1 401 Unauthorized

{"error":{"message":"dataplane: auth: identity: invalid virtual key","type":"authentication_error","param":null,"code":"invalid_api_key"}}
```

Deleting a key also clears its budget state. There is no route that zeroes spend while keeping the key, so "delete and recreate" is the lever when a key must start over before its reset window.

## What is and is not durable after this tutorial

Everything you created in this tutorial lived in the gateway's memory: a key created through the admin API, its spend, and its rate-limit bucket are all gone at the next restart, and the gateway comes back with exactly what `config.yaml` says. Making keys and spend survive a restart, or sharing them between replicas, is covered in [Virtual keys and budgets](../how-to/virtual-keys-and-budgets.md) and [Backup and restore](../how-to/backup-and-restore.md).

Not available today:

- There is no admin route that resets a key's spend without deleting it. The levers are `DELETE` and recreate, or a `budget_reset_interval_seconds` window.
- There is no team or organisation hierarchy above keys; each key stands alone.
- The admin API has no JSON error envelope (errors are plain text) and no OpenAPI specification.

## Where next

- To do the same with static YAML, add rotation with a grace period, set per-model limits or a concurrency cap, or make keys and spend survive a restart: [Virtual keys and budgets](../how-to/virtual-keys-and-budgets.md), [Rotate credentials](../how-to/rotate-credentials.md), [Backup and restore](../how-to/backup-and-restore.md).
- To give finance a read-only token for `/spend`, or an operator a token that can rotate but not create keys: [Admin API and RBAC](../how-to/admin-api-rbac.md).
- Every admin route, body field and status code: [Admin API reference](../reference/admin-api.md). Every `type` and `code` the data plane returns: [Error codes](../reference/error-codes.md). Every `virtual_keys` field: [Configuration reference](../reference/config.md) and the annotated [`gateway/config.example.yaml`](../../gateway/config.example.yaml).
- What happens to budgets and keys when Redis or the persistence store fails: [Failure modes](../operations/FAILURE-MODES.md).
- Why keys are hashed, why the admin API is loopback-only, and how tenants are isolated: [Security model](../explanation/security-model.md).
- Running the gateway in a container, where loopback inside the container is not your host's loopback: [Deploy with Docker Compose](../how-to/deploy/docker-compose.md).
- Next tutorial: [Your first eval suite](first-eval-suite.md).
