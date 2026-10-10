# kelvran CLI

`kelvran` is the gateway's companion binary (`gateway/cmd/kelvran`), shipped beside `kelvran-gateway` in every release archive and package and at `/kelvran` in the container image. It turns the by-hand first run — generate a secret, hash it, author `config.yaml` — into one command (`init`), checks a config against the environment the gateway will actually run in (`doctor`), creates, lists, rotates and deletes virtual keys through the admin API or in the file (`keys`), shows what the gateway serves (`status`), reads spend per key (`spend`), and prints or writes a coding tool's client configuration (`connect`). First shipped in `gateway/v0.18.0`. Design: [RFC: `kelvran` CLI and single-user mode](../rfcs/2026-10-09-gateway-kelvran-cli-and-single-user-mode.md).

This page is the public surface [`docs/VERSIONING.md`](../VERSIONING.md) binds to for the CLI: every verb, flag, exit code and output shape below is covered by the compatibility policy from `gateway/v0.18.0` on.

## Getting the binary

| Where | How |
|---|---|
| Release archive or package | Beside `kelvran-gateway`: `kelvran-gateway_<version>_<os>_<arch>.tar.gz` (and the `.zip` on Windows) unpack both binaries; the deb, rpm and apk install `/usr/bin/kelvran` (see [Release artifacts](release-artifacts.md)) |
| Container image | `/kelvran` in `ghcr.io/kelvran/gateway`. The `ENTRYPOINT` stays `/gateway`; run the CLI with `docker run --rm --entrypoint /kelvran ghcr.io/kelvran/gateway:<tag> …` or `docker exec <container> /kelvran …` (see [Container image](container-image.md)) |
| Source | `cd gateway && go build -o /tmp/kelvran ./cmd/kelvran` (a plain build reports `kelvran dev (none, built unknown, …)`) |

## Conventions

- Usage errors (an unknown command or flag, a missing required flag, a flag that does not apply to the selected provider) print the usage text to stderr and exit `2`.
- Any other failure prints `kelvran <command>: <reason>` to stderr and exits `1`.
- Results go to stdout; the Next steps block follows the result on stdout, or goes to stderr with the dry-run notice under `--dry-run`. Warnings go to stderr prefixed `kelvran <command>: warning:`. An invocation with no command prints the usage and exits `2`.
- The CLI never inspects a credential's value beyond whether it is set, except for the one prefix check described under `init`, and never prints one it merely detected. The secrets it *generates* (a virtual key, an admin token) are printed exactly once, as a shell export line; `connect` is the one verb that reads a secret it is handed — the virtual key — and prints it exactly once or writes it into the Claude Code settings file.

## `kelvran -version`

Prints one line, `kelvran <version> (<commit>, built <date>, <go version>, <os>/<arch>)`, and exits `0`. The three build fields are injected by the release build's `-ldflags -X main.version/.commit/.date` for `./cmd/kelvran` separately from the gateway's (ldflags symbols are per main package); a source build prints `kelvran dev (none, built unknown, …)`. `--version` and `version` are accepted spellings. The release acceptance job asserts the `kelvran <version> (` prefix on the installed deb and the tarball; the image publish job asserts it on `/kelvran` in the pushed image.

## `kelvran init`

```
kelvran init [--single-user] [--out config.yaml] [--provider auto|anthropic|openai|gemini|bedrock|openaicompat]
             [--base-url URL] [--region REGION] [--listen 127.0.0.1:8080] [--models a,b]
             [--upstream-model CANONICAL=BEDROCK_ID]... [--budget USD] [--price MODEL=PROMPT,COMPLETION]...
             [--persist-path ABS] [--no-persist] [--dry-run] [--force]
```

Writes a minimal, priced `config.yaml` in the parser's subset (block mappings, two-space indents, quoted strings, bare numbers, comments on their own lines), generates the first virtual key, and prints the client exports once. The file holds no secret: every credential in it is the *name* of an environment variable the gateway reads at startup. Before writing, `init` loads its own output through the gateway's loader and validator and checks the parsed fields it intended, so a file it writes is one `kelvran-gateway -validate` accepts.

### Provider selection

| `--provider` | Selected when | Deployment it writes |
|---|---|---|
| `auto` (default) | For each credential found in the CLI's environment: `ANTHROPIC_API_KEY`; `OPENAI_API_KEY`; `GEMINI_API_KEY` or `GOOGLE_API_KEY`; `AWS_ACCESS_KEY_ID` together with `AWS_SECRET_ACCESS_KEY`. None found: exit `1` naming the variables it looked for. | One per detected provider, in the order anthropic, openai, gemini, bedrock |
| `anthropic` | always | `model`/`upstream_model` `claude-sonnet-5-5`, `base_url` `https://api.anthropic.com/v1/messages`, `api_key_env` `ANTHROPIC_API_KEY` |
| `openai` | always | `gpt-4o`, `https://api.openai.com/v1/chat/completions`, `OPENAI_API_KEY` |
| `gemini` | always | `gemini-2.5-flash`, `https://generativelanguage.googleapis.com/v1beta/models/<model>:generateContent`, `GEMINI_API_KEY` (`GOOGLE_API_KEY` when it is the only one of the two set) |
| `bedrock` | always; needs a region from `--region`, `AWS_REGION` or `AWS_DEFAULT_REGION` (exit `1` otherwise — `init` refuses to guess), shaped like an AWS region id (`us-east-1`, `us-gov-west-1`; anything else is exit `2` from the flag or `1` from the variable, because the region is spliced into the signed endpoint's host) | `model` `claude-sonnet-5-5`, `upstream_model` `global.anthropic.claude-sonnet-5-5` (the global inference-profile form; Anthropic's models overview lists the base id `anthropic.claude-sonnet-5-5`, which Bedrock's Converse endpoint rejected with 400 while the global form answered 200, checked live on 2026-10-10), `base_url` `https://bedrock-runtime.<region>.amazonaws.com/model/<upstream_model>/converse`, `access_key_id_env`/`secret_access_key_env` `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`, `session_token_env` `AWS_SESSION_TOKEN` only when that variable is set, `region` |
| `openaicompat` | never by `auto` (a protocol, not a vendor); requires `--base-url`, `--models` and a `--price` for every model (exit `2` without the first two, `1` without a price); a URL carrying credentials, a query or a fragment is exit `2`, and the URL is never echoed | `base_url` is `--base-url` with `/chat/completions` appended unless already present; `allow_insecure_http: true` for an `http://` URL; `api_key_env` `OPENAI_API_KEY` |

Bedrock is the one provider where the canonical `model` differs from `upstream_model`; the price table, routing and allow-lists are keyed by the canonical id, which is what Claude Code and the Anthropic SDK send.

### Models and prices

- `--models a,b` replaces the provider's default model with the listed canonical ids. It applies to one provider: with `--provider auto` and several credentials detected it is a usage error (exit `2`) — pass `--provider`.
- Every emitted model must have a `price_table` entry, because a model absent from the table costs $0 and never decrements a budget. `init` takes the price from `--price MODEL=PROMPT_PER_TOKEN,COMPLETION_PER_TOKEN` (USD per token; `0,0` is the documented way to opt into $0 knowingly) or from its embedded table; a model in neither is exit `1` naming the model and the flag.
- The embedded table, as of **2026-10-10**, from the providers' pricing pages (Anthropic's standard tier; Haiku 5.5 at its ≤100k-token-prompt tier; Gemini's paid tier; gpt-4o from this repository's documented example):

