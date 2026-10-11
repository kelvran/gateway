# Security model

This page explains what the Kelvran gateway defends against, what it deliberately does not, and why the design looks the way it does: why secrets never appear in config or on the admin API, why the admin listener is a separate, tiered surface, why tenant isolation in the cache is structural rather than a filter, and how a vulnerability is reported. It is for operators and security reviewers deciding whether the gateway's posture fits their own threat model. It is not a step-by-step guide; the how-to and reference pages linked throughout carry the exact keys, routes and commands. The authoritative, continuously reviewed detail lives in [THREAT_MODEL.md](../../THREAT_MODEL.md) and [SECURITY.md](../../SECURITY.md).

## What the model protects, and in what order

The threat model applies STRIDE per component across five trust boundaries: tenant to gateway, gateway to upstream provider, gateway to its in-process cache, Evals control plane to sandboxed agent execution, and both deployables to the shared `api/` contract. The cache boundary has no network hop, but it still crosses tenant-isolation logic, so it is treated as a boundary anyway ([THREAT_MODEL.md](../../THREAT_MODEL.md), Methodology; the cache is in-process by decision [0002](../decisions/0002-cache-embedded-in-gateway.md)).

The severity taxonomy in [SECURITY.md](../../SECURITY.md) is deliberately system-specific rather than a generic CVSS scale. P0 is supply chain. P1 is cross-tenant isolation failure: any path by which one tenant's cached data, prompts or completions become visible to another. P2 is an Evals sandbox escape. P3 is privilege escalation such as a virtual-key or budget bypass. P4 is a guardrail or policy bypass that crosses no tenant or sandbox boundary. The ordering is a statement of priorities. Cross-tenant leakage is named as the highest-priority known threat class for this system, and most of the design choices below exist to keep it structurally impossible rather than merely filtered out.

## Three published attack classes shaped the design

The threat model is not a generic disclaimer. Three published 2026 findings are recorded as direct design inputs ([SECURITY.md](../../SECURITY.md), Known Threat Classes; the papers are listed in the [L3 lexical cache RFC](../rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md)).

**CacheAttack** demonstrated an 86% response-hijack rate against similarity-only semantic caches, rising to 90.6% against agentic tool invocation, including a financial-agent case study that triggered an unintended transaction. The gateway's answer is that no semantic hit is ever served on a bare similarity threshold. An entity, number and date hard-gate sits in front of every near-duplicate match, and the lexical layer's similarity floor is calibrated against Jaccard similarity over 3-word shingles, not an embedding distance. The reasoning behind the gate, and the efficiency it costs, is the subject of [Cache gate](cache-gate.md). The project's own agent guidance lists weakening the entity/freshness hard-gate in favour of a bare similarity threshold as a standing 'never' (AGENTS.md, Boundaries).

**KeyPooling** found cross-tenant prompt-cache leakage exploitable in 5 of 5 production-representative gateways, with pooled upstream credentials as the mechanism. The gateway's answer is that the tenant namespace is baked into the cache partition itself and enforced at every hop. `tenantID` is a parameter of every `Cache` method (`gateway/internal/cache/port.go`), the L1 and L2 keys fold the virtual-key ID into the hash, and the L3 lexical cache keeps a separate LRU bucket per tenant so that `Search` only ever scans one tenant's own entries. The tenant unit is the virtual key. By default every end user behind one key shares its partition, so a key that fronts many end users should set `cache_scope_to_end_user`, which folds the caller-supplied `X-Kelvran-End-User-Id` header into the L1/L2 partition and, when the header is missing, gives that request a scope nothing else can ever hit rather than falling back to key-wide sharing (`gateway/internal/identity/identity.go`). The in-process implementation's own comment states the trade-off: total memory scales with active-tenant count times the per-tenant cap, accepted because "true partitioning is a security requirement, not a style choice" (`gateway/internal/cache/inprocess/lexical.go`). The provider-side half of KeyPooling, where one upstream credential is shared across tenants, is addressed by a separate flag described in the [shared-tenant cache RFC](../rfcs/2026-09-09-gateway-cache-shared-tenant-flag.md).

**An eval-sandbox escape** through a package-registry-proxy zero-day is the third input. Today the real mitigation on the Evals side is the Docker execution boundary and a full network egress block. Package-registry-proxy hardening and cross-sandbox isolation are named as unbuilt rather than claimed.

