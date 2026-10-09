# Quickstart: your first chat completion through the Kelvran gateway

This tutorial takes you from a fresh clone to one successful, OpenAI-shaped chat completion through the gateway, with a config file that passes `-validate`, a virtual key you generate yourself, and the `curl` and Python calls that show the request went through. It is for a developer or operator who has never run the gateway before and wants a single working path in about ten minutes. It follows one path only: build from source, route one model to OpenAI. Other ways to run the gateway are linked at the end.

## Before you start

You need:

- A Go toolchain that satisfies `gateway/go.mod` (`go 1.26.9`).
- `git`, `curl`, `openssl`, and `shasum` (macOS) or `sha256sum` (Linux). The steps below use `shasum -a 256`; on Linux, substitute `sha256sum` in the same position.
- An OpenAI API key. The gateway reads it from an environment variable at startup and never stores it.
- Python 3 with the OpenAI SDK, for Step 9 only: `python3 -m pip install openai`. Skip Step 9 if you do not want Python; Steps 7 and 8 already prove the gateway works.
- Port 8080 free on your machine.

Keep one shell open for the whole tutorial. The steps share four shell variables (`KELVRAN_KEY`, `KELVRAN_KEY_HASH`, `OPENAI_API_KEY`, `GATEWAY_PID`) and one background process.

## Step 1: Clone and build the gateway

The Go module lives in the `gateway/` subdirectory of the repository, so the build runs from `gateway/gateway` after the clone.

```bash
git clone https://github.com/kelvran/gateway.git && cd gateway/gateway
go build -o /tmp/kelvran-gateway ./cmd/gateway
/tmp/kelvran-gateway -version
```

What you should see: one line of the form `kelvran-gateway dev (none, built unknown, go1.26.9, darwin/arm64)`. A source build identifies itself as `dev`; the Go version and platform reflect your machine. The binary prints this without reading any config.

## Step 2: Generate a virtual key

A virtual key is a bearer secret the gateway issues to a client. The config file stores only the SHA-256 hash of the secret, never the secret. `printf '%s'` matters: the gateway hashes the exact bytes the client sends, with no trailing newline.

```bash
export KELVRAN_KEY=$(openssl rand -hex 32)
export KELVRAN_KEY_HASH=$(printf '%s' "$KELVRAN_KEY" | shasum -a 256 | cut -d' ' -f1)
echo "$KELVRAN_KEY_HASH"
```

What you should see: a 64-character lowercase hex string. That is the `key_hash` for the next step. `$KELVRAN_KEY` is the secret your client will send as `Authorization: Bearer <secret>`.

## Step 3: Write config.yaml

Write the config into the current directory. `config.yaml` is the binary's default `-config` path, and the repository's `.gitignore` already excludes a file by that name, so it cannot be committed by accident. The heredoc fills in the hash from Step 2.

```bash
cat > config.yaml <<EOF
listen_addr: ":8080"
virtual_keys:
  quickstart:
    key_hash: "${KELVRAN_KEY_HASH}"
    budget_usd: 25.0
deployments:
  gpt4o-primary:
    model: "gpt-4o"
    provider: "openai"
    upstream_model: "gpt-4o"
    base_url: "https://api.openai.com/v1/chat/completions"
    api_key_env: "OPENAI_API_KEY"
price_table:
  gpt-4o:
    prompt_per_token: 0.0000025
    completion_per_token: 0.00001
telemetry:
  exporter: "none"
EOF
cat config.yaml
```

What you should see: the YAML above with your hash inside the `key_hash` quotes.

What each block does:

- `listen_addr` is the only required top-level scalar.
- `virtual_keys` needs at least one entry with a non-empty `key_hash`. `quickstart` is the key's name; `budget_usd` caps what this key may spend.
- `deployments` needs at least one entry with `model`, `provider`, `upstream_model` and `base_url`. `model` is the name clients send; `upstream_model` is what the provider receives. A non-bedrock deployment also needs `api_key_env`, the NAME of the environment variable that holds the provider key. `base_url` must be `https://`.
- `price_table` prices `gpt-4o` so the budget can count. A model absent from `price_table` costs 0 USD, and a budget never decrements for it.
- `telemetry.exporter: "none"` keeps stdout to log lines only. Without this section the gateway prints OTel spans and metrics as JSON on stdout as well.