| Canonical model | Provider | `prompt_per_token` | `completion_per_token` | `cache_read_per_token` | `cache_creation_per_token` | Bedrock id |
|---|---|---|---|---|---|---|
| `claude-sonnet-5-5` | anthropic, bedrock | 0.000002 | 0.00001 | 0.0000001 | 0.0000025 | `global.anthropic.claude-sonnet-5-5` |
| `claude-opus-5-5` | anthropic, bedrock | 0.000004 | 0.00002 | 0.0000002 | 0.000005 | `global.anthropic.claude-opus-5-5` |
| `claude-haiku-5-5` | anthropic, bedrock | 0.0000001 | 0.0000005 | 0.00000001 | 0.000000125 | `global.anthropic.claude-haiku-5-5` |
| `claude-fable-5-1` | anthropic, bedrock | 0.00001 | 0.00005 | 0.00000025 | 0.0000125 | `global.anthropic.claude-fable-5-1` |
| `gpt-4o` | openai | 0.0000025 | 0.00001 | | | |
| `gemini-2.5-flash` | gemini | 0.0000003 | 0.0000025 | | | |

  The date is written as a comment above the emitted `price_table`. Bedrock bills these models at Anthropic's first-party rates on its global endpoints; regional endpoints carry a premium the table does not apply. Prices change — check your provider's page and override with `--price`.
- `--upstream-model CANONICAL=BEDROCK_ID` (bedrock only, repeatable) supplies the Bedrock id for a `--models` entry the embedded mapping lacks, or overrides a mapped one (for example a regional `us.`/`eu.` inference-profile form); without it such a model is exit `1` naming the flag. `--base-url` is rejected for bedrock (exit `2`): the Converse URL is derived.

### Modes

| | `--single-user` | default (team mode) |
|---|---|---|
| `listen_addr` | `127.0.0.1:8080` (`--listen` to change; the host must be an IP address or a hostname; a non-loopback value such as `:8080` or `0.0.0.0:8080` is written with a warning) | same |
| `virtual_keys` | one key named `default`, `key_hash` = SHA-256 of the generated secret, no `budget_usd` (unlimited) unless `--budget USD` | same |
| `admin` | none (so `keys --config` and `status --config` work offline against the file; `spend` and the live columns of `status` need the admin API) | `token_env: "KELVRAN_ADMIN_TOKEN"` and `persist_path`: `--persist-path` (must be absolute), or `<directory of --out>/kelvran-identity.db`, or `/var/lib/kelvran-gateway/identity.db` when `--out` resolves under `/etc/kelvran-gateway/` (the package layout's `StateDirectory`); `--no-persist` omits it with a warning that admin-created keys will not survive a restart. `--persist-path` and `--no-persist` are usage errors under `--single-user` |
| `telemetry` | `exporter: "none"`, preceded by a comment explaining that without it spans and a 60 s metrics dump would share stdout with the JSON logs | same |
| Admin token | not generated | generated and printed once as `export KELVRAN_ADMIN_TOKEN=…` for the gateway's shell |

### Output

Without `--dry-run`: `Wrote <absolute path> (mode 0644; it holds no secret).` followed by a **Next steps** block on stdout — `kelvran-gateway -config '<--out>' -validate` (the path is single-quoted; `--out` may not contain a quote or a line break, and the listen host must be an IP address or hostname, so no printed line carries shell syntax), the admin-token export in team mode, `kelvran-gateway -config …`, then the client exports: the generated secret exactly once as `export KELVRAN_KEY=<secret>`, and `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN="$KELVRAN_KEY"` and `OPENAI_BASE_URL`/`OPENAI_API_KEY="$KELVRAN_KEY"` referencing it. The base URLs come from `listen_addr` with an empty or wildcard host replaced by `127.0.0.1` (`http://127.0.0.1:8080` and `http://127.0.0.1:8080/v1`). The block also notes that the exports belong in the client's shell (an `OPENAI_API_KEY` export in the gateway's shell would replace the upstream credential an openai deployment reads), that `POST /v1/responses` is not served, that Claude Code's native mode needs `POST /v1/messages` which the gateway does not serve yet, that a pasted export persists in shell history (`read -rs` keeps it out), under `--single-user` that `spend` and the live columns of `status` need an `admin` section, and, whenever exactly one deployment was written, `One model is configured (<model>); rerun with --models a,b to serve more.`

