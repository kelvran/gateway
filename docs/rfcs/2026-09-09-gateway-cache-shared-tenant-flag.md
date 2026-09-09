# RFC: `SharedAcrossTenants` deployment flag — closing the cross-tenant provider-cache-leakage risk

## Status

Accepted, implemented 2026-09-09.

## Context

A `docs/upgrade-research/cache-next-upgrade-2026-09-09.md` deep-research pass surfaced a genuinely new, previously-undocumented finding: `Deployment` (both the runtime `dataplane.Deployment` and the config-level `controlplane.DeploymentConfig`) has exactly one upstream credential (`APIKey`, or `AccessKeyID`/`SecretAccessKey` for Bedrock) per deployment — never per tenant — and routing (`internal/router`) selects purely by canonical model name with zero tenant awareness at all. `gateway/config.example.yaml`'s own shipped example demonstrates this structurally, not hypothetically: `team-alpha` (which explicitly allows `claude-opus-4`) and `team-beta` (which has no `allowed_models` restriction at all) both resolve to the identical `claude-opus-primary` deployment and its one `ANTHROPIC_API_KEY`.

Since `docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md` made Anthropic/Bedrock system-prompt caching default-ON, this is the default case for any shared deployment, not an edge case. It is structurally identical to a real mechanism a 2026 CacheProbe/SAGAI'26 study measured live against OpenRouter: when multiple tenants share one org-level provider credential, the *provider's own* server-side prompt cache can leak cache-hit/timing signal across those tenants — root-caused specifically to the shared credential, not to any routing-logic bug, and fixed there by BYOK (bring-your-own-key), after which the measured leak signal dropped to noise. `THREAT_MODEL.md`'s existing Cache Information-Disclosure row already names the identical "KeyPooling" pooled-upstream-credentials mechanism, but its mitigation clause only ever covered Kelvran's own L1/L2/L3 cache — the provider's own pass-through cache was an uncovered gap in that row.

The user was asked directly (given the real security-vs-performance tradeoff and the "would real deployments actually share credentials" product judgment involved) and confirmed building a new deployment-scoped flag rather than a documentation-only response or flipping the auto-populate default.

## Design

A new, cleanly-separate field, `SharedAcrossTenants bool`, added to both `controlplane.DeploymentConfig` (parsed from `shared_across_tenants: true`) and the runtime `dataplane.Deployment` — mirroring `DisableCacheControlAutoPopulate`'s own exact dual-homing. Deliberately a new field, never a rename/repurpose of the existing one: `DisableCacheControlAutoPopulate` is a cost/latency opt-out an operator can flip for reasons entirely unrelated to tenancy; `SharedAcrossTenants` is a topology *fact* about credential sharing that other future features may also need to key off. `router.Deployment` (the routing-candidate type) is deliberately untouched — its own doc comment is explicit that it's decoupled from any tenant concept, and routing selection has no legitimate use for this fact.

Composition with the existing flag is centralized in one new method, `Deployment.effectiveCacheControlAutoDisabled() bool`, returning `d.DisableCacheControlAutoPopulate || d.SharedAcrossTenants`. All three real call sites that previously read `dep.DisableCacheControlAutoPopulate` directly (`dataplane.go`'s `callDeployment`; `streaming.go`'s `streamDeployment` and `streamDeploymentBedrock`) now call this method instead — centralizing the OR in one named, tested place rather than inlining it three times, so a future fourth call site can't forget it. `cmd/gateway/main.go`'s `buildPipeline` threads the new config field into the runtime `Deployment` literal alongside the existing one.

## Alternatives considered

**Flipping `DisableCacheControlAutoPopulate`'s default to `true` (opt-in caching instead of opt-out)** — the option the user did not choose. Would be safer by default industry-wide, but reduces the prompt-caching cost/latency benefit for the common single-tenant case unless every operator explicitly opts back in, and doesn't distinguish "this deployment happens to be shared" from "every deployment, shared or not."

**Documentation-only (no code change)** — the option the user did not choose. Cheap, but leaves an operator who does have a shared-credential deployment with no dedicated way to express that fact and get the corresponding protection automatically; they would have to remember to set the unrelated `DisableCacheControlAutoPopulate` flag for the right reason on every shared deployment by hand.

## Verification

`gateway/internal/gateway/dataplane/cache_control_auto_populate_test.go`: new `TestEffectiveCacheControlAutoDisabledComposesBothFields` (table-driven, all 4 combinations of the two source fields) and `TestCallDeploymentSharedAcrossTenantsForcesCacheControlOffEvenWhenNotExplicitlyDisabled` (full-pipeline proof: a `SharedAcrossTenants: true` deployment emits no `cache_control` marker even with `DisableCacheControlAutoPopulate` left at its default `false`). `gateway/internal/gateway/controlplane/config_test.go`: new `TestLoadDeploymentSharedAcrossTenantsUnsetDefaultsToFalse`/`TestLoadDeploymentSharedAcrossTenantsParsesTrue`. `config.example.yaml`'s own real round-trip test (`TestLoadExampleConfig`) re-verified passing after adding the new commented-out worked example on `claude-opus-primary`. Full `go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./... && go-arch-lint check && gofmt -l . && go mod tidy` clean except the same pre-existing, already-documented rootless-Docker environmental failures.
