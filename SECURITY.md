# Security Policy

## Reporting a Vulnerability

Report privately via [GitHub Security Advisories](https://github.com/kelvran/gateway/security/advisories/new) on the relevant deployable's repository (primary channel). If that's not accessible, email `security@kelvran.dev` (to be activated once the domain is registered — see `DECISIONS.md`'s open naming action).

Please include: affected component (Gateway/Cache/Evals/MCP-A2A), affected version, a minimal reproduction, and your assessment of impact. We do not require PGP-encrypted reports.

**Acknowledgement / resolution targets** (real and contractual as of `gateway/v0.1.0`/`evals/v0.1.0`, tagged 2026-09-03 — the "first real release" condition below has been met): acknowledgement within 3 business days; a fix or mitigation plan communicated within 14 days for Critical/High severity.

## Vulnerability Severity Taxonomy

Adapted to this system's three actual attack surfaces (not a generic CVSS-only scale):

| Severity | Definition |
|---|---|
| **P0 — Supply chain** | Compromise of a dependency, build pipeline, or published package that could inject malicious code into a Kelvran release |
| **P1 — Cross-tenant isolation failure** | Any path by which one tenant's cached data, prompts, or completions become visible to another tenant (see `THREAT_MODEL.md`'s Cache STRIDE table — this is the highest-priority known threat class for this system) |
| **P2 — Sandbox escape** | An Evals rollout escapes its execution sandbox, or breaches isolation between concurrent rollouts |
| **P3 — Privilege escalation** | Virtual-key/budget bypass, MCP/A2A auth-passthrough abuse, guardrail bypass on non-tool operations |
| **P4 — Guardrail/policy bypass** | Content-safety or PII-guardrail circumvention that doesn't cross a tenant or sandbox boundary |

## Supported Versions

| Deployable | Supported |
|---|---|
| `gateway` | Latest minor release only, until a formal support-window policy exists (tracked for `RELEASE.md`) |
| `evals` | Latest minor release only, same caveat |

## Scope

**In scope**: `gateway/`, `evals/`, the shared `api/` contract, and the MCP/A2A brokering subsystem.

**Out of scope**: vulnerabilities in upstream LLM providers themselves (OpenAI/Anthropic/Gemini/Bedrock/self-hosted inference engines) — report those to the provider directly; vulnerabilities requiring physical access to infrastructure the operator controls; social-engineering attacks against maintainers.

## Known Threat Classes We Actively Defend Against

This system was designed with three specific, published 2026 attack classes in mind — not a generic disclaimer, an actual design input (full detail: `THREAT_MODEL.md`):

| Threat class | Published finding | Where it's mitigated |
|---|---|---|
| Semantic-cache response hijacking | "CacheAttack" — up to 86-90.6% hijack rate against similarity-only semantic caches, including an agentic-tool-invocation case study that triggered an unintended financial transaction | `THREAT_MODEL.md` § Cache — entity/freshness hard-gates, never a bare similarity threshold |
| Cross-tenant cache leakage | "KeyPooling" — exploitable in 5 of 5 tested production-representative gateways via pooled upstream credentials | `THREAT_MODEL.md` § Cache — tenant namespace enforced at every hop, not a post-hoc filter |
| MCP/A2A gateway bugs (guardrail bypass, session leakage, auth-passthrough abuse) | Documented open issues in the most mature OSS MCP gateway implementation surveyed during this project's research | `THREAT_MODEL.md` § Cross-Component MCP/A2A |
| Eval sandbox escape via untrusted dependency proxies | A real 2026 incident: an agent escaped its eval sandbox via a package-registry-proxy zero-day, coordinating with other agents to breach a production database | `THREAT_MODEL.md` § Evals — real today: `--network=none` full egress block + the Docker execution boundary. Package-registry-proxy hardening and cross-sandbox isolation are unbuilt and, as of 2026-09-04, correctly named as such rather than claimed |

## Known Limitations & Non-Goals

Dated, honest — updated as the system evolves rather than left as boilerplate:

- `2026-09-02`: Pre-scaffolding. No code exists yet, so no vulnerability disclosure is possible against a running system — this document describes the intended security posture, to be verified against the actual implementation once Phase 0 (per each deployable's `ARCHITECTURE.md`) ships.
- `2026-09-05` (corrected — stale since the 2026-09-03 release): the line above no longer describes reality. `gateway/v0.1.0`/`evals/v0.1.0` shipped 2026-09-03 with real attack surface a report could target today — virtual keys/budgets (`internal/identity`/`internal/budget`), the live-mutable Admin API (`internal/admin`, per `docs/rfcs/2026-09-05-gateway-admin-api.md`), and the Evals sandbox executor (`evals/rollout/sandbox.py`). This document's own severity taxonomy and threat-class table above are real, verified security posture, not aspirational — see `THREAT_MODEL.md` for the current, actively-reviewed detail.
- Kelvran does not currently support (and has no near-term plan to support) a hosted/managed offering — self-hosting is the only deployment model, per `PRD.md`'s non-goals.
- `2026-09-04`: The Evals sandbox's threat mitigations are narrower than this document previously implied — see the corrected `THREAT_MODEL.md` § Evals table row above. Package-registry-proxy hardening, cross-sandbox isolation, scoped per-tool credentials, and audit-trail-tied-to-a-trace are all unbuilt; splitting these into separate future RFCs, not fixed in one pass, per that document's own change log.
- `2026-09-15`: Server-side prompt/template management (`docs/rfcs/2026-09-13-gateway-prompt-management.md`) shipped with prompts deliberately GLOBAL, not tenant-scoped, per that RFC's own resolved design fork — any virtual key may resolve any `prompt_id`, with no ownership/authorization concept anywhere in `internal/prompt`. This is a direct instance of the **P1** threat class above (that class's own definition already names "prompts" explicitly, predating this feature) — see `THREAT_MODEL.md`'s Gateway Information Disclosure row for the full detail. Not a bug: it's the accepted, disclosed cost of the global-config design. Operator guidance below.

## Security Best Practices for Operators

