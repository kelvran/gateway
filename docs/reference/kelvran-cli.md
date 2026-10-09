# kelvran CLI

`kelvran` is the gateway's companion binary (`gateway/cmd/kelvran`), shipped beside `kelvran-gateway` in every release archive and package and at `/kelvran` in the container image. It turns the by-hand first run — generate a secret, hash it, author `config.yaml` — into one command. On `main` since 2026-10-10, not in `gateway/v0.17.0`: the first `gateway/v*` tag pushed after that date is the first release that carries it. Design: [RFC: `kelvran` CLI and single-user mode](../rfcs/2026-10-09-gateway-kelvran-cli-and-single-user-mode.md).

This page is the public surface [`docs/VERSIONING.md`](../VERSIONING.md) binds to for the CLI: every verb, flag, exit code and output shape below is covered by the compatibility policy once it ships in a tagged release.

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
- The CLI never inspects a credential's value beyond whether it is set, except for the one prefix check described under `init`, and never prints one it read. The secrets it *generates* (a virtual key, an admin token) are printed exactly once, as a shell export line.

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
| `admin` | none (so the coming `keys` and `status --config` will work offline against the file; `spend` and the live columns of `status` will need the admin API) | `token_env: "KELVRAN_ADMIN_TOKEN"` and `persist_path`: `--persist-path` (must be absolute), or `<directory of --out>/kelvran-identity.db`, or `/var/lib/kelvran-gateway/identity.db` when `--out` resolves under `/etc/kelvran-gateway/` (the package layout's `StateDirectory`); `--no-persist` omits it with a warning that admin-created keys will not survive a restart. `--persist-path` and `--no-persist` are usage errors under `--single-user` |
| `telemetry` | `exporter: "none"`, preceded by a comment explaining that without it spans and a 60 s metrics dump would share stdout with the JSON logs | same |
| Admin token | not generated | generated and printed once as `export KELVRAN_ADMIN_TOKEN=…` for the gateway's shell |

### Output

Without `--dry-run`: `Wrote <absolute path> (mode 0644; it holds no secret).` followed by a **Next steps** block on stdout — `kelvran-gateway -config '<--out>' -validate` (the path is single-quoted; `--out` may not contain a quote or a line break, and the listen host must be an IP address or hostname, so no printed line carries shell syntax), the admin-token export in team mode, `kelvran-gateway -config …`, then the client exports: the generated secret exactly once as `export KELVRAN_KEY=<secret>`, and `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN="$KELVRAN_KEY"` and `OPENAI_BASE_URL`/`OPENAI_API_KEY="$KELVRAN_KEY"` referencing it. The base URLs come from `listen_addr` with an empty or wildcard host replaced by `127.0.0.1` (`http://127.0.0.1:8080` and `http://127.0.0.1:8080/v1`). The block also notes that the exports belong in the client's shell (an `OPENAI_API_KEY` export in the gateway's shell would replace the upstream credential an openai deployment reads), that `POST /v1/responses` is not served, that Claude Code's native mode needs `POST /v1/messages` which the gateway does not serve yet, that a pasted export persists in shell history (`read -rs` keeps it out), under `--single-user` that `spend` and `status`'s health columns will need an `admin` section, and, whenever exactly one deployment was written, `One model is configured (<model>); rerun with --models a,b to serve more.`

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

## In the container image

`/kelvran` is a diagnostic and admin client, not the first-run tool: the image has no shell, `cwd` `/`, no `HOME`, UID 65532, and the documented config mount is a read-only single file. What works as shipped: `docker run --rm --entrypoint /kelvran ghcr.io/kelvran/gateway:<tag> -version`; `docker run --rm --entrypoint /kelvran ghcr.io/kelvran/gateway:<tag> init --dry-run --provider openai > config.yaml` on the host (stdout is the YAML). `init` without `--dry-run` needs a writable mount and an explicit `--out` (`-u $(id -u) -v "$PWD:/out" … init --out /out/config.yaml`).

## Not available today

- `doctor`, `keys`, `connect`, `status` and `spend` — the remaining Stage 1 verbs of RFC-3, which follow in later commits.
- `--json` output (every read verb gains it when the read verbs land; `init` has none to give).
- A refusal, rather than a warning, on an OAuth token offered as an upstream credential (gate G33).
- Any `init` for providers beyond the five the gateway ships adapters for, and any price the embedded table does not carry: pass `--price` (and `--upstream-model` for bedrock).

## Related

- [Quickstart](../tutorials/quickstart.md) — the manual path `init` replaces, kept as the documented `key_hash` contract.
- [Configuration reference](config.md) — every key `init` writes.
- [Release artifacts](release-artifacts.md) and [Container image](container-image.md) — where the binary ships.
- [Virtual keys and budgets](../how-to/virtual-keys-and-budgets.md) — adding more keys by hand until `kelvran keys` lands.
