# Configure routing and failover

This page shows how to spread one client-facing model across several deployments, weight and canary that traffic, probe deployment health, fall back by error class, and cap a shared deployment's own load. It is for the operator who edits the gateway's `config.yaml`. Every key below is a `deployments.<name>.*` key or the top-level `health_probe` block, all annotated in [`gateway/config.example.yaml`](../../gateway/config.example.yaml); the full key reference is [config.md](../reference/config.md).

Use this when one upstream is not enough: you want a second provider or region behind the same model name, a cheaper self-hosted copy to prefer, a canary to ramp, or a different model to catch context-window and content-policy errors.

## Prerequisites

- A gateway binary. From the repository: `cd gateway && go build -o /tmp/kelvran-gateway ./cmd/gateway`. See [quickstart.md](../tutorials/quickstart.md) for the other install paths.
- Credentials for every deployment you add, named as environment variables (`api_key_env`, or the Bedrock `access_key_id_env` / `secret_access_key_env` pair). See [provider-credentials.md](provider-credentials.md).
- For the live weight change under Variants: an admin server (`admin.token_env` is required; `admin.listen_addr` is optional and defaults to `127.0.0.1:8081`; `admin.operator_token_env` optionally adds the operator tier, which may also call the weight route). See [admin-api-rbac.md](admin-api-rbac.md).
- The config parser reads a YAML subset: scalars and 2-space-indented mappings, no lists. Every ordered value on this page is a comma-separated string.

## Steps

### 1. Build the pool

Give several deployments the same `model`. They form that model's routing pool. Each member keeps its own `provider`, `upstream_model`, `base_url` and credentials, so one client-facing name can fan out across providers. Every member of a chat pool is billed at the alias's single `price_table` rate. Embedding pools (`kind: embedding`) must share one `provider` and one `upstream_model`; a pool that mixes either fails config load.

```yaml
deployments:
  gpt4o-primary:
    model: "gpt-4o"
    provider: "openai"
    upstream_model: "gpt-4o"
    base_url: "https://api.openai.com/v1/chat/completions"
    api_key_env: "OPENAI_API_KEY"
  gpt4o-selfhosted:
    model: "gpt-4o"                          # same model = same pool
    provider: "openaicompat"
    upstream_model: "gpt-4o-compatible"
    base_url: "https://vllm.internal/v1/chat/completions"
    api_key_env: "VLLM_API_KEY"
```

Selection is smooth weighted round-robin over the members sorted alphabetically by deployment name (file order is not preserved). Two equal-weight members alternate, the alphabetically first one first.

### 2. Weight it

Add `weight` to each member. Omitted or `0` means `1`. A negative weight fails config load.

```yaml
  gpt4o-primary:
    weight: 9
  gpt4o-selfhosted:
    weight: 1
```

Over ten picks, nine go to `gpt4o-primary` and one to `gpt4o-selfhosted`.

### 3. Prefer the cheaper tier

Add `cost_tier` (lower is cheaper, above 0) to every member of the pool. The router then picks from the lowest tier that has a healthy member and falls through to the next tier when every cheaper member is unhealthy, or for the turns a cheaper member's recovery ramp or latency de-weighting withholds. The filter is strict opt-in: if one member has no tier, the whole pool ignores tiers. A negative tier fails config load.

```yaml
  gpt4o-primary:
    cost_tier: 2
  gpt4o-selfhosted:
    cost_tier: 1                             # preferred while healthy
```

`cost_tier` is static operator configuration. It is not read from `price_table` and not learned from traffic.

### 4. Run a sticky canary

Set `sticky: true` on the canary side only; if every member is sticky there is no split and the pool uses plain weighted selection. The first pick for each request is bucketed by the calling virtual key's ID with a monotonic threshold hash, so a key stays on its side across calls. Raising the canary's weight only adds keys to the canary; it never moves a key back. A sticky pick still has to pass the health, recovery-ramp, latency de-weighting and cost-tier checks; when it fails them, the request takes the normal weighted pick instead. Fallback re-picks are never sticky.

```yaml
  gpt4o-selfhosted:
    weight: 1
    sticky: true                             # canary side
```

### 5. Turn on health probing

Probing is off until the top-level `health_probe.interval_seconds` is above 0. Each tick sends one synthetic request per deployment, concurrently, with a 5 s timeout: a 1-token chat completion whose user text is `ping`, or for a `kind: embedding` deployment a one-string embedding (on main since 2026-10-08, not in gateway/v0.17.0). The first pass runs after the first interval, not at startup, and a never-probed deployment counts as healthy. Probes bypass auth, cache, guardrails, budgets and rate limits. Each probe is a real upstream call that the provider can charge for; Kelvran attributes it to no virtual key and does not count it in its own cost accounting.

