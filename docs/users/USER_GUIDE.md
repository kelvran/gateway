# User Guide

Operator-facing "how do I configure and run Kelvran" guide — goes beyond `README.md`'s quickstart, never re-explains *why* something is designed the way it is (that's `ARCHITECTURE.md`/`gateway/ARCHITECTURE.md`/`evals/ARCHITECTURE.md`'s job; this document assumes you've read those or don't need to).

> **Status notice**: `gateway/v0.1.0` and `evals/v0.1.0` are tagged, released, and live — real code, not just intended shape. Each section below is honestly marked real vs. not-yet-implemented, corrected as of 2026-09-04; see `STATUS.md` for the always-current snapshot.

## 1. Before You Start

Decide your topology: local dev (Docker Compose) or production (Kubernetes) — see `docs/operations/DEPLOY.md`. You need at least one upstream LLM provider's credentials before Kelvran does anything useful.

## 2. Provider Credentials

See `docs/operations/PROVIDERS.md` for the full list of supported providers and what each one needs. Secrets handling: environment variables or a secrets manager only, never a committed config file — see `SECURITY.md`.

## 3. Virtual Keys and Budgets

Real and implemented, per `docs/rfcs/2026-09-02-virtual-keys-budgets.md`. Generate a key yourself — Kelvran never generates or stores the raw secret, only its hash:

```bash
openssl rand -hex 32                          # this is the secret — give it to the caller, keep it out of config
printf '%s' '<that secret>' | sha256sum        # this hash goes in config.yaml as key_hash
```

Add it to `config.yaml` under `virtual_keys:`:

```yaml
virtual_keys:
  team-alpha:
    key_hash: "<the sha256 hash from above>"
    budget_usd: 100.0          # optional; omit or 0 for unlimited
    rate_limit:
      burst: 20
      refill_per_second: 10
    allowed_models:            # optional; omit for "every configured model"
      gpt-4o: true
```