The parser is a small YAML subset: `key: value` scalars and 2-space-indented mappings. A tab in indentation or a duplicate key at one level is a load error. Every key is annotated in [`gateway/config.example.yaml`](../../gateway/config.example.yaml); the full key list is in [the config reference](../reference/config.md).

## Step 4: Validate the config

```bash
/tmp/kelvran-gateway -validate -config config.yaml
```

What you should see: `config is valid` and exit status 0. On a problem it prints `config error: ...` to stderr and exits 1.

`-validate` only loads the file and checks its shape: required fields, that every `provider` is one of `openai`, `anthropic`, `gemini`, `bedrock`, `openaicompat`, and that `fallback_chains` targets exist. Beyond reading the config file itself, it never reads an environment variable, never opens a credential file or a store, and never dials anything. It also does not check that `key_hash` is well-formed hex; a malformed hash passes `-validate` and fails at real startup with `constructing identity verifier`.

## Step 5: Start the gateway

Export the provider key, then start the gateway in the background with its log going to a file so this shell stays usable.

```bash
export OPENAI_API_KEY="<your OpenAI API key>"
/tmp/kelvran-gateway -config config.yaml > /tmp/kelvran-gateway.log 2>&1 &
GATEWAY_PID=$!
sleep 1
cat /tmp/kelvran-gateway.log
```

What you should see on a Linux host: three JSON log lines, in this order.

```text
{"time":"...","level":"INFO","msg":"build_info","version":"dev","commit":"none","date":"unknown","go_version":"go1.26.9","platform":"linux/amd64"}
{"time":"...","level":"INFO","msg":"gateway_starting","instance_id":"..."}
{"time":"...","level":"INFO","msg":"gateway listening","addr":":8080"}
```

On macOS, Windows or any other host without cgroups, two more lines appear between `build_info` and `gateway_starting`: `{"time":"...","level":"ERROR","msg":"failed to set GOMEMLIMIT","package":"github.com/KimMachineGun/automemlimit/memlimit","error":"failed to set GOMEMLIMIT: cgroups is not supported on this system"}` and `{"time":"...","level":"WARN","msg":"automemlimit: could not set GOMEMLIMIT from cgroup","error":"failed to set GOMEMLIMIT: cgroups is not supported on this system"}`. Both are harmless: the gateway only reads a cgroup memory limit on Linux, and it is up once the last line is `gateway listening`. Set `AUTOMEMLIMIT=off` before starting to replace them with one `INFO` line `AUTOMEMLIMIT is off, skipping`. Inside a Linux container that has a memory limit, one `INFO` line `GOMEMLIMIT is updated` appears in the same position instead.

`gateway listening` means the data plane is accepting connections. If you forgot the `export`, you also see a `WARN` line `deployment's upstream API key env var is not set; calls to this deployment will fail`. The gateway still starts in that case; Step 7 then fails with a 502.

## Step 6: Check health and readiness

```bash
curl -s http://localhost:8080/healthz
echo
curl -s http://localhost:8080/readyz
echo
```

What you should see:

```text
{"status":"ok"}
{"models":{"gpt-4o":true},"ready":true}
```

Neither route needs a bearer token. `/healthz` is pure liveness and always answers 200 while the process is up. `/readyz` answers 200 when every configured model has a healthy deployment and 503 otherwise; with no `health_probe` section, every deployment stays eligible, so it reports ready.

## Step 7: Send your first chat completion

```bash
curl -s -i http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $KELVRAN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Say hello in five words."}]}'
```

What you should see: status `HTTP/1.1 200 OK`, `Content-Type: application/json`, an `X-Kelvran-Overhead-Duration-Ms` header (the gateway's own added latency in milliseconds, excluding the provider's time), and a body of this shape:

```json
{"id":"...","object":"chat.completion","created":1759900000,"model":"...","choices":[{"index":0,"message":{"role":"assistant","content":"..."},"finish_reason":"stop"}],"usage":{"prompt_tokens":14,"completion_tokens":7,"total_tokens":21}}
```

The gateway hashed the bearer token, matched it to `quickstart`, routed `gpt-4o` to the `gpt4o-primary` deployment, sent the request upstream with your `OPENAI_API_KEY`, and priced the `usage` against `price_table` toward the key's 25 USD budget.

The request body is OpenAI Chat Completions shaped: `model`, `messages[]` of `{role, content}`, and optional `temperature`, `max_tokens`, `tools`, `stream`, `response_format`. Unknown fields are dropped silently.

## Step 8: See the key being enforced