```yaml
health_probe:
  interval_seconds: 300          # <= 0 or omitted = probing disabled
  unhealthy_threshold: 3         # defaults shown
  healthy_threshold: 2
  recovery_ramp_steps: 4
  recovery_ramp_initial_percent: 20
```

- A deployment leaves the pool after `unhealthy_threshold` consecutive failed probes and returns after `healthy_threshold` consecutive successes.
- On return its effective weight starts at `recovery_ramp_initial_percent` and reaches 100 after `recovery_ramp_steps` more successful probes. A failure during the ramp restarts it. Values above 100 are clamped to 100.
- While a deployment is unhealthy its probe gap doubles after every probe that leaves it unhealthy (a success short of `healthy_threshold` included), up to 8x the interval, and resets once it recovers.
- Probe latency softly de-weights slower members: an EMA (alpha 0.3) of each member's probe round-trip is compared with the fastest peer in the same pool. A real upstream 503 on a client request de-weights that member to the 10 percent floor, the lowest share the latency signal can impose, until the next successful probe recomputes the factor.
- If every member of a pool is unhealthy, selection fails open and still returns a member rather than "no deployment". A single-member pool is always served. In this state requests do reach a failing upstream; `/readyz` (see Verify) is the signal that reports it.

### 6. Add error-classified fallback chains

`fallback_chains` maps an error class to an ordered, comma-separated string of other deployment names. Three classes exist: `content_policy`, `context_window_exceeded` and `generic`. Any other class fails config load. Every target must be a configured deployment; an unknown name fails startup and `-validate`. Targets may serve a different model.

`claude-opus-primary` and `gemini-flash-primary` below are deployments in `config.example.yaml`; define them before this step, or with only the two-member pool from step 1 keep just the `generic` line.

```yaml
  gpt4o-primary:
    fallback_chains:
      context_window_exceeded: "claude-opus-primary"
      content_policy: "claude-opus-primary,gemini-flash-primary"
      generic: "gpt4o-selfhosted"           # catch-all for 5xx/429/timeouts and any unlisted class
```

How a failed call resolves its chain:

- The class is a lowercase keyword heuristic over the upstream error body: context-window keywords are checked first, then content-policy keywords, else `generic`. Gemini's 200-OK safety block is `content_policy`. Capacity, network and adapter errors are `generic`.
- The chain for that class is used. If none is configured, the `generic` chain is used. If neither exists, that deployment gets no fallback for the request. Opting into chains never reverts to the single re-pick described under Variants.
- Each hop is skipped when the target is unhealthy, the key's RPM for the target's model is exhausted, the target is at its own capacity (step 7), it cannot honor `response_format`, it is outside the key's `allowed_regions`, or its model is outside the key's `allowed_models`.
- A short jittered pause (25 ms base, 400 ms cap) precedes each hop after the first. Three consecutive real failures stop the walk for that request.

### 7. Cap a shared deployment's own load

`deployments.<name>.rate_limit` caps the deployment itself, aggregated across every virtual key and every fallback hop that lands on it. This is separate from a virtual key's own `rate_limit` (see [virtual-keys-and-budgets.md](virtual-keys-and-budgets.md)).

```yaml
  gpt4o-primary:
    rate_limit:
      burst: 500
      refill_per_second: 200
      max_concurrent_requests: 50
  claude-bedrock-primary:
    rate_limit:
      tpm_capacity: 3000000
      tpm_refill_per_second: 50000
      tpm_accounting:
        output_token_multiplier: 5          # 5 Claude <= 4.7, 10 Sonnet 5 / Opus 5(.5) / Fable 5.1, 15 Claude 4.8
        exclude_cache_read_tokens: true
```

- `burst` and `refill_per_second` are the deployment's requests-per-minute ceiling. Set both or neither; a half-set pair fails config load. Both omitted means no ceiling.
- `tpm_capacity` and `tpm_refill_per_second` are its tokens-per-minute ceiling, reserved before and reconciled after every call to it. Enforced since gateway/v0.17.0 (2026-10-07); a config that set them earlier starts limiting at that version.
- `tpm_accounting` requires the TPM pair, else config load fails. `output_token_multiplier` (at least 1; unset means 1) weights completion tokens the way a provider quota does. `exclude_cache_read_tokens: true` drops cache-read tokens. The weighted count is prompt tokens, minus cache reads when excluded, plus completion tokens times the multiplier.
- `max_concurrent_requests` bounds in-flight calls to this deployment, checked per hop.
- A rejection is HTTP 503 with a `Retry-After` header, never the per-key 429. The JSON envelope that carries `code: deployment_capacity_exceeded` is on main since 2026-10-08, not in gateway/v0.17.0. Inside a chain the rejection is a `generic` failure and the walk moves to the next hop. Health probes are never gated by these ceilings. See [error-codes.md](../reference/error-codes.md).
- All three ceilings are in-memory per gateway instance, even when the per-key limiter is backed by Redis.