Clients authenticate with `Authorization: Bearer <the raw secret>` — never the hash. Budget and rate-limit state are tracked **in memory only** and reset on restart; there is no persistent control-plane store yet (see `STATUS.md`/`DECISIONS.md`). A key that exceeds its budget gets HTTP 429 (distinguishable from a rate-limit 429 only by the error message body, per that RFC's OpenAI-SDK-compatibility rationale); a request for a model outside `allowed_models` gets HTTP 403.

**Not implemented yet:** the "teams" hierarchy (a key inheriting a team's budget/rate-limit ceiling) and live, no-restart key provisioning — both remain flat, single-level, static-YAML-only for now, per that RFC's explicit scope boundary.

## 4. Routing & Failover Configuration

Real for static + weighted routing and a single same-model fallback attempt, per `docs/rfcs/2026-09-04-weighted-routing.md`. Multiple `deployments:` entries sharing one `model` value form that model's routing pool; each gets an optional `weight` (integer, omit or `0` for equal-weight round-robin — today's default):

```yaml
deployments:
  gpt4o-primary:
    model: "gpt-4o"
    provider: "openai"
    upstream_model: "gpt-4o"
    base_url: "https://api.openai.com/v1/chat/completions"
    api_key_env: "OPENAI_API_KEY"
    weight: 2        # gets ~2x the traffic of an equal-priority sibling deployment
  gpt4o-secondary:
    model: "gpt-4o"
    provider: "openai"
    upstream_model: "gpt-4o"
    base_url: "https://api.openai.com/v1/chat/completions"
    api_key_env: "OPENAI_API_KEY_SECONDARY"
    weight: 1
```

Selection is weighted round-robin (`gateway/internal/router`); on an upstream error, exactly one fallback attempt is made to another deployment in the same model's pool — never a retry loop, never a second fallback. **Not implemented yet:** named model *groups* (falling back to a different canonical model, not just a different deployment of the same one), an explicit fallback-chain list, and usage/latency/cost-based selection signals — none of these are in `PRD.md`'s v1 scope.

## 5. Cache Configuration

Real for all three layers. L1 (exact-match) and L2 (normalized-match — outer whitespace trim, Unicode NFC, trailing terminal punctuation strip; deliberately not internal-whitespace collapsing or case-folding, since agent traffic can include pasted code) per `docs/rfcs/2026-09-03-cache-l2-normalized-match.md`; L3-lite (MinHash/shingling lexical near-duplicate matching, entity/number/date hard-gated, never real embedding-based semantic similarity) per `docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md`. The whole `cache:` section is optional — omit it for L1's 5-minute/L2's 75-second/L3's 5-minute TTL defaults, each capacity-bounded (LRU, 10,000 entries):

```yaml
cache:
  ttl_seconds: 300
  max_entries: 10000
  l2:
    ttl_seconds: 75
    max_entries: 10000
  l3:
    ttl_seconds: 300
    max_entries: 10000
```

**Never disable the entity/freshness hard-gate to chase a higher hit rate.** This isn't a suggestion: `AGENTS.md`'s Boundaries section lists this as a hard "Never," specifically because of the CacheAttack finding in `THREAT_MODEL.md` (an 86-90% response-hijack rate against exactly this kind of unguarded semantic cache) — and there is no config knob that weakens it; the hard-gate is unconditional in code, not a setting.

## 6. MCP/A2A Tool Brokering

*(Not implemented yet — v2 per `PRD.md`'s scope.)* Registering an MCP server or A2A agent will go through the same identity/budget objects as outbound LLM routing, per `gateway/ARCHITECTURE.md`'s MCP/A2A Subsystem section. Auth-passthrough is intentionally limited — see `THREAT_MODEL.md`'s Cross-Component MCP/A2A row for why.

## 7. Observability

Real OTel spans on every request (buffered and streaming), per `docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md`: GenAI semantic-convention attributes, `agent_run_id` propagated via W3C Baggage from the first line of the pipeline through to span close, queryable end to end. Optional `telemetry:` config section — omit for the "stdout" default (spans printed locally, nothing shipped anywhere):

```yaml
telemetry:
  exporter: "stdout"   # "stdout" | "otlp" | "none"
  # otlp_endpoint: "localhost:4318"   # only read when exporter: "otlp"
```

See `docs/operations/TELEMETRY.md` for the full SLI/dashboard/alerting picture (that operator-facing layer — dashboards, alerting rules — is not part of `gateway` itself and is tracked separately from this real, shipped span-emission mechanism).

## 8. Running Your First Eval Suite

Real end to end: a `Run` model, JSONL Results Store, `Score` model + persistence, and real Anthropic-backed LLM-judge wiring are all shipped, per `docs/rfcs/2026-09-04-evals-rollout-scheduler.md`/`2026-09-04-evals-llm-judge-provider-wiring.md`/`2026-09-04-evals-score-model.md`.

```bash
# in-process, deterministic scoring against a baked-in expected output:
evals run --suite path/to/suite.json --scores out/scores.jsonl

# real sandboxed execution (Docker) + persisted Run records:
evals rollout --suite path/to/suite.json --results out/runs.jsonl --scores out/scores.jsonl

# either command, scored by a real Anthropic call instead of the deterministic scorer
# (requires ANTHROPIC_API_KEY; pinned default judge model, never a bare alias):
evals run --suite path/to/suite.json --scores out/scores.jsonl --llm-judge

# a pass rate + Wilson CI over persisted Scores, grouped by scorer_type,
# with each group's summed cost_usd (deterministic scores cost exactly $0;
# llm_judge scores carry the real, computed Anthropic API cost):
evals report --scores out/scores.jsonl
```

Never a bare pass rate — every `report` output carries a Wilson confidence interval, per `evals/ARCHITECTURE.md`'s harness-transparency design. v1 scoring is a single LLM-judge with bias mitigations (CoT-forcing, reference-guided grading, judge model always ≠ policy model); the adversarial skeptic-panel upgrade is v2 — don't expect panel-level rigor from the first working version, and this guide won't overclaim it once it exists either.

## 9. Upgrading

See `UPGRADE.md` for the actual migration steps once a breaking change ships in a release (currently empty — none has, though a breaking `evals` CLI change is already on `main` ahead of its next release).

## 10. Troubleshooting

Common misconfigurations will be documented here once there's a real system to misconfigure. For anything not covered, see `SUPPORT.md`.