A fourth class, bugs in MCP/A2A gateways, appears in both documents as a design input for a subsystem that has no shipped code. See [MCP and A2A status](mcp-a2a-status.md).

## Why no secret ever lives in the config file

The gateway separates two kinds of credential and treats each according to who issued it.

Credentials the gateway must *verify* are its own virtual keys. The config stores only `key_hash`, the hex-encoded SHA-256 digest of the bearer secret; a virtual key without one is a load error. The raw token is compared at request time by `identity.Verifier`, never stored. The [virtual-keys RFC](../rfcs/2026-09-02-virtual-keys-budgets.md) explains the choice under "Why hashes, not env-var names": a virtual key is a credential Kelvran itself issues, so a one-way digest is enough and nothing needs to be recoverable. An example, using a hash published in [config.example.yaml](../../gateway/config.example.yaml) whose plaintext is public and must not be used for anything real:

```yaml
virtual_keys:
  team-alpha:
    key_hash: "6701a1ecc6b08958fa24e13f267aac7233d47f390e92e71f8cc8fb3144672cf1"
```

Credentials the gateway must *present* to a provider are someone else's secret. These are referenced by environment-variable name (`api_key_env`, `access_key_id_env`, `secret_access_key_env`, `session_token_env`) or by file path (`api_key_file` and the matching `*_file` siblings), never inline. A Bedrock deployment therefore looks like this, and contains no secret:

```yaml
deployments:
  claude-bedrock:
    model: "claude-bedrock"
    provider: "bedrock"
    upstream_model: "anthropic.claude-3-5-sonnet-20241022-v2:0"
    base_url: "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20241022-v2:0/converse"
    access_key_id_env: "AWS_ACCESS_KEY_ID"
    secret_access_key_env: "AWS_SECRET_ACCESS_KEY"
    session_token_env: "AWS_SESSION_TOKEN"
    region: "us-east-1"
```

The file variants exist for rotation. An environment variable is fixed at process start, so a rotated secret can never reach a running gateway through it. A `*_file` path is re-read periodically by `RunCredentialReloadLoop`, which is what lets a Kubernetes projected Secret update or an STS session refresh land without a restart. `session_token_env` in turn exists so an operator can replace a long-lived IAM keypair with a short-lived STS triple at zero code cost; the policy's stated goal is to eliminate the long-lived credential, not just rotate it. Procedures: [Provider credentials](../how-to/provider-credentials.md) and [Rotate credentials](../how-to/rotate-credentials.md).

This discipline has a payoff on the admin surface. `GET /admin/config` encodes the whole `Config` struct as JSON with no redaction, and that is safe precisely because the struct holds env-var names, file paths and key hashes, never values. The one admin route that can disclose a raw secret is `pprof`, which reads process memory; it is off by default (`admin.enable_pprof`) and gated to the Admin tier alone.

The policy is also written from experience. The same live AWS access key reached coding-agent transcripts twice, on 2026-09-13 and 2026-09-14, through `docker compose config` printing a fully interpolated environment. The mitigations that followed are `make config-safe`, a redacting wrapper in the root `Makefile`, and the STS session-token path above ([SECURITY.md](../../SECURITY.md), Security Best Practices).

## Why the admin surface is a separate, tiered listener

The admin API can create, delete and rotate virtual keys, change deployment weights and manage prompts. A leaked admin credential that provisions an unlimited-budget, no-allowlist key is the elevation-of-privilege case the threat model names explicitly. Four decisions follow from that ([admin API RFC](../rfcs/2026-09-05-gateway-admin-api.md)).

**Isolation.** The admin surface is its own `*http.Server` on its own port, never a path on the client-facing mux. It does not exist unless `admin.token_env` is set. When it is, the default listen address is `127.0.0.1:8081`; reaching it from another host requires a deliberate `listen_addr`. The process refuses to start if any configured tier's environment variable resolves empty, so an unauthenticated admin server cannot come up by accident (`gateway/cmd/gateway/main.go`). Tokens are compared with `subtle.ConstantTimeCompare`.