With `--dry-run`: the YAML is the only thing on stdout (so `kelvran init --dry-run --provider openai > config.yaml` and the release acceptance job's `kelvran-gateway -config <that file> -validate` both work); the dry-run notice and the Next steps block — including the generated secret's export line — go to stderr, and the notice says so; nothing is written. Redirect stderr away from a log you keep.

A new `--out` is created with `O_EXCL` and mode `0644` regardless of the umask (the packaged unit's `DynamicUser` must read it; it holds no secret), so a file that appears between the check and the write is refused rather than clobbered. An existing `--out` is exit `1` with the file untouched unless `--force`, which replaces it atomically through a temp file and rename; a symlinked `--out` is written through to its target and the link is kept.

### Warnings

| Condition | Message (stderr) |
|---|---|
| `--listen` is not loopback-only | `listen_addr "…" is not loopback-only; every host that can reach it can present a virtual key` |
| `--no-persist` | `admin.persist_path omitted (--no-persist): keys created, rotated or deleted through the admin API will not survive a restart` |
| `ANTHROPIC_API_KEY` holds a value starting with `sk-ant-oat` | names the variable and the prefix, explains that a Claude subscription OAuth token is not an API key and that own-token passthrough is outside the gateway's scope today, and continues (exit `0`). The value itself is read only for this prefix test and never printed. A later release turns this into a refusal once the primary sources are archived (RFC-3 decision 9, gate G33) |

### Exit codes

| Code | When |
|---|---|
| `0` | Written (or printed under `--dry-run`); `-h` |
| `1` | No provider credential found under `--provider auto`; bedrock without a region, or an `AWS_REGION`/`AWS_DEFAULT_REGION` that is not shaped like a region id; a model without a price or, for bedrock, without a Bedrock id; a value (such as an `--upstream-model` id) containing a quote, `#` or line break the config parser cannot represent (the value is not echoed); an existing `--out` without `--force`; a write failure; the generated config failing its own load/validate check (a bug, not a flag error) |
| `2` | Unknown flag or positional argument; `--provider` not one of the six values; `--models` with several detected providers, empty, listing a model twice, or listing two models that collide on the deployment key (`a/b` and `a-b`); a malformed `--upstream-model` pair; a malformed (non-decimal) `--budget`; `--base-url` outside openaicompat, not an http(s) URL, or carrying credentials, a query or a fragment; `--region` or `--upstream-model` outside bedrock; a `--region` that is not shaped like an AWS region id; openaicompat without `--base-url` or `--models`; a malformed or negative `--price`; a non-positive `--budget`; a relative `--persist-path`; `--persist-path`/`--no-persist` with `--single-user`; an unparsable `--listen` or a listen host that is not an IP address or hostname; an `--out` containing a quote or a line break |

## `kelvran doctor`

```
kelvran doctor [--config config.yaml] [--env-file PATH]... [--strict-env]
               [--url URL] [--admin-url URL] [--admin-token-file PATH]
               [--allow-insecure-http] [--json]
```

`-validate` plus the startup-only checks a user actually hits, evaluated honestly against the environment `doctor` can see. It loads the config with the gateway's own loader and validator, then prints one row per finding — `severity | check | detail`, sorted errors first — and a summary line; exit `1` when any row is an `error` (the gateway would refuse to start, or the premise is wrong), `0` otherwise. `--json` prints one document instead: `{"config", "findings": [{"severity", "check", "detail"}], "errors", "warnings", "info"}`; `findings` is always an array (`[]` for a clean config). Every line `doctor` prints — table cells, the `kelvran doctor:` error lines, the flag parser's own errors and the stderr host note — escapes control characters as `\u00XX`, so a config key, a path or a host cannot steer the terminal. No value of any variable or file is ever printed, only names and paths: a load or validation error withholds the quoted content of the offending line and prints any URL it echoes with the password redacted and the query withheld, or withheld whole when it does not parse, along with any fragment of it the URL parser's own error repeats (the loader's own messages echo both — `base_url` verbatim when it is not https; `-validate` keeps that), a `*_env` field that does not look like a variable name is reported as a pasted value without showing it, a `--config` that is the packaged env file or one of the `--env-file` paths is refused before loading (exit `2`), and a non-200 admin response contributes at most one sanitised line (nothing at all for 401/403) with the token redacted.

### Where the gateway's environment comes from

The CLI's process environment is not the gateway's for the deployments the packages and the image target: the unit loads `EnvironmentFile=-/etc/kelvran-gateway/env`, a container gets `docker run -e`, `env_file:` or `envFrom:`. So:

- `--env-file PATH` (repeatable; later files win) parses the systemd `EnvironmentFile=` grammar — blank and `#`/`;` lines skipped, `KEY=value`, matching single or double quotes stripped, a trailing backslash continues the value — which Docker's `--env-file` and Compose's `env_file:` share when unquoted. A shell `export` prefix is refused with a pointer to `KEY=value`, because neither consumer accepts it. Its values are merged over the process environment for the checks only.
- With no `--env-file` and a `--config` under `/etc/kelvran-gateway/`, `/etc/kelvran-gateway/env` is read automatically. It is root-only (`install -m 0600`), so a non-root run gets one `warning` (`env.file`: pass `--env-file` with a copy you can read, or run `sudo kelvran doctor …` — as root every file reads as readable, so the `*_file` rows then say less, while the world-readable checks still apply) and continues without an env source.
- `--strict-env` says this shell *is* the gateway's whole environment (the `init --single-user` flow): every variable the config names must be set here, so each miss is an error, not the warning startup would log.

### Checks

| Check | Severity | Condition |
|---|---|---|
| `config.load`, `config.validate` | error | What `kelvran-gateway -validate` reports, as a row, with a parse error's quoted line content withheld and any echoed URL's password redacted and query withheld; a load failure stops the run |
| any `*_env` field | error | The field holds something that is not an environment-variable name (`^[A-Za-z_][A-Za-z0-9_]*$`): almost always a pasted value, which `-validate` accepts silently; the value is not shown |
| `env.file` | warning | The packaged `/etc/kelvran-gateway/env` exists but cannot be read (root-only: pass `--env-file` or run under `sudo`), does not parse (the unit's `EnvironmentFile=` would reject it too), or fails to open for another reason; the run continues without an env source |
| `deployments.<name>.api_key_env` / `access_key_id_env` / `secret_access_key_env` / `session_token_env`, `guardrails.bedrock_guardrails.*_env`, `guardrails.embed_sim.session_token_env`, `*.redis_password_env`, `alerting.webhook_url_env`, `alerting.signing_secret_env` | warning with an env file, **error** with `--strict-env` | The named variable is unset or empty. With an env file the severity mirrors startup (the gateway starts and logs the consequence the detail names; the file may not be its whole environment); with `--strict-env` the shell is declared to be the whole environment, so the miss is an error; without either it says the variable is not set *in this process* and how to rerun where the gateway's environment is visible (`--env-file`, `docker exec <ctr> /kelvran doctor --config /config.yaml --strict-env`, `kubectl exec … -- /kelvran doctor … --strict-env`, `--strict-env`) |
| `admin.token_env`, `admin.viewer_token_env`, `admin.cost_viewer_token_env`, `admin.operator_token_env`, `config_propagation.signing_secret_env` (when `redis_addr` is set), `guardrails.embed_sim.access_key_id_env` / `secret_access_key_env` | **error** with an env source, warning without | The gateway refuses to start on these: the admin tiers and the propagation channel must not run unauthenticated, and the embed-sim detector embeds its corpus at construction, so an empty credential fails startup |
| `config_propagation.signing_secret_env` | error | `redis_addr` is set and the key is absent: the gateway refuses to start; `-validate` passes |
| `deployments.<name>.api_key_env` | warning | The variable's value starts with `sk-ant-oat` (a Claude subscription OAuth token, not an API key; gate G33, Stage 1 form). The value is read for this prefix test only |
| `deployments.<name>.*_file`, `guardrails.*.*_file` | warning | The credential file cannot be read as the invoking user or is empty (the gateway's own reader rule) — documented as relative to the gateway's uid and mount namespace, so a miss is a pointer, not proof |
| `deployments.<name>.tls.*`, `admin.mtls.*`, `guardrails.embed_sim.corpus_path` | warning | The file does not exist as the invoking user |
| `config.mode` | error | Only under `/etc/kelvran-gateway/`: the config is not world-readable, so the unit's `DynamicUser` cannot read it and `ExecStartPre -validate` fails |
| `admin.persist_path`, `budget.persist_path`, `prompt.persist_path`, `admin.audit_log_path`, `admin.backup_dir`, every `*_file`, PEM and `guardrails.embed_sim.corpus_path` path | warning | Only under `/etc/kelvran-gateway/` (paths are cleaned first, so `..` segments cannot dodge the rules): a path under `/home`, `/root` or `/tmp` (hidden by `ProtectHome=yes`/`PrivateTmp=yes`); a writable path outside `/var/lib/kelvran-gateway/` (`ProtectSystem=strict`); a credential file that is not world-readable (the `DynamicUser` uid is allocated at start) |
| `price_table.<model>` | warning | A deployment's model has no `price_table` entry: it costs $0 and never decrements a budget |
| `telemetry.exporter` | error / info | Not one of `stdout`, `otlp`, `none` (the gateway refuses to start; `-validate` does not check this) / the section is absent, so the default `stdout` exporter interleaves spans and a 60 s metrics dump with the logs |
| `admin.persistence` | warning | An `admin` section with neither `persist_path` nor `redis_addr`: admin-made key changes are lost at restart |
| `listen_addr` | warning | Single-user mode (no `admin` section) and `listen_addr` is not loopback-only |
| `deployments.<name>.allow_insecure_http` | warning | Requests to that deployment leave in clear text, credential included (the URL is printed with any password redacted and the query withheld) |
| `probe.url` | error | `--url` is not an http(s) URL, or carries credentials, a query or a fragment; no request is sent |
| `probe.readyz`, `probe.auth` | error / info | With `--url`: `GET /readyz` must answer 200 and a bearer-less `GET /v1/models` must answer 401 (the listener is up and auth is enforced). Neither request carries a credential, so `--url` is not subject to the loopback rule below, but it must be an http(s) URL without credentials, query or fragment; redirects are not followed and a 3xx is reported as a redirect |
| `admin.probe`, `admin.config`, `admin.deployments[.<name>]` | error / warning / info | With `--admin-url`: the token resolves from `--admin-token-file`, then `KELVRAN_ADMIN_TOKEN_FILE`, then the variable the config's `admin.token_env` names, then `KELVRAN_ADMIN_TOKEN` (never a flag value; each variable is looked up in the env file first, then in this process, so `sudo kelvran doctor --admin-url …` under the package layout finds the token `/etc/kelvran-gateway/env` holds); none → `admin.probe` error, probes skipped. The probe runs only when `--admin-url` is given — `KELVRAN_ADMIN_URL` and the `http://127.0.0.1:8081` default belong to the verbs that always talk to the admin plane — and the URL must be `https`, or `http` with a loopback host, or `--allow-insecure-http` was passed — otherwise `kelvran doctor: --admin-url <url> is not https and not loopback; pass --allow-insecure-http if this is a deliberate LAN/dev endpoint`, exit `1`, no request sent. `scheme://host[:port]` is printed to stderr before the first authenticated request; the URL may not carry credentials, a query or a fragment, and redirects are never followed (a 3xx is reported as `a redirect, not followed`, so the bearer never travels to a `Location`). `GET /admin/config` must answer 200 — a 401/403 is reported by status alone, any other failure with one sanitised line of the body (the token — raw, URL-encoded or base64 — is redacted before the line is cut to 120 runes, so a truncation cannot leave a prefix behind), and the token is redacted from every detail; every entry of `GET /admin/deployments` with `healthy: false` is a warning |

What `doctor` does not check yet: the lossy-ingress warning (plan item 11), Redis reachability, whether the audit log, backup and persist paths can be opened (under the package layout they are checked for location only), the Gemini and Bedrock `base_url` suffixes, and anything about a running gateway without `--url`/`--admin-url`.

### Supported invocations

- Local: `kelvran doctor --config config.yaml --strict-env` after `init --single-user`.
- Package: `sudo kelvran doctor --config /etc/kelvran-gateway/config.yaml` (reads `/etc/kelvran-gateway/env`), or without `sudo` plus `--env-file`.
- Image: `docker exec <container> /kelvran doctor --config /config.yaml --strict-env` or `kubectl exec <pod> -- /kelvran doctor --config /config.yaml --strict-env` — inside the gateway's own environment, so the `*_env` checks see what the gateway sees and, with `--strict-env`, a miss is an error rather than a warning; add `--url http://127.0.0.1:8080` for the listener probe.

### Exit codes

| Code | When |
|---|---|
| `0` | No `error` finding; `-h` |
| `1` | Any `error` finding (including a config that does not load); an unreadable or malformed `--env-file` (a line that is not `KEY=value`, a dangling backslash, an `export` prefix); a refused or non-http(s) `--admin-url`; an unreadable or empty admin token file (`--admin-token-file` or the one `KELVRAN_ADMIN_TOKEN_FILE` names) |
| `2` | Unknown flag or positional argument; a `--config` that is the packaged env file or one of the `--env-file` paths |

## `kelvran keys`

```
kelvran keys create <name> [--budget USD] [--reset daily|weekly|monthly|none] [--warn FRACTION] [--models a,b]
                           [--expires 30d|720h|RFC3339] [--billing-subject ID] [--replace] [--force] [--json]
kelvran keys list   [--spend] [--json]
kelvran keys rotate <name> [--grace 10m] [--expires 30d|720h|RFC3339] [--json]
kelvran keys delete <name> [--json]
  every verb: [--config PATH] [--admin-url URL] [--admin-token-file PATH] [--allow-insecure-http]
```

Create, list, rotate and delete virtual keys (RFC-3 decision 3; since `gateway/v0.18.0`). The CLI generates the secret (32 random bytes as 64 hex characters, the shape `openssl rand -hex 32` produces) and prints it exactly once, in the client-shell export block `init` prints; only its SHA-256 hash is sent to the admin API or written to the file.

### Online or offline

The mode follows the admin token, never a flag:

- **Online**, whenever a token resolves: from `--admin-token-file`, else the file `KELVRAN_ADMIN_TOKEN_FILE` names, else the variable the config's `admin.token_env` names when `--config` is given and loads, else `KELVRAN_ADMIN_TOKEN` — never a flag value. The URL is `--admin-url`, else `KELVRAN_ADMIN_URL`, else `http://127.0.0.1:8081`, and must be `https`, or `http` with a loopback host, or `--allow-insecure-http` was passed; otherwise `kelvran keys: --admin-url <url> is not https and not loopback; pass --allow-insecure-http if this is a deliberate LAN/dev endpoint` (naming `KELVRAN_ADMIN_URL` instead when the URL came from it; only the scheme and host are echoed, never a username, query or fragment, and a URL carrying any of those — or a bare `?`/`#` — is refused outright), exit `1`, no request sent. `kelvran: sending the admin token to scheme://host[:port]` is printed to stderr before the first request. A token whose URL refuses the connection is `admin API unreachable at <url>; if this is a single-user config it has no admin listener (<dial error>)`, exit `1`; the dial error is redacted like a response body, so a far end that echoes the bearer cannot put it on stderr. A `--config` that does not load beside a resolving token is a stderr warning naming the file, and the verb proceeds online.
- **Offline**, with `--config` and no token: the `virtual_keys.<name>` block of the file is rewritten as a text operation that preserves every other byte (below). A persisted store (`admin.persist_path`, `admin.redis_addr`) is never edited.
- Neither a token nor `--config`: exit `2`, naming both routes.

### What each verb does

| Verb | Online | Offline |
|---|---|---|
| `create <name>` | `GET /admin/virtual_keys` first (the upsert answers 204 for create and replace alike): an existing name is refused without `--replace`; with it, a listed `previous_key_hash_expires_at`, `max_concurrent_requests` or a `billing_subject_id` not given again is named and refused without `--force`, because a full replace drops them; every other field of the old entry (`rate_limit`, `allowed_regions`, `allowed_source_cidrs`, `cache_scope_to_end_user`, `attribution_capture_ids`) is dropped without a prompt. Then `POST /admin/virtual_keys/<name>` with `key_hash` and the fields below | An existing name is refused without `--replace` (offline `--replace` = delete + append, with no `--force` inspection: every field of the old entry goes). The entry is appended after the block's last content line at the entries' indent, with `key_hash` and only the non-default fields |
| `list` | `GET /admin/virtual_keys[?include=spend]`, as the table `key \| budget_usd \| reset \| warn \| models \| expires_at` (+ `spent_usd \| percent_used`). A row whose spend could not be read shows `n/a`; the stderr line `kelvran keys: spend unavailable for N key(s): budget backend error` follows and the exit code is `1`, so the table is never mistaken for an all-zero fleet | The file's keys in the same table, headed `from <path>; a running gateway reflects this file only after restart`; `--spend` is refused (spend lives in the budget store) |
| `rotate <name>` | `POST /admin/virtual_keys/<name>/rotate` with `new_key_hash`, `grace_period_seconds` (`--grace` in whole seconds; default 0, the previous secret stops at once) and `expires_at` when `--expires` is given; a 409 (the key has already expired) is reported with "re-run with `--expires`" | Replaces only the value token of the entry's `key_hash` line, keeping its quoting, spacing and any trailing comment; `--expires` rewrites or inserts `expires_at`; `--grace` is a usage error (the file holds one hash, so an offline rotation is a hard cut at the next start); an entry whose `expires_at` has passed needs `--expires` |
| `delete <name>` | `DELETE /admin/virtual_keys/<name>`; a 404 and the last-key 409 are reported | Removes the entry's span — its line through its last content line, comments inside included; the file's last key is refused |

Flags map to wire fields: `--budget` → `budget_usd` (omitted = unlimited); `--reset` → `budget_reset_interval_seconds`, daily = 86400, weekly = 604800, monthly = 2592000 (rolling windows, not calendar ones), none = 0; `--warn` → `budget_warn_percent`, accepted only in (0, 1] because the server's [0, 100] bound would take 80 for 0.8 and yield a threshold that never fires; `--models` → `allowed_models`, the key's allow-list (not `init --models`, which selects deployments); `--expires` → `expires_at`, an absolute RFC 3339 UTC instant converted locally from `30d` or `720h` so retries are idempotent, refused when not in the future (exit `1`; a malformed value is exit `2`); `--billing-subject` → `billing_subject_id`. `--alert-thresholds` is not offered (the ladder is fixed at 50/75/90/100) and `--expires` is RFC 3339, not a unix timestamp.

After an online `create`, `rotate` or `delete` the CLI reads `GET /admin/config` once: when the served config has neither `admin.persist_path` nor `admin.redis_addr`, it prints `warning: this gateway has no admin.persist_path or admin.redis_addr — this change is lost at the next restart (docs/reference/admin-api.md)` on stderr. A 401 on that read (an Operator token on `rotate`) skips the warning and the base-URL lines silently and never fails the mutation.

### The offline rewrite

The writer is defined in the parser's own terms — the loader's comment stripper, colon finder and quote stripper classify every line — so it cannot diverge from the gateway's reading of the file. The section is the unique indent-0 `virtual_keys:` line with nothing after the colon (a `virtual_keys:` inside a comment, deeper, or with a value is not it). The entries' indent is read from the file, never assumed. An entry spans from its line to the last content line before the next line at its indent or shallower; comment and blank lines strictly inside that span go with it on `delete`, while comment lines above the entry and blank or comment lines after the span stay byte for byte. `create` inserts before any trailing blank or comment lines, so column-0 commentary introducing the next section stays attached to it. A name the subset cannot represent — empty, with surrounding whitespace, a `#`, a line break, or wrapped in quotes — is refused with a pointer at the online path; a name with a colon is written double-quoted.

Before anything is written, the ORIGINAL must load (a file the gateway cannot read is never edited). The candidate must then load, every `Config` field except `VirtualKeys` must be deep-equal to the original's, and `VirtualKeys` must be the original's plus exactly the intended change — otherwise nothing is written and the message says so. The write is atomic (a temp file in the same directory, synced, renamed) and keeps the file's mode and owner; a symlinked `--config` keeps its symlink and rewrites the target; under `/etc/kelvran-gateway/` a result that is not world-readable draws `doctor`'s warning. Caveats on stderr: always that the change takes effect on the next gateway start; with `admin.persist_path` or `admin.redis_addr`, that a key ever changed through the admin API is overridden by the store on restart; with an `admin:` section and no store, that admin-API changes are in-memory only and the file wins on restart.

### Output

Text: a headline — `Created virtual key "team-beta" (online: POST /admin/virtual_keys/team-beta).`, or `… in <path> (offline; takes effect on the next gateway start).` — the `expires_at` when set, then the client-shell block: `export KELVRAN_KEY=<secret>` once, with `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN` and `OPENAI_BASE_URL`/`OPENAI_API_KEY` built from `listen_addr` (the served config online, the file offline; the two token lines alone when the listen address is unknown or its host is not an IP address or hostname — a served value is untrusted input for a line you paste into a shell), and the shell-history note. `--json`: one document — `{"verb","mode","name","config"?,"key","key_hash","expires_at"?,"grace_period_seconds"?}` for `create` and `rotate` (the secret appears once, there), `{"verb","mode","name","config"?}` for `delete`, and the served (or file-derived) list entries for `list`. Caveats and warnings go to stderr in every mode.

### Exit codes

| Code | When |
|---|---|
| `0` | The verb succeeded; `-h` |
| `1` | A refusal (the name exists without `--replace`, dropped fields without `--force`, not found, the file's last key, a past `--expires`, an expired key rotated without `--expires`), an admin API error or an unreachable admin URL, a refused `--admin-url` or `KELVRAN_ADMIN_URL`, an empty or unreadable admin token file, an unloadable `--config` offline, a candidate that fails the semantic check, a write error, `list --spend` with an unavailable row, offline `list --spend` |
| `2` | Unknown verb or flag; a missing or extra name; a malformed `--budget`, `--reset`, `--warn`, `--models`, `--expires` or `--grace`; `--grace` offline; a name or model the file subset cannot hold; neither a token nor `--config` |

## `kelvran status`

```
kelvran status [--url http://127.0.0.1:8080] [--admin-url URL] [--admin-token-file PATH] [--config PATH]
               [--allow-insecure-http] [--json]
```

What the gateway serves (RFC-3 decision 10; since `gateway/v0.18.0`). The mode follows the admin token exactly as `keys` does: **online** whenever a token resolves (`--admin-token-file`, `KELVRAN_ADMIN_TOKEN_FILE`, the variable the config's `admin.token_env` names, `KELVRAN_ADMIN_TOKEN`), through the same credential-URL guard; **offline** with `--config` and no token; neither is exit `2` naming both routes; a token whose URL refuses the connection is the `admin API unreachable at <url>` line, exit `1`.

- `--url` (either mode) probes the data plane without a credential: `GET /readyz` is `200 ready`, `503 not ready` with the models that have no healthy deployment, `unreachable`, or `not ready` with the reason when the body is not the gateway's JSON (a proxy page answering 200 never reads as ready); the URL must be http(s) without credentials, a query or a fragment. A probed plane that is not ready or unreachable makes the exit code `1` after the view is printed. Without `--url` the line reads `not probed`.
- **Online** the view comes from three reads: `GET /admin/config` (the served config as PascalCase JSON, from which `status` reads only `listen_addr`, the configured key count and the admin store — the body carries key hashes, which are never decoded or printed), `GET /admin/deployments` (the live table: `name | model | upstream_model | provider | kind | healthy | weight | latency_factor_percent | sticky`) and `GET /admin/virtual_keys` (the live key count, admin-API changes included).
- **Offline** the view is the file's: `listen_addr`, the deployment table limited to its static columns (`name | model | upstream_model | provider | kind`) and the key count, headed `from <path>; a running gateway reflects this file only after restart`, plus one line saying how to get the live view — for a config without an `admin:` section (single-user mode): re-run `init` without `--single-user`, or add a block-mapping `admin:` section with `token_env: KELVRAN_ADMIN_TOKEN` (the admin listener defaults to `127.0.0.1:8081`, so it stays loopback-only); for a config with one: export the variable `admin.token_env` names, or pass `--admin-token-file`. Exit `0`, unless `--url` probed a plane that is not ready. The text view prints the restart caveat once, in its headline; `--json` keeps it in `notes`.
- `admin store` names where admin-API key changes persist: `persist_path <p>`, `redis_addr <a>`, `none (keys changed through the admin API are in-memory only)` with an `admin:` section and no store, or `none (single-user: no admin section)`.

`--json` prints one composite document — `{"mode","source","readyz"?,"listen_addr","deployments":[{"name","model","upstream_model","provider","kind","healthy"?,"weight"?,"latency_factor_percent"?,"sticky"?}],"virtual_keys":{"configured","live"?},"admin_store","notes"?}` — pinned by a golden file; `readyz` is `{"url","status"?,"ready","models_without_healthy_deployment"?,"error"?}` (`status` is absent when the plane was unreachable); the live deployment fields and `virtual_keys.live` are present online only. Fields may be added, never renamed or removed.

| Code | When |
|---|---|
| `0` | The view printed and, when probed, the data plane is ready; `-h` |
| `1` | A probed data plane not ready or unreachable; an admin API error or an unreachable admin URL; a refused `--admin-url`, `KELVRAN_ADMIN_URL` or `--url`; an unloadable `--config` offline; an empty or unreadable admin token file |
| `2` | Unknown flag or a positional argument; neither a token nor `--config` |

## `kelvran spend`

```
kelvran spend [--by key] [--admin-url URL] [--admin-token-file PATH] [--config PATH] [--allow-insecure-http] [--json]
```

Spend per virtual key (RFC-3 decisions 5 and 10; since `gateway/v0.18.0`): `GET /admin/virtual_keys?include=spend` rendered as `key | spent_usd | budget_usd | percent_used | expires_at`, the same cell spellings as `keys list --spend` (`unlimited`, `never`, `n/a`). A row whose spend could not be read shows `n/a`, the stderr line `kelvran spend: spend unavailable for N key(s): budget backend error` follows and the exit code is `1`. `--by` defaults to `key`; `model`, `tool` and `session` are refused with `needs the spend ledger (plan item 13d, RFC-2 …)` until that ledger lands; any other value is a usage error. `--json` prints the served entries as one document.

Spend lives only in the gateway's budget store, which the admin API fronts, so there is no offline view: with `--config` and no token the command fails closed — the same line `status` prints offline goes to stderr, exit `1` — and with neither a token nor `--config` it is exit `2`. The token order, the URL guard and the unreachable line are `keys`'.

| Code | When |
|---|---|
| `0` | The table printed with every row's spend; `-h` |
| `1` | An unavailable row; no token in reach with `--config` given; an admin API error or an unreachable admin URL; a refused `--admin-url`; an unloadable `--config`; an empty or unreadable admin token file |
| `2` | Unknown flag or a positional argument; `--by` outside key/model/tool/session; `--by model`, `tool` or `session` (the ledger); neither a token nor `--config` |

## `kelvran connect`

```
kelvran connect claude [--url http://127.0.0.1:8080] [--key-file PATH] [--write] [--scope user|project] [--discovery]
                       [--check] [--replace-credential] [--force] [--allow-insecure-http]
kelvran connect codex|aider|continue [--url http://127.0.0.1:8080] [--key-file PATH]
```

The client configuration that points a coding tool at this gateway (RFC-3 decision 8, gate G32's defaults; since `gateway/v0.18.0`). The virtual key comes from `--key-file`, else the file `KELVRAN_KEY_FILE` names, else `ANTHROPIC_AUTH_TOKEN` in this shell — the export `init` printed, so after pasting it `connect` needs no file; never `OPENAI_API_KEY`, which in the same shell is the upstream credential an `openai` deployment reads. None of the three → exit `1` naming them. `--url` is the gateway's base URL without a path (Claude Code appends `/v1/messages`, the OpenAI SDKs `/v1/…`); its host must be an IP address or a hostname, since it is pasted into shell lines. The key must be a bearer token: a value carrying whitespace, a control character or a byte outside `A-Z a-z 0-9 - . _ ~ + / =` could never authenticate and is exit `1` naming its source (the file or the variable), never its value; `--key-file` must be a regular file.

### `connect claude`

`--url` is subject to the credential-URL guard whether printing, writing or checking — `https`, or `http` with a loopback host, or `--allow-insecure-http` — because the written `ANTHROPIC_BASE_URL` makes Claude Code send the key on every turn; a refused URL is exit `1` with nothing printed or sent. In every mode, `ANTHROPIC_API_KEY` set in this shell is a stderr warning with the `env -u ANTHROPIC_API_KEY claude` remedy (`init`'s single-user flow exports it as the upstream credential; Claude Code would send it in `x-api-key`, which this gateway does not read). `--check` and `--write` are exclusive, and `--scope`, `--replace-credential` and `--force` apply only with `--write` (exit `2` otherwise).

- **Print** (the default): the settings env block `{"env": {"ANTHROPIC_BASE_URL": "<url>", "ANTHROPIC_AUTH_TOKEN": "<key>", "CLAUDE_CODE_GATEWAY_HINT_HEADERS": "1"}}` (plus `"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1"` with `--discovery`), then the VS Code `claudeCode.environmentVariables` form and the shell-export form, which refer to the key printed above rather than repeating it — the key appears exactly once. `ANTHROPIC_AUTH_TOKEN`, not `ANTHROPIC_API_KEY`: the connect page's own default, immediate precedence without an interactive approval, and this gateway reads bearers only.
- **`--write`** merges that block into the Claude Code settings file for `--scope`: `~/.claude/settings.json` (`user`, the default) or `./.claude/settings.local.json` (`project`), never a project's committed `.claude/settings.json`. It owns `env.ANTHROPIC_BASE_URL`, `env.ANTHROPIC_AUTH_TOKEN`, `env.CLAUDE_CODE_GATEWAY_HINT_HEADERS` (and the discovery key with `--discovery`; a later write without the flag leaves that key in place), overwrites them idempotently (a changed URL is reported after the write as `replaced ANTHROPIC_BASE_URL <old> → <new>`, the old value reduced to `scheme://host` — or `a different value` when it does not parse or carries userinfo, a query or a fragment; the key is never printed), preserves every other key (numbers as written), and writes mode `0600` by a temp file and rename (the directory is created `0700` when absent). A file that sets `env.ANTHROPIC_API_KEY` or a top-level `apiKeyHelper` is refused naming the file and the key — this gateway never reads `x-api-key`, so leaving either yields a 401 the probe cannot explain — unless `--replace-credential`, which deletes exactly those keys in the same write. The two settings files that are not the target (of `~/.claude/settings.json`, `./.claude/settings.local.json`, `./.claude/settings.json`) are read, read-only, and a credential source in any of them is a stderr warning (`also sets …`); values are never printed.
- **Both scopes ask git whether the file is tracked**, when `git` is on PATH: `git ls-files --error-unmatch` runs in the file's real directory — every directory symlink resolved, so a `~/.claude` that stow links into a dotfiles repository is seen — and a tracked file is refused with the `git rm --cached` remedy unless `--force`, which writes it with a `TRACKED by git` note. Exit 128 with a `.git` at or above that directory means git could not read the repository (a corrupt `.git`, dubious ownership, `GIT_CEILING_DIRECTORIES`): refused unless `--force`; with no `.git` it is not a repository and the write proceeds. A settings file that is itself a symbolic link is refused unless `--force`, which writes through to the link's target so the link survives. The git child runs without `GIT_DIR`, `GIT_WORK_TREE`, `GIT_COMMON_DIR`, `GIT_INDEX_FILE`, `GIT_OBJECT_DIRECTORY`, `GIT_ALTERNATE_OBJECT_DIRECTORIES` and `GIT_CEILING_DIRECTORIES` from this shell (a hook or `git-*` script exports them, and they would redirect the question to another repository); a bare-repository dotfiles manager that git reaches only through `GIT_DIR` (yadm) is therefore not seen.
- **`--scope project`** writes only when git reports the file ignored: `git check-ignore -q -- .claude/settings.local.json` with the current directory as cwd, which honours `.gitignore` at any level, `.git/info/exclude` and `core.excludesFile` (the file Claude Code itself writes to). Ignored → write. Not ignored → `git ls-files --error-unmatch`: tracked → refuse with the `git rm --cached .claude/settings.local.json` remedy; untracked → refuse with the ignore remedy (`printf '%s\n' '.claude/settings.local.json' >> .gitignore`, or your `core.excludesFile`); exit 128 with no `.git` here or above (not a work tree) → write with a note; exit 128 with one → git could not read the repository → refuse unless `--force`. `git` not on PATH with a `.git` here or above → refuse (coverage cannot be proven); no `.git` → write with a note. `--force` is the one override and always writes, with a note. Fails closed: the connect page says the file must be gitignored and `AGENTS.md` forbids a secret in a committed file.
- **`--check`** sends the connect page's own probe, `POST <url>/v1/messages` with `max_tokens: 1` and the key as `Authorization: Bearer`, using a model no gateway serves so a gateway that does serve the route rejects the model — which proves URL and credential without spending a token. It reads the answer honestly: `404` → this gateway does not serve the Anthropic Messages API yet (plan item 11, gate G8), exit `1`; `401` → the key is not a virtual key for this gateway (the probe sends the bearer itself, so your variable choice is not in play), exit `1`; `200`, or `400` carrying the gateway's own `model_not_found` envelope (`error.code`, [error codes](error-codes.md); the gateway authenticates before it routes, so that 400 proves the credential) → URL and credential are good, exit `0`; a `3xx` → a redirect, not followed (the key never travels to a second host), exit `1`; any other answer, a `400` with another code included → reported with one sanitised line, exit `1`. Anthropic's own error envelope has no `code` member, so the happy path depends on the `/v1/messages` ingress (RFC-1, plan item 11) keeping Kelvran's `code` on its 400s — recorded as a cross-reference in that RFC. A `settings` section follows: the probe did not exercise any settings file — in Claude Code run `/status` and look for the "Auth token or API key" line, and if `ANTHROPIC_API_KEY` is also set, `/config` → "Use custom API key" — plus the read-only conflict scan.

### `connect codex|aider|continue`

Print-only (G32: `--write` arrives only after each tool's own documentation is archived and cited; today it is an unknown flag, exit `2`): `export OPENAI_BASE_URL=<url>/v1`, `export OPENAI_API_KEY=<key>` (the key appears exactly once), one pointer at the tool's own documentation for where it takes an OpenAI-compatible base URL and API key (nothing about these tools is archived in this repository, so the CLI asserts nothing about them), and the gateway fact that under `/v1` only `POST /v1/chat/completions`, `POST /v1/embeddings` and `GET /v1/models` are served — `POST /v1/responses` (the OpenAI Responses API) is a 404, so a tool that defaults to the Responses API must be switched to Chat Completions per its own documentation. Print-only output is exempt from the loopback rule (decision 3); `--url` must still be an http(s) URL without credentials, a query or a fragment.

### Exit codes

| Code | When |
|---|---|
| `0` | Printed or written; a `--check` that proved URL and credential; `-h` |
| `1` | No virtual key in reach, or one outside the bearer alphabet; a refused `--url` (the credential guard for `claude`; for any tool a non-http(s) URL or one carrying credentials, a query or a fragment — the exit `2` host rule is reached only once the guard passes); a settings file that is not a regular file or a JSON object, or sets a conflicting credential without `--replace-credential`; a write git cannot vouch for without `--force` (project scope: not ignored, tracked, `git` absent inside a repository, or a repository git cannot read; both scopes: a tracked file, a repository git cannot read, or a symbolic link); a write error; a `--check` answered 404, 401, a redirect, an unexpected status, or unreachable |
| `2` | Unknown tool or flag (`--write` on `codex`, `aider`, `continue`); a positional argument; `--scope` outside `user`/`project`; `--check` with `--write`; `--scope`, `--replace-credential` or `--force` without `--write`; a `--url` with a path or a host that is not an IP address or hostname |

## In the container image

`/kelvran` is a diagnostic and admin client, not the first-run tool: the image has no shell, `cwd` `/`, no `HOME`, UID 65532, and the documented config mount is a read-only single file. What works as shipped: `docker run --rm --entrypoint /kelvran ghcr.io/kelvran/gateway:<tag> -version`; `docker run --rm --entrypoint /kelvran ghcr.io/kelvran/gateway:<tag> init --dry-run --provider openai > config.yaml` on the host (stdout is the YAML); `docker exec <container> /kelvran doctor --config /config.yaml --strict-env`, which reads the `:ro` mount and the container's own environment — the environment the gateway actually sees, so a missing variable is an error there. `init` without `--dry-run` needs a writable mount and an explicit `--out` (`-u $(id -u) -v "$PWD:/out" … init --out /out/config.yaml`).

## Not available today

- `connect codex|aider|continue --write` — print-only until each tool's own documentation is archived and cited (gate G32); `connect claude --write` is live.
- A Claude Code that actually works through this gateway: `POST /v1/messages` is not served yet (plan item 11, gate G8), which `connect claude --check` reports as a 404.
- `connect --check` for `codex`, `aider` or `continue` (no probe of `GET /v1/models`); a `connect cursor` target — Cursor is an OpenAI-shaped client: point it at `<gateway>/v1` with a virtual key by hand; and turning model discovery off again — a write without `--discovery` leaves the key in place, delete it from the settings file by hand.
- `spend --by model|tool|session`, and any aggregate spend view — wait for the spend ledger (plan item 13d, RFC-2).
- Flags on `keys create` for a key's `rate_limit` block (`burst`, `refill_per_second`, `tpm_*`, `per_model`, `max_concurrent_requests`), `allowed_regions`, `allowed_source_cidrs`, `cache_scope_to_end_user` and `attribution_capture_ids`: set them in `config.yaml` or in the admin API body ([reference](admin-api.md)); a `create --replace` through the CLI drops them.
- `--json` on `init` and `connect` (they have nothing to report as data; `doctor`, `keys`, `status` and `spend` have it).
- A refusal, rather than a warning, on an OAuth token offered as an upstream credential (gate G33).
- Any `init` for providers beyond the five the gateway ships adapters for, and any price the embedded table does not carry: pass `--price` (and `--upstream-model` for bedrock).

## Related

- [Quickstart](../tutorials/quickstart.md) — the manual path `init` replaces, kept as the documented `key_hash` contract.
- [Configuration reference](config.md) — every key `init` writes.
- [Release artifacts](release-artifacts.md) and [Container image](container-image.md) — where the binary ships.
- [Virtual keys and budgets](../how-to/virtual-keys-and-budgets.md) — the by-hand path `kelvran keys create` automates.
- [Compatibility](compatibility.md) — what of the OpenAI surface is honoured, and why Claude Code's native mode is not served, which `connect claude --check` reports.
- [MCP/A2A and Anthropic-Messages status](../explanation/mcp-a2a-status.md) — the record of the `/v1/messages` ingress (plan item 11, RFC-1) that turns that check green.
