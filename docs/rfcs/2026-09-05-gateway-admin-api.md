# RFC: Admin API v1 slice (`/internal/admin`)

## Status

Accepted, implemented 2026-09-05.

## Context

`gateway/internal/admin` has been a documented-but-unbuilt package since the initial scaffolding — `gateway/ARCHITECTURE.md`'s Package Layout marks it `NOT BUILT`, and `docs/rfcs/2026-09-02-virtual-keys-budgets.md`'s own Unresolved Questions already named the real gap: "no live/no-restart key provisioning... `/internal/admin`'s 'declarative config, live no-restart mutation' remains unbuilt." Every config value today is loaded exactly once (`controlplane.Load` → `buildPipeline`, both called once in `main.go`'s `run()`) — adding, removing, or updating a virtual key requires editing `config.yaml` and restarting the process.

This RFC ships the v1 slice a prior backlog-planning pass had already recommended without designing in detail: read-only config introspection, plus exactly one config section made live-mutable — virtual keys, the specific gap the existing RFC named.

## Design

### Scope, deliberately narrow

1. `GET /admin/config` — read-only introspection of the full loaded `*controlplane.Config`.
2. `POST /admin/virtual_keys/{name}` (upsert) and `DELETE /admin/virtual_keys/{name}` — the only live-mutable section in v1.
3. Every other config section (guardrails, budgets' global shape, rate limits, routing/deployments, cache, price table, telemetry) stays static-YAML-only in v1 — named explicitly as later follow-on work, not solved here.

### Why `Config` is safe to return wholesale

`controlplane.Config` never holds a raw secret — confirmed by reading the struct, not assumed: `DeploymentConfig.APIKeyEnv`/`AccessKeyIDEnv`/etc. are environment variable *names*, not values (the actual secret is resolved from `os.Getenv` only inside `buildPipeline`, after `Config` has already been fully parsed); `VirtualKeyConfig.KeyHash` is a SHA-256 digest, not the bearer token itself (per `internal/identity`'s own design — a virtual key is a credential Kelvran issues, so config only ever needs to verify a match, never recover the raw secret). `GET /admin/config` therefore marshals the real, already-loaded `*controlplane.Config` directly to JSON with no redaction step, since there is nothing in it to redact.

### Mechanism: `atomic.Pointer[identity.Verifier]`, not a config reload

`dataplane.Pipeline.verifier` changes from `*identity.Verifier` to `atomic.Pointer[identity.Verifier]`. `identity.Verifier` is itself immutable once constructed (its `NewVerifier` builds a map once and never mutates it), so a virtual-key mutation is implemented as: build a new full key list (current keys, from a new `(*Verifier).Keys()` accessor, plus the one being added/updated/removed), construct a brand-new `*Verifier` via the existing `identity.NewVerifier`, and atomically `Store` it — every in-flight request that already loaded the old pointer finishes against the old, consistent key set; every new request sees the new one. No lock, no partial-update window inside a single request.

`identity.NewVerifier([]VirtualKey{})` already refuses to construct a Verifier with zero keys (`"at least one virtual key is required"`) — reused directly as the guard against deleting the last remaining virtual key, rather than inventing a second check: `Pipeline.DeleteVirtualKey` simply lets `NewVerifier`'s existing error surface, wrapped with `ErrCannotDeleteLastVirtualKey` context, rather than locking every client out of the gateway with no remaining way back in via this same API.

### A real correctness hazard found by tracing the code, not assumed: the rate limiter

The plan that recommended this v1 slice sketched only the Verifier swap. Tracing `dataplane.Pipeline.checkRateLimit`'s actual call (`p.limiter.Allow(ctx, vk.ID)`, passing only the ID — no burst/refill parameters at call time) into `ratelimit.KeyLimiter.Allow` surfaced a real, previously-latent bug class this feature is the first thing to actually trigger: in the default in-memory mode, `l.buckets[keyID]` on an ID with no pre-built bucket returns a `nil *TokenBucket`, and `TokenBucket.Allow()` immediately dereferences `b.mu` — a nil-pointer panic, not a graceful rejection. In Redis mode, an unregistered ID's `l.configs[keyID]` returns a zero-valued `KeyConfig{Capacity: 0, RefillPerSecond: 0}`, silently rate-limiting the new key to zero forever rather than panicking — a different failure, equally wrong. Both are unreachable today only because `identity.Verifier` and `ratelimit.KeyLimiter` are always built from the exact same config slice, in lockstep, at the same `buildPipeline` call — this feature is the first thing that can make them diverge.

Fixed by giving `KeyLimiter` a new exported `Register(cfg KeyConfig)` method (thread-safe — `KeyLimiter` gained a `sync.RWMutex`, since its maps were previously build-once/read-only and are now live-mutable) that upserts a bucket (in-memory mode) or a config entry (Redis mode) for one key. `Pipeline.UpsertVirtualKey` calls `limiter.Register` **before** swapping the Verifier pointer — so any request resolving through the *new* Verifier can never find an unregistered rate-limit entry underneath it. `budget.Tracker` needed no equivalent fix: `spent map[string]decimal.Decimal` is a value-typed map, and a lookup on an unregistered key ID correctly zero-values to "no prior spend" rather than panicking or misbehaving — verified by reading `Tracker.Allow`/`SpentUSD`/`Record` directly, not assumed by analogy to the rate limiter.

### Auth: a deliberately separate credential scheme

The admin surface uses a single static shared-secret bearer token (`Authorization: Bearer <token>`), read from an environment variable named by `admin.token_env` — never a raw secret in config, matching every other secret-reference convention in this codebase (`DeploymentConfig.APIKeyEnv`). Compared via `subtle.ConstantTimeCompare`, mirroring `internal/identity`'s own timing-safety posture. This is a genuinely separate credential space from client-facing virtual keys — a client's virtual key bearer token must never authenticate against `/admin/*`, and the admin token must never authenticate against `/v1/chat/completions`; `internal/admin` does not import or call into `internal/identity` at all.

If `admin:` is configured but the named environment variable resolves empty, `run()` fails hard at startup — never silently starting an admin server with an empty/bypassable token. This is stricter than the equivalent deployment-API-key convention (a missing deployment key only degrades that one deployment); an admin mutation surface with no real credential behind it is not an acceptable degraded state.

### Never internet-facing by default

The admin server is a **second, separate `http.Server`** on its own listener — never the same mux as `/v1/chat/completions` — started only if the `admin:` config section is present (`admin.token_env` non-empty). Its `listen_addr` defaults to `127.0.0.1:8081` (loopback-only) when the section is present but `listen_addr` is omitted; reaching it from outside the host requires an explicit, deliberate operator choice (a non-loopback address, or a container port mapping), never a default.

### Persistence: none, in v1 — explicit and loud about it

Admin-mutated virtual keys live only in the in-memory `atomic.Pointer[identity.Verifier]` (and the rate limiter's in-memory bucket map). A process restart reverts to exactly what `config.yaml` says — every admin mutation since the last restart is lost. This mirrors `budget.Tracker`'s own pre-persistence default posture, stated here with the same explicitness `docs/rfcs/2026-09-03-budget-persistence.md` used for budgets: a real, deliberate v1 scope limit, not a hidden gap. A future RFC can add a durable store for admin-mutated keys if operators need mutations to survive a restart; not solved here.

## Alternatives considered

**Hot-reloading the whole `config.yaml` on a filesystem watch** — rejected: reintroduces exactly the config's own multi-section validation surface (deployments, price table, guardrails, everything) for a live-mutation need that, per the existing RFC, is specifically about virtual keys, not the whole file. A targeted API for the one section that actually needs live mutation is a smaller, more auditable surface.

**Making `/admin/*` a route on the same mux/port as `/v1/chat/completions`, gated by a different auth header** — rejected: a single shared listener means a misconfigured reverse proxy or firewall rule that intends to expose only `/v1/chat/completions` publicly would also expose `/admin/*` on the same port by construction. A separate listener makes "admin is not supposed to be reachable from wherever the client traffic terminates" the default shape, not something an operator has to remember to enforce with a routing rule.

**Letting `KeyLimiter.Allow` lazily self-register an unknown key with some default burst/refill** — rejected: this would silently apply an implicit default rather than the actual burst/refill the admin API call specified (or the config's), producing a subtly wrong rate limit rather than a loud failure. An explicit `Register` call driven by the same code path that already knows the intended `KeyConfig` is correct by construction, not a guess.

## Verification

New `internal/admin` package tests: every route requires the admin token (401 on missing/wrong/absent), `GET /admin/config` returns the real loaded config as JSON, `POST` upserts a brand-new key (a subsequent real request through the pipeline using that key's raw token succeeds), `POST` updates an existing key's budget/rate-limit/allowed-models (effect observable on the next request), `DELETE` removes a key (its old token is subsequently rejected), `DELETE` on the last remaining key is refused (409). `internal/ratelimit`: `Register` tests proving a never-before-configured ID transitions from a would-be panic/zero-capacity hazard to a working bucket/config entry, in both in-memory and Redis modes. `internal/identity`: `Keys()` round-trips the exact configured set. `internal/gateway/dataplane`: `Pipeline.UpsertVirtualKey`/`DeleteVirtualKey` unit tests, including the last-key-deletion refusal. A real end-to-end `cmd/gateway` integration test: start both listeners for real, `POST` a new virtual key via the admin port, then make a genuine `/v1/chat/completions` request on the main port using that key's raw bearer token and confirm it succeeds — and confirm a client virtual key's own bearer token is rejected against `/admin/config`.