Send the same request with a wrong token.

```bash
curl -s -i http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer wrong" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

What you should see: `HTTP/1.1 401 Unauthorized`, `Content-Type: application/json; charset=utf-8`, `X-Content-Type-Options: nosniff`, and this body:

```json
{"error":{"message":"dataplane: auth: identity: invalid virtual key","type":"authentication_error","param":null,"code":"invalid_api_key"}}
```

Every error on the three `/v1` routes uses this OpenAI envelope with all four keys present (`null` when unknown), so the official OpenAI SDKs raise their usual exception classes. The full status-to-code table is in [the error codes reference](../reference/error-codes.md).

## Step 9: Call it from the OpenAI Python SDK

Any OpenAI SDK works by pointing `base_url` at the gateway's `/v1` prefix and passing the virtual key secret as the API key. With the `openai` package installed (see Before you start), in the same shell:

```bash
python3 - <<'EOF'
import os
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key=os.environ["KELVRAN_KEY"])
resp = client.chat.completions.create(model="gpt-4o", messages=[{"role": "user", "content": "hi"}])
print(resp.choices[0].message.content)
EOF
```

What you should see: one line with the model's reply. The SDK sent `Authorization: Bearer $KELVRAN_KEY` and parsed the same `chat.completion` body as Step 7. More SDK detail is in [the OpenAI Python how-to](../how-to/clients/openai-python.md).

## Step 10: Stop the gateway

```bash
kill "$GATEWAY_PID"
sleep 1
tail -n 2 /tmp/kelvran-gateway.log
```

What you should see: a log line with `"msg":"gateway shutting down"`, then the process exits. `SIGTERM` and `SIGINT` both trigger a graceful shutdown: up to 30 s to drain the HTTP server, then up to 15 s for in-flight handlers, then up to 5 s to flush telemetry. With no traffic it exits at once.

You now have a built binary, a config that loads, a virtual key with a budget, and a client that talks to the gateway exactly as it would talk to OpenAI.

## If something went wrong

- `502` with `error.message` `upstream provider returned status 401` and `error.code` `upstream_error`: your virtual key was accepted and the request reached OpenAI, which rejected `OPENAI_API_KEY`. Check the export in Step 5. The provider's full error text is only in the gateway's `chat_completion` log line, never in the response.
- `config error: ... is not https`: a `base_url` with `http://` needs `allow_insecure_http: true` on that deployment. This tutorial's `base_url` is `https://` and does not need it.
- `gateway exited` with `constructing identity verifier`: the `key_hash` is not 64 hex characters. Re-run Step 2 and Step 3.

The full catalogue is in [the troubleshooting how-to](../how-to/troubleshooting.md) and [FAILURE-MODES](../operations/FAILURE-MODES.md).

## Not available today

- No GitHub Release carries downloadable binaries or packages yet. The release-asset pipeline is on main since 2026-10-08, not in gateway/v0.17.0; `gateway/v0.17.0` and earlier tags have no assets and cannot be rebuilt. The next `gateway/v*` tag is the first that will. See [RELEASE.md](../../RELEASE.md).
- No Homebrew formula, no npm or crates.io package.
- No `make run` or `make dev` target; `make setup` only downloads dependencies.
- No first-party client SDK. The OpenAI SDK with `base_url` is the integration path, as in Step 9.
- No OpenAPI document for the public routes.
- No Anthropic Messages API. The gateway serves five routes only: `POST /v1/chat/completions`, `POST /v1/embeddings`, `GET /v1/models`, `GET /healthz`, `GET /readyz`.

## Where next

- [Your first virtual key and budget](first-virtual-key-and-budget.md): rate limits, `allowed_models`, budget windows and what a 429 looks like.
- [Streaming](../how-to/streaming.md): add `"stream": true` and read Server-Sent Events ending in `data: [DONE]`.
- [Provider credentials](../how-to/provider-credentials.md): Anthropic, Gemini, Bedrock and self-hosted `openaicompat` deployments.
- [Run with Docker Compose](../how-to/deploy/docker-compose.md) and [the container image reference](../reference/container-image.md): the same config mounted at `/config.yaml` in `ghcr.io/kelvran/gateway`.
- [Data-plane API reference](../reference/data-plane-api.md): every route, header and response field.
- [Your first eval suite](first-eval-suite.md): the separate Python `evals` deployable.
- [Security model](../explanation/security-model.md): why the config holds hashes and environment-variable names, never secrets.
