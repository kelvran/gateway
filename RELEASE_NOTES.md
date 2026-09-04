# Release Notes

Narrative, user-facing notes spanning **both** deployables — written for someone deciding whether to upgrade, not for a mechanical diff. This is explicitly distinct from `gateway/changelog/` and `evals/changelog/`, which are per-deployable, per-version, Keep-a-Changelog-style (Added/Changed/Deprecated/Removed/Fixed/Security) — this file groups by feature area instead, and covers a coordinated release point across whichever deployable(s) actually changed.

First release shipped 2026-09-03. Entries below are sourced directly from `gateway/changelog/0.1.0.md`/`evals/changelog/0.1.0.md` — narrative summary, not a restatement; read those for full mechanical detail.

---

## v0.1.0 — 2026-09-03

Covers: `gateway` v0.1.0, `evals` v0.1.0 — first tagged release of both deployables.

### Gateway Core

**A real, working LLM gateway.** Canonical OpenAI-shaped request/response schema; working OpenAI and Anthropic provider adapters (Gemini/Bedrock/openaicompat stubbed with typed "not implemented" errors); a non-streaming and a real SSE-streaming (`stream: true`) request pipeline; static + weighted routing across multiple deployments of one canonical model, with a single same-model fallback attempt on upstream error; a stdlib-only YAML config loader; a multi-stage Docker build.

### Identity, Budgets & Rate Limiting

**Multi-tenant from day one.** Virtual API keys (hash-matched, never a raw secret stored) with per-key USD budget caps, per-key token-bucket rate limits, and optional per-key allowed-model lists. Rate limiting is in-memory by default, optionally Redis-backed for correctness across multiple gateway instances; budget spend is in-memory by default, optionally persisted (bbolt) across restarts.

### Cost Accounting

**Decimal, not float.** Every cost/budget number — per-token pricing, cumulative spend, logged `cost_usd` — is `decimal.Decimal` arithmetic end to end, never binary floating point, closing a real drift risk a naive `float64` implementation would have.

### Cache

**Three real layers, one hard-gated.** L1 exact-match, L2 normalized-match (a deliberately narrow allowlist: whitespace trim, Unicode NFC, trailing terminal punctuation — never internal-whitespace or case normalization, since agent traffic can include pasted code), and L3-lite lexical near-duplicate matching (MinHash/Jaccard, entity/number/date hard-gated, a volatile-query bypass, a staleness budget) — never real embedding-based semantic similarity in this pass. The entity/freshness hard-gate is unconditional in code, not a config option, per the CacheAttack/KeyPooling findings in `THREAT_MODEL.md`.

### Observability

**Real OTel spans, `agent_run_id` from day one.** GenAI semantic-convention attributes on every request (buffered and streaming), with `agent_run_id` propagated in via W3C Baggage — never fabricated when absent.

### api/ Contract

**`api/gatewayevents/v1`** — the first real cross-language contract: `GatewayDecisionEvent` gives every gateway decision (success or rejection) a structured, queryable `Outcome`, joinable to the same request's OTel span. `buf lint`/`buf breaking` real in CI; generated Go/Python bindings committed and drift-checked. `evals/ingestion/` decodes it end to end — the fixture proving the round-trip was produced by `gateway`'s own real bindings, not hand-authored.

### Evals

**A working eval harness, not just a scorer library.** `EvalCase` model, a real Wilson-CI calculator (never a bare pass rate), a deterministic scorer, an LLM-as-judge scorer with a CoT-forcing prompt (dependency-injected, so tests never need a live key), a Docker-sandboxed rollout wrapper (network-isolated by default), and a CLI (`evals run`, `evals report`).

### Breaking Changes

`config.yaml`'s single static `api_key_env` gateway key is replaced by a `virtual_keys:` mapping (see `docs/users/USER_GUIDE.md` §3) — no migration path, since there was no released version yet to migrate from. `evals`' PyPI distribution name is `kelvran-evals`, not the bare `evals` it briefly was during scaffolding (the importable module and CLI command are both still `evals`, unchanged). See `UPGRADE.md`.

---

<!--
## vX.Y.Z — YYYY-MM-DD

Covers: `gateway` vA.B.C, `evals` vD.E.F

### <Feature Area>

**<Bolded lead-in>.** Narrative prose describing what changed and why it matters to someone running Kelvran, not just what the diff touched. `[gateway]` (#123)

### api/ Contract

Any change to the shared OTel/gatewayevents contract gets its own standing feature area here — cross-links `docs/operations/DEPLOY.md`'s compatibility matrix rather than restating it. `[gateway, evals]` (#124)

### Breaking Changes

Explicit operator-action callouts. Pointer to `UPGRADE.md` for the full migration steps, not restated here.

### Contributors

(Only for releases large enough to warrant it.)
-->