- Never store upstream provider API keys in Kelvran's own config files in plaintext — use environment variables or a secrets manager, per this project's global coding-security conventions (see `AGENTS.md`).
- **Added 2026-09-14, real and recurring, not hypothetical**: never run `docker compose config` (or any command that prints a fully-resolved/interpolated environment) against a real `.env`/`config.yaml` — it prints every real secret in cleartext, including AWS/provider credentials. This exact mechanism has exposed the same live AWS access key twice during this project's own development (2026-09-13, 2026-09-14), with no rotation between occurrences and no technical control in place to prevent a third. If you must inspect resolved Compose config for debugging, scope it to a single non-secret key, or redact known secret env-var names before viewing the output (`make config-safe`, per `AGENTS.md`'s own Gotchas entry). If a credential is ever exposed this way — to a terminal, a screen-share, a CI log, or an AI coding agent's own transcript — rotate it immediately; do not assume a private/local context makes exposure safe. See `THREAT_MODEL.md`'s `2026-09-14` Change Log entry and `AGENTS.md`'s Gotchas section for the full incident history.
- **Corrected 2026-09-18**: eliminate the long-lived Bedrock credential, don't just rotate it — `config.yaml`'s `deployments.*.session_token_env` (already a real, shipped, optional field for every `provider: "bedrock"` deployment) lets you populate `.env` with a short-lived AWS STS session triple (`aws sts get-session-token`/`assume-role`) instead of a permanent IAM user's static keypair, per AWS's own current guidance that access keys should be replaced, not rotated. This requires zero Kelvran code changes — only a credential-generation step you run against your own AWS account. See `docs/upgrade-research/secrets-management-lifecycle-2026-09-15.md` Finding 1 for the full rationale, and `terraform/iam-access-analyzer/` for a complementary, different hygiene control (catching *forgotten*, not leaked, credentials) for whatever long-lived keys you can't eliminate this way.
- **Added 2026-09-25**, found missing from this list by a 26-agent production-readiness audit despite being real, shipped operator controls since 2026-09-23 (the 8-phase upgrade round's own Phase 2): a virtual key can be restricted to a CIDR allowlist of client source IPs (`AllowedSourceCIDRs`, unset = unrestricted, never trusts a spoofable `X-Forwarded-For` by default); a self-hosted deployment's `base_url` must be `https://` unless you explicitly set `AllowInsecureHTTP` for a legitimate localhost/dev case (fails closed at config-load time); and a specific self-hosted backend can be pinned to its own CA/client-cert pair (`TLSConfig`) instead of sharing the global transport. None of these are on by default — use them for any deployment where source-IP restriction or backend-specific TLS trust is part of your own threat model.
- Issue least-privilege virtual keys per team/agent, not one shared key across an entire organization.
- Restrict network exposure of the Gateway's admin API to a private network or VPN; it is not designed to be internet-facing.
- Terminate TLS at or before the Gateway — plaintext prompt/completion traffic should never traverse an untrusted network segment.
- Never embed tenant-specific secrets, PII, or confidential business logic in a shared prompt template (`internal/prompt`) — prompts are global config, not tenant-scoped, so any virtual key can resolve and indirectly probe any prompt's content by design (see the `2026-09-15` entry above).
- **Added 2026-09-23**, per `docs/upgrade-research/api-key-abuse-anomaly-detection-2026-09-22.md` Finding 1: GitHub's secret-scanning partner program (which auto-detects and reports a leaked OpenAI/Anthropic/Google API key in a public repo to the issuing provider within seconds, triggering that provider's own automatic revocation) covers Kelvran's *upstream* provider credentials, but has no equivalent for a Kelvran-issued virtual key — `VirtualKey.KeyHash` is a Kelvran-native secret format no external partner program's regex patterns recognize, so a leaked virtual key would only ever be caught by Kelvran itself, or not at all. If you commit configs, scripts, or logs to a public (or even shared-private) repository, run a generic secret-scanner (Gitleaks, TruffleHog) against it yourself for the virtual-key shape Kelvran issues — this is an operator action, not something Kelvran's own admin API can currently do on your behalf.
- **Added 2026-09-23**, per `docs/upgrade-research/multi-agent-fanout-concurrency-composition-2026-09-23.md`: if one integration's agent runs fan out into many parallel sub-calls, a single runaway or misbehaving run can consume that entire virtual key's own `MaxInFlight`/RPM/TPM/budget allowance, starving any OTHER concurrent use of the same key (a different agent run, or ordinary traffic) — extending the "least-privilege virtual keys per team/agent" guidance above with the specific case fresh, dedicated research confirms has no safer automated fix industry-wide: no production LLM gateway safely nests a per-agent-run sub-limit inside a parent key's own allowance, because doing so would require trusting a client-supplied run identifier, which every gateway surveyed (and Kelvran's own `agent_run_id`) treats as spoofable and unsafe for enforcement, not just as a Kelvran gap. If different agent runs sharing one integration need isolation from each other, issue each run its own virtual key rather than sharing one key across unrelated runs — every existing cap already enforces independently per key, with zero new configuration needed. `GET /admin/virtual_keys/{name}/inflight` (Admin/Viewer credential) surfaces a live, per-agent-run breakdown of one key's current in-flight count — self-reported and never used for enforcement, but useful for spotting "most of this key's concurrency is one agent run" before deciding whether to split it out.

## Provider & Data-Flow Inventory

See `docs/operations/PROVIDERS.md` for exactly which upstream providers receive what data, under what auth mechanism.

## Compliance Evidence for Operators

**Added 2026-09-14**, per `docs/upgrade-research/ai-compliance-regulatory-readiness-2026-09-14.md` Finding 2. SOC 2 is an attestation an *operating organization* produces via an independent audit of its own controls over time — it is not a property that attaches to open-source software itself, and Kelvran the project has no such organization to certify. If your own organization needs to represent your Kelvran deployment's controls to an auditor or customer (for your own SOC 2, ISO 27001, or an equivalent framework), the artifacts already maintained in this repository are the evidence to point at, not a substitute for your own audit:

- `THREAT_MODEL.md` — STRIDE/OWASP-LLM-Top-10/NIST-AI-600-1 crosswalk, actively reviewed
- This document's own severity taxonomy and disclosure process (above)
- `SECURITY-INSIGHTS.yml` — machine-readable OpenSSF Security Insights metadata
- The published container image's cosign signature, CycloneDX SBOM, and SLSA Build Level 2 provenance (`RELEASE.md`'s verification section)
- `docs/operations/PROVIDERS.md` — the provider/data-flow inventory, including the cross-border-transfer-safeguard disclosure added alongside this section