**Risk tiering, not role names.** There are four credential tiers, each a distinct environment variable: Admin, Viewer, CostViewer and Operator. The split follows the blast radius of a route rather than a job title. Reads (`GET /admin/config`, `/admin/audit`, `/admin/virtual_keys`, `/admin/prompts` and their siblings) accept Admin or Viewer. `GET /admin/virtual_keys/{name}/spend` additionally accepts CostViewer, a credential a finance consumer can hold without seeing topology, prompt content or rate limits. Operator authenticates only the reversible, single-resource writes: `POST .../rotate`, `POST /admin/deployments/{name}/weight` and `POST /admin/cache/erase`. Everything irreversible, cross-tenant or secret-disclosing stays Admin-only: virtual-key create and delete, prompt writes and label changes, `POST /admin/backup`, and `pprof`. The [RBAC risk-tiering RFC](../rfcs/2026-09-20-gateway-admin-rbac-risk-tiering.md) records why a narrower write tier was added and why it stops at exactly those three routes. Route-by-tier detail: [Admin API reference](../reference/admin-api.md) and [Admin API RBAC](../how-to/admin-api-rbac.md).

**Audit every route, including reads.** Every authenticated admin request, reads included, writes a structured log line naming which tier authenticated, never the credential itself. The two exceptions are `GET /admin/audit`, which reads the trail rather than appending to it, and the `pprof` routes; a request rejected at the bearer check is not logged at all. Reads were added to the audit trail after a backlog audit observed that reading the entire topology and every key's budget shape left no trace. An optional durable JSONL copy (`admin.audit_log_path`) is queryable through `GET /admin/audit`; both paths are governed by the single `admin.enable_audit_log` switch.

**mTLS as an additive layer.** `admin.mtls` turns the admin server into a TLS server that requires and verifies a client certificate against an operator-supplied CA. It uses `RequireAndVerifyClientCert`, not an if-given mode, so a client without a valid certificate fails the handshake rather than skipping a check. It sits on top of bearer tokens, never instead of them. All three PEM paths must be set together, and the config names paths, never certificate material. [DECISIONS.md](../../DECISIONS.md) names the gap under its 2026-09-15 entry and records the closure (commit `d55764db`) under its 2026-09-23 entry.

## Per-request defenses in the data plane

Every chat request passes the same checks in the same order: bearer verification against the key hash, then an optional per-key source-IP CIDR allowlist, then the key's model allowlist. The CIDR check reads the connection's remote address only. It deliberately does not consult `X-Forwarded-For`, because a client-supplied header is spoofable; an operator behind a proxy must restrict at the proxy instead.

Request shape is bounded before any upstream call. The body cap is 32 MiB. A `response_format` JSON schema is bounded to 32 nesting levels and 10,000 JSON tokens by `adapter.ValidateResponseFormatSchema`, using a streaming token count so the check never pays the parsing cost it exists to prevent. THREAT_MODEL.md's Gateway Denial-of-Service row named this gap on 2026-09-15 and records that it was closed the same day. Every deployment's `base_url` must be `https://` unless `allow_insecure_http` is set for that deployment (the escape hatch exists for a self-hosted localhost or dev backend), and a deployment can pin its own CA and client certificate instead of sharing the global transport.

Error bodies returned to clients are redacted. A provider's non-2xx response becomes `upstream provider returned status N`; the raw body, which for Bedrock can name an AWS account ID and IAM role ARN, stays in the gateway's own structured logs. One exception, decided in RFC-1 §9 and recorded in `THREAT_MODEL.md`'s Tampering row: on `/v1/messages`, an `anthropic` deployment's own `400`/`422` whose body is Anthropic's error object is relayed as sent — no ids, keys or ARNs there, but Anthropic's wording can name the deployment's `upstream_model` and the operator's account state. On a streamed turn the deployment's frames are relayed as sent, an in-band `error` frame of any class included once a frame has been relayed; an error frame, or an undecodable frame, before any relayed frame is redacted and the turn may fall back (slice S11b2). Transport-level failures (a refused connection, a timeout, a mid-stream decode error) are redacted the same way to `upstream call failed for model "<model>"` so an internal `base_url` and `host:port` never reach a tenant; this is in place since `gateway/v0.18.0`. Codes and shapes: [Error codes](../reference/error-codes.md).

## What guardrails are, and are not