### 8. Validate, then start or restart

Adding or removing a deployment, or changing any field other than `weight`, needs a restart. The one live exception is credential contents: a deployment using the `*_file` credential fields picks up a rewritten file without a restart (see [rotate-credentials.md](rotate-credentials.md)); `*_env` credentials remain restart-only.

```bash
/tmp/kelvran-gateway -validate -config config.yaml
```

Prints `config is valid` and exits 0. A fallback target that is not a configured deployment exits 1 with, for example, `config error: deployment "gpt4o-primary" fallback_chains.generic names "gpt4o-secondary", which is not a configured deployment`. `-validate` does not resolve environment variables.

## Variants

### Change a weight without a restart

```bash
curl -sS -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:8081/admin/deployments/gpt4o-selfhosted/weight \
  -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"weight": 3}'
```

Prints `204`. The admin or operator tier may call it; a negative weight is `400`; an unknown name is `404`. The change is in-memory and reverts to `config.yaml` on restart. With `config_propagation` configured, other replicas receive it. See [admin-api.md](../reference/admin-api.md).

### Fail over without chains

Without `fallback_chains`, an upstream error gets exactly one re-pick in the same pool, excluding the failed deployment and filtered to members that can honor `response_format` and sit inside the key's `allowed_regions`. If no member is eligible, the original error is returned. The same deployment is never retried.

### Streaming requests

Fallback, by chain or by single re-pick, happens only before the first chunk reaches the client. After the first chunk no hop is attempted and the stream ends with the error (delivered as an in-band SSE error frame on main since 2026-10-08, not in gateway/v0.17.0). See [streaming.md](streaming.md).

## Verify it worked

Readiness per model (`listen_addr` is `:8080` in the example config):

```bash
curl -sS -i http://127.0.0.1:8080/readyz
```

Returns `200` with `{"models":{"gpt-4o":true},"ready":true}` while every model has a healthy member, and `503` with `"ready":false` and that model set to `false` when one does not. This is only a real signal once probing is on and the first pass has run: with `health_probe` omitted, or during the first `interval_seconds` after startup, every deployment counts as healthy and `/readyz` is always `200`. `/healthz` stays a liveness check and returns `{"status":"ok"}` either way.

On the gateway's stdout log:

- `health_probe_deployment_unhealthy` and `health_probe_deployment_recovered`, each with a `deployment` field, mark every state change.
- `admin_deployment_weight_updated` with `name`, `weight` and `authorized_by` follows a live weight change.
- Each `chat_completion` line's `gatewayevents_v1` field carries `fallback_happened`, `fallback_from_deployment` and `fallback_reason` for the first abandoned deployment. The deployment that served the request is the `kelvran.deployment.name` attribute on the request's OTel span; each failed hop is a `fallback_hop` span event with `kelvran.fallback.hop.error_class` and `kelvran.fallback.hop.duration_ms`. See [metrics-and-logs.md](../reference/metrics-and-logs.md) and [TELEMETRY.md](../operations/TELEMETRY.md).

## Not available today

- Traffic-derived outlier detection or circuit breaker (Envoy-style). Only synthetic probes and probe-latency de-weighting feed health; real request failures do not mark a deployment unhealthy.
- Latency or cost signals learned from real request traffic, other than the single real-503 de-weight in step 5. Everything else is probe-derived or static config.
- A distributed (Redis) deployment rate, TPM or concurrency limiter. Each replica enforces its own ceiling.
- First-class cross-model "model groups". Naming another model's deployment inside `fallback_chains` is the only way to cross models.
- Per-deployment pricing. `price_table` is keyed by canonical model, so every member of a pool is billed at one rate.
- Live add or remove of a deployment, or a live change to any field other than `weight`.
- Transfer of a virtual key's TPM reservation to a fallback hop's model. The key's TPM is reserved once against the requested model; its RPM is re-checked per hop.

## Related

- [config.md](../reference/config.md) for every key on this page; [data-plane-api.md](../reference/data-plane-api.md) for `/readyz` and `/healthz`.
- [architecture.md](../explanation/architecture.md) and [FAILURE-MODES.md](../operations/FAILURE-MODES.md) for what happens when an upstream or Redis is down.
- [structured-output.md](structured-output.md) for the `response_format` capability check that gates first picks and hops; [troubleshooting.md](troubleshooting.md) for `503` and `/readyz` failures.
- Design records: [weighted routing](../rfcs/2026-09-04-weighted-routing.md), [active health probing](../rfcs/2026-09-07-gateway-active-health-probing.md), [probe backoff](../rfcs/2026-09-08-gateway-health-probe-backoff.md), [error-classified fallback chains](../rfcs/2026-09-07-gateway-error-classified-fallback-chains.md), [retry-storm mitigation](../rfcs/2026-09-07-gateway-retry-storm-mitigation.md).