None of these artifacts constitute a SOC 2 report, an ISO 42001 certification, or a formal AI-Policy governance document on their own — those require an operating organization with named leadership and a management-review cadence to certify against, which this OSS project structurally is not. Pursue them only once a real operator or customer requires that specific attestation, not speculatively ahead of one.

## Data Retention & Right to Erasure

**Added 2026-09-18**, per `docs/upgrade-research/data-retention-right-to-erasure-2026-09-15.md`. Kelvran itself is
almost never the GDPR/CCPA-obligated party — that's typically the organization operating a deployment — but the
operator's ability to comply is gated entirely on what this software actually does. Retention windows below are
disclosed provisional defaults (mirroring the same honest framing `docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md`
§4 already uses for its own 90-day S3 lifecycle rule), not numbers derived from a specific compliance requirement.

| Store | Default retention | Configurable? | Erasure mechanism |
|---|---|---|---|
| Cache L1 (exact-match) | 300s (`cache.ttl_seconds`) | Yes | `POST /admin/cache/erase` (see below) or wait out the TTL |
| Cache L2 (normalized-match) | 75s (`cache.l2.ttl_seconds`) | Yes | Same as L1 |
| Cache L3-lite (lexical near-duplicate) | 300s (`cache.l3.ttl_seconds`) | Yes | **None** — `POST /admin/cache/erase` deliberately does not cover this layer (no `Delete` method exists on `LexicalCache` today); only the TTL removes an L3 entry |
| Budget-spend history (`internal/budget`, persisted via `budget.persist_path` when configured) | Indefinite — no automatic expiry/rolling-deletion exists | No automatic window; per-key deletion is real (see below) | `DELETE /admin/virtual_keys/{name}` — `budget.Tracker.Delete` removes both the live in-memory record and, via `boltstore.Store.Delete`, the persisted bbolt record too |
| Admin audit log (`gateway/internal/admin`, `slog.Info` lines) | Indefinite by design (see below), bounded only by whatever process captures stdout | Yes — `admin.enable_audit_log: false` disables it entirely | **None** at the record level (only whole-log disablement) |
| Admin audit trail, durable copy (`internal/admin/auditstore`, added 2026-09-25) | Indefinite — an append-only JSONL file with no built-in rotation or size cap; unlike the `slog.Info` line above (bounded only by whatever external process captures stdout), this is a specific on-disk file Kelvran itself writes and never prunes | Governed by the same `admin.enable_audit_log` switch as the `slog.Info` line above — disabling it disables both paths together | **None** at the record level, same limitation as the `slog.Info` line above; unlike that line, an operator wanting erasure here must manage the file directly — no `Delete`/`Rotate`/`Prune` method exists on `auditstore` today |

**Cache erasure**: `POST /admin/cache/erase` (Admin-tier) services a per-request erasure against L1/L2 for one
specific request shape — the caller must already know the original request's defining fields, since Kelvran's
cache is keyed by a content hash, not a per-tenant index. See `dataplane.Pipeline.EraseCacheEntry`'s own doc
comment for the full mechanism and its L3 limitation.

**Audit log retention basis (disclosed default posture, not a legal determination)**: the admin audit log's
lack of automatic expiry is deliberate, not an oversight — an administrative action log (who changed a virtual
key, a deployment weight, a prompt, when) serves this software's own accountability/incident-investigation
purpose in a way that plausibly falls under UK GDPR Article 17(3)(b)/(e) (compliance with a legal obligation;
establishment, exercise, or defence of legal claims) as a real, available basis for declining an erasure
request against it. **This is disclosed as an available basis an operator may invoke, not a settled legal
conclusion** — per the research doc's own Finding 2, that exemption "must be affirmatively claimed and
documented" with a genuine statutory basis specific to the invoking organization's own jurisdiction and
circumstances. Confirm this applies to your own deployment (or adopt a different basis/retention policy)
before relying on it; this is not legal advice.

See `docs/operations/DATA-SUBJECT-REQUESTS.md` for the manual, interim data-subject-request procedure across
all three stores, including what today's tooling can and cannot do.

## Bug Bounty

Not yet adopted. Tracked as a future decision, not a current commitment.

## Contact

General questions about this policy: open a GitHub issue. Vulnerability reports: use the reporting channel above, not a public issue — this separation exists specifically so a live vulnerability is never disclosed publicly before a fix ships.