The guardrail engine scans for PII, secrets and prompt-injection patterns using regex and checksum detectors, with two optional detectors alongside: Amazon Bedrock Guardrails and `embedsim`, an embedding-similarity detector that catches paraphrases the exact-phrase regex misses. It is not a content-moderation layer. The NIST AI 600-1 crosswalk in THREAT_MODEL.md names dangerous, violent, hateful, obscene and abusive content, and model-output toxicity, as unbuilt, and explains that building them would duplicate the upstream provider's own safety tuning. Post-call enforcement also differs by path: on the buffered path a blocked verdict refuses delivery; on the streaming path, where bytes are already with the client, the post-call check is audit-only and only prevents the cache write.

## Trade-offs the project accepted on purpose

**Prompts are global, not tenant-scoped.** Server-side prompt templates are operator-managed config in the same category as deployments and price tables. Any virtual key can resolve any `prompt_id`, and there is no ownership concept in `internal/prompt`. The [prompt-management RFC](../rfcs/2026-09-13-gateway-prompt-management.md) resolved this fork explicitly, and SECURITY.md records it as a disclosed instance of the P1 class rather than a bug. The consequence for operators is a rule, not a knob: never embed tenant secrets, PII or confidential business logic in a shared template.

**Per-agent-run sub-limits inside one key were rejected.** Nesting a limit under a parent key would require trusting a client-supplied run identifier, which the gateway treats as spoofable. The research behind this found no production gateway that does it safely. The supported answer is to issue a dedicated virtual key per team, agent or run. See [Virtual keys and budgets](../how-to/virtual-keys-and-budgets.md).

**The L3 cache has no erase path.** `POST /admin/cache/erase` removes one known L1/L2 entry; `LexicalCache` has no `Delete`, so an L3 entry leaves only when its 300-second default TTL expires.

**The admin audit log never expires on its own.** Both the log lines and the durable JSONL file are indefinite with no record-level erasure. SECURITY.md discloses this as deliberate accountability data and names a possible legal basis an operator may invoke, while stating that it is not a legal conclusion. The interim data-subject procedure is in [DATA-SUBJECT-REQUESTS.md](../operations/DATA-SUBJECT-REQUESTS.md).

## Not available today

- MCP/A2A brokering. No `gateway/internal/mcp` package exists; the threat model's rows for it describe a future subsystem.
- General content moderation of prompts or completions.
- Output sanitization before a response reaches the calling application (OWASP LLM10:2026), disclosed as a real gap.
- A fleet-wide retry budget or global circuit breaker.
- Record-level erasure for the admin audit log, and any erasure for L3 cache entries.
- Detection of a leaked virtual key by an external secret-scanning partner; operators must run a generic scanner such as Gitleaks or TruffleHog themselves.
- Per-agent-run sub-limits nested inside a key.
- A bug bounty.

## Supply chain and compliance evidence

CI runs `govulncheck` and `pip-audit` for dependency CVEs, CodeQL for both languages, `gosec` and `bodyclose` for Go, ruff's `S` ruleset for Python, Dependabot and the OpenSSF Scorecard. The container image runs as UID 65532, and the published image carries a cosign keyless signature, a CycloneDX SBOM and SLSA Build Level 2 provenance that any consumer can re-verify ([RELEASE.md](../../RELEASE.md)). The provider and data-flow inventory is [PROVIDERS.md](../operations/PROVIDERS.md).

None of this is a SOC 2 report, an ISO 42001 certification or a formal AI-policy document. SECURITY.md is explicit that such attestations belong to an operating organization with named leadership, which an open-source project structurally is not; the artifacts above are the evidence an operator points an auditor at, not a substitute for the operator's own audit.

## Reporting a vulnerability

Reports go through GitHub Security Advisories on the gateway repository. There is no e-mail channel; the address once listed was never activated and was removed rather than left as a dead contact. SECURITY.md commits to acknowledgement within 3 business days and a fix or mitigation plan within 14 days for Critical and High severity. Supported versions follow the window in [VERSIONING.md](../VERSIONING.md): the latest minor receives all fixes, the previous minor receives security fixes for 90 days after the newer minor ships. A report should name the component (Gateway, Cache or Evals), the version as gateway/vX.Y.Z or evals/vX.Y.Z, a minimal reproduction and an impact assessment.

Related pages: [Architecture](architecture.md), [Cache gate](cache-gate.md), [Design decisions](design-decisions.md), [Configuration reference](../reference/config.md).
