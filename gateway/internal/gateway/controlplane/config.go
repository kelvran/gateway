// Package controlplane compiles the gateway's static YAML configuration
// into typed Go values: listen address, the gateway's own virtual-key
// environment-variable name, the configured deployments (model ->
// provider/upstream routing), and the static cost price table.
//
// Per AGENTS.md's "Never store secrets in a committed file" rule, no
// secret VALUE ever lives in this config — only the NAME of the
// environment variable that holds it at runtime. Resolving those names
// into actual values happens in cmd/gateway at wiring time, never here.
//
// This pass parses YAML with a minimal, hand-rolled, stdlib-only parser
// (see parseYAMLMini below) rather than a third-party YAML library,
// per the plan's explicit "no third-party Go deps for this pass"
// constraint. It supports exactly the subset this config's shape needs:
// scalar values and nested mappings (no lists, no anchors, no multi-line
// strings) — deployments that would naturally be a YAML list are instead
// modeled as a mapping keyed by a deployment name, so multiple
// deployments can still share one canonical model for round-robin
// routing without requiring list support.
package controlplane

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"
)

// DeploymentConfig is one upstream deployment: a canonical (client-facing)
// model routed to a specific provider/upstream-model/endpoint.
type DeploymentConfig struct {
	// Name uniquely identifies this deployment within the config file.
	// Multiple deployments may share the same Model, in which case the
	// dataplane round-robins across them (per gateway/ARCHITECTURE.md's
	// Request Lifecycle router step).
	Name string
	// Model is the canonical, client-facing model name requests are
	// routed by.
	Model string
	// Provider is the adapter registry key (e.g. "openai", "anthropic").
	Provider string
	// UpstreamModel is the provider-side model identifier sent upstream
	// (which may differ from Model, e.g. a versioned Anthropic model ID).
	UpstreamModel string
	// BaseURL is the full upstream endpoint URL to POST the provider
	// request to.
	BaseURL string
	// APIKeyEnv is the name of the environment variable holding this
	// deployment's upstream provider API key. Never the raw key value.
	// Required for every provider except "bedrock", per
	// docs/rfcs/2026-09-04-bedrock-adapter.md: Bedrock's real
	// authentication is AWS SigV4 request signing, which needs
	// AccessKeyIDEnv/SecretAccessKeyEnv instead of a single bearer secret.
	APIKeyEnv string
	// AccessKeyIDEnv/SecretAccessKeyEnv are the names of the environment
	// variables holding this deployment's AWS access key ID / secret
	// access key. Never the raw values. Required only when Provider ==
	// "bedrock".
	AccessKeyIDEnv     string
	SecretAccessKeyEnv string
	// SessionTokenEnv is the name of the environment variable holding an
	// AWS session token, for temporary/STS-issued credentials. Optional
	// even for "bedrock" deployments — most real deployments use
	// long-lived IAM user credentials with no session token at all.
	SessionTokenEnv string
	// Region is the AWS region SigV4 signing is computed against.
	// Required only when Provider == "bedrock". Not a secret — a plain
	// config value, unlike every *Env field above.
	Region string
	// Weight controls this deployment's share of routing selection among
	// deployments serving the same Model, per PRD.md's "static + weighted
	// routing" v1 scope line and docs/rfcs/2026-09-04-weighted-routing.md.
	// Zero (unset in YAML) means "use the default weight of 1" —
	// normalized in internal/router, not here, matching this file's
	// existing looseness (getInt's silent-swallow-on-malformed-string
	// behavior). A negative value is a config error.
	Weight int
	// CostTier is an optional, OPERATOR-CONFIGURED cost-preference band
	// (lower = cheaper), per DECISIONS.md's [2026-09-12] entry — NOT a
	// learned or usage-derived signal, and NOT read from PriceTable
	// (which is keyed by canonical model, identical for every deployment
	// sharing one Model value; this field is the genuinely new
	// per-deployment signal PriceTable can't express). The real
	// motivating case: one canonical model served by two differently-
	// priced backends (e.g. the same model via Bedrock vs. a direct
	// provider API, or two different negotiated contracts/regions).
	// Zero (unset, the default, and every deployment configured before
	// this field existed) means "no tier configured" — router.Router's
	// tier-preference filtering activates for a model's whole deployment
	// group ONLY when EVERY deployment in that group has an explicit
	// tier set; a group with even one untiered deployment is
	// byte-for-byte unaffected, matching Weight's own "zero means unset,
	// resolve to today's behavior" convention immediately above.
	CostTier int
	// FallbackChains maps an error class (one of
	// fallbackClassContentPolicy/fallbackClassContextWindowExceeded/
	// fallbackClassGeneric below) to an ordered list of OTHER
	// deployments' Names to attempt, in order, when a call to THIS
	// deployment fails with that class of error — per
	// docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md.
	// Empty/nil (the default — the common case, and every config written
	// before this feature existed) means this deployment has not opted
	// into explicit fallback-chain configuration at all; dataplane falls
	// back to its pre-existing, router-based single-fallback behavior
	// for it instead. Parsed from a comma-separated string per class, not
	// a YAML list — see parseYAMLMini's own doc comment for why this
	// file's parser has no list support at all.
	FallbackChains map[string][]string
	// DisableCacheControlAutoPopulate opts this deployment OUT of
	// Kelvran's default auto-population of a CacheControl marker on an
	// otherwise-unmarked system message, per
	// docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md. False
	// (the default, and every deployment configured before this field
	// existed) means auto-populate stays ON. Only the anthropic/bedrock
	// adapters ever read the resulting value (via dataplane.Deployment
	// of the same name) — a no-op, harmless either way, on every other
	// provider.
	DisableCacheControlAutoPopulate bool
	// SharedAcrossTenants declares this deployment's upstream credential
	// is deliberately shared by more than one tenant's virtual key(s),
	// per docs/rfcs/2026-09-09-gateway-cache-shared-tenant-flag.md. When
	// true, forces CacheControl auto-populate off for this deployment
	// regardless of DisableCacheControlAutoPopulate's own value — see
	// dataplane.Deployment.effectiveCacheControlAutoDisabled for the
	// composition. False (the default) means not declared shared.
	SharedAcrossTenants bool
	// MaxConcurrentRequests bounds how many requests may be simultaneously
	// in flight against THIS deployment, aggregated across every virtual
	// key that routes to it -- including via a fallback_chains hop, or
	// the pre-existing single-fallback behavior -- per docs/upgrade-
	// research/gateway-per-deployment-concurrency-2026-09-09.md. A
	// genuinely different scope from VirtualKeyConfig.MaxConcurrentRequests
	// above, which is per-CALLER: many different keys, each comfortably
	// under their own cap, can still converge on one shared deployment
	// with nothing bounding ITS aggregate load. <= 0 (the default, and
	// every deployment configured before this field existed) means
	// unlimited, matching VirtualKeyConfig.MaxConcurrentRequests' own
	// convention.
	MaxConcurrentRequests int
	// RateLimitBurst/RateLimitRefill configure this deployment's own
	// aggregate RPM ceiling, checked and consumed on EVERY call to this
	// deployment (hop 1 or a fallback hop), regardless of which virtual
	// key initiated it. Unlike VirtualKeyConfig.RateLimitBurst/
	// RateLimitRefill, 0 here means "no ceiling at all", never "use the
	// gateway's operational default" -- a deployment's real throughput
	// ceiling is provider/plan-specific, with no principled platform-wide
	// default to substitute. Must be set together or left both unset; a
	// half-set pair is a load-time config error, per parseDeploymentRateLimit.
	RateLimitBurst  float64
	RateLimitRefill float64
	// TPMCapacity/TPMRefillPerSecond configure this deployment's own
	// aggregate tokens-per-minute ceiling. 0 (both, the default) means
	// disabled. Parsed and validated here so the YAML surface doesn't
	// need a second breaking change later, but deliberately NOT wired
	// into the dataplane yet -- correct TPM accounting needs Reserve-
	// then-Reconcile bookkeeping PER HOP (a reservation against a
	// skipped/failed hop must be undone; only the hop that actually
	// served the response reconciles), the same class of complexity
	// docs/upgrade-research/gateway-tpm-permodel-fallback-2026-09-09.md
	// found has no production precedent anywhere. Named future work, not
	// a silent gap.
	TPMCapacity        float64
	TPMRefillPerSecond float64
}

// fallbackClassContentPolicy, fallbackClassContextWindowExceeded, and
// fallbackClassGeneric are the only valid fallback_chains keys a
// deployment may configure — mirroring, but never importing,
// dataplane.FallbackClassContentPolicy/FallbackClassContextWindowExceeded/
// FallbackClassGeneric of the same values: controlplane and dataplane
// are sibling, leaf packages per gateway/ARCHITECTURE.md's dependency
// rules, neither may import the other, so this is a deliberately
// duplicated plain-string convention, not a shared type.
const (
	fallbackClassContentPolicy         = "content_policy"
	fallbackClassContextWindowExceeded = "context_window_exceeded"
	fallbackClassGeneric               = "generic"
)

var validFallbackClasses = map[string]bool{
	fallbackClassContentPolicy:         true,
	fallbackClassContextWindowExceeded: true,
	fallbackClassGeneric:               true,
}

// ModelPriceConfig is the static per-token price for one model.
// Decimal, not float64, per docs/rfcs/2026-09-02-decimal-cost-accounting.md
// — these values are parsed from their original YAML source string via
// getDecimal, never round-tripped through float64 first.
type ModelPriceConfig struct {
	PromptPerToken     decimal.Decimal
	CompletionPerToken decimal.Decimal
	// CacheReadPerToken/CacheCreationPerToken are nil (unset) unless the
	// operator explicitly configured cache_read_per_token/
	// cache_creation_per_token for this model -- see
	// costaccounting.ModelPrice's doc comment for why a pointer, not a
	// bare decimal.Decimal.
	CacheReadPerToken     *decimal.Decimal
	CacheCreationPerToken *decimal.Decimal
}

// VirtualKeyConfig is one statically-configured virtual key, per
// docs/rfcs/2026-09-02-virtual-keys-budgets.md. KeyHash is the hex-encoded
// SHA-256 digest of the actual secret bearer token — never the raw secret
// — since a virtual key is a credential Kelvran itself issues, not a
// third party's credential it must protect on someone else's behalf (see
// that RFC's "Why hashes, not env-var names" section).
type VirtualKeyConfig struct {
	// Name uniquely identifies this key within the config file.
	Name string
	// KeyHash is the hex-encoded SHA-256 digest of the actual secret.
	KeyHash string
	// BudgetUSD is this key's cumulative spending cap. Zero means
	// unlimited. Decimal, not float64 — see ModelPriceConfig's doc comment.
	BudgetUSD decimal.Decimal
	// BudgetResetIntervalSeconds, when positive, makes BudgetUSD a rolling
	// window (e.g. 2592000 for a 30-day "monthly" budget) rather than a
	// lifetime-of-the-process cap. Zero (the default) preserves the
	// original, never-resets behavior exactly. A plain int-seconds field,
	// matching this file's existing TTLSeconds convention (CacheConfig,
	// CacheL2Config, CacheL3Config) rather than a duration string.
	BudgetResetIntervalSeconds int
	// BudgetWarnPercent, when positive, is the fraction of BudgetUSD (e.g.
	// 0.8 for 80%) at which dataplane logs a warning on every billable
	// completion while spend remains at or above it — log-only, per
	// docs/rfcs/2026-09-05-gateway-budget-warn-threshold.md. Zero (the
	// default) disables it. A percentage of BudgetUSD, not a second
	// absolute USD value, so it can't drift out of sync if BudgetUSD is
	// ever changed.
	BudgetWarnPercent float64
	// AllowedModels restricts this key to a subset of configured models.
	// Empty means every configured model is allowed.
	AllowedModels []string
	// RateLimitBurst and RateLimitRefill configure this key's own
	// token-bucket rate limiter. Zero means "use the gateway's default"
	// (resolved by cmd/gateway, not here — this package only parses what
	// the config file says, it doesn't own operational defaults).
	RateLimitBurst  float64
	RateLimitRefill float64
	// TPMCapacity and TPMRefillPerSecond configure this key's optional,
	// separate tokens-per-minute rate-limit dimension, per
	// docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md. Zero (the default)
	// disables it. In-memory rate-limit mode only in v1 — a no-op when
	// rate_limit.redis_addr is configured, named explicitly as future
	// work rather than silently ignored (cmd/gateway logs a startup
	// warning if both are configured together).
	TPMCapacity        float64
	TPMRefillPerSecond float64
	// PerModelRateLimits maps a model name to that model's own, separate
	// RPM (burst/refill_per_second) override — the "consumer x model"
	// dimension of Kong-style multi-dimensional rate-limit matching, per
	// docs/upgrade-research/gateway-2026-09-06.md's Finding 5 and
	// docs/rfcs/2026-09-07-gateway-multi-dimensional-rate-limits.md.
	// Nil/empty (the default, and every config written before this field
	// existed) means no override for any model — every model this key is
	// allowed to call shares its single RateLimitBurst/RateLimitRefill
	// bucket above, exactly as before this feature existed. Bridged into
	// ratelimit.KeyConfig.PerModel by cmd/gateway, never referenced
	// directly by internal/ratelimit, matching this file's existing
	// RateLimitBurst/RateLimitRefill decoupling convention.
	PerModelRateLimits map[string]ModelRateLimitConfig
	// MaxConcurrentRequests bounds how many of this key's requests may be
	// simultaneously in flight, per
	// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md. <= 0 (the
	// default, and every config written before this field existed) means
	// unlimited — bridged into ratelimit.ConcurrencyConfig by cmd/gateway,
	// never referenced directly by internal/ratelimit, matching this
	// file's existing RateLimitBurst/RateLimitRefill decoupling
	// convention. Nested under rate_limit: in YAML, alongside burst/
	// refill_per_second/tpm_capacity — a per-key throughput/capacity
	// control, the same family as those three, even though the mechanism
	// underneath (in-flight count, not a token bucket) is genuinely
	// different.
	MaxConcurrentRequests int
}

// ModelRateLimitConfig is one virtual key's per-model RPM override — see
// VirtualKeyConfig.PerModelRateLimits' doc comment for the full design.
// Burst/RefillPerSecond are required and must be positive for an entry to
// parse at all (see parsePerModelRateLimits) — unlike RateLimitBurst/
// RateLimitRefill, which resolve 0 to the gateway's own operational
// default, a per-model entry has no equivalent "unset means use some
// other default" fallback to resolve to, so a malformed entry is a
// config error, not a silent no-op.
//
// TPMCapacity/TPMRefillPerSecond are the equivalent per-model override
// for the TPM dimension — unlike Burst/RefillPerSecond, this pair is
// OPTIONAL on top of an otherwise-valid entry (an entry may set RPM-only,
// or RPM+TPM together, but never TPM-only, since Burst/RefillPerSecond
// are already mandatory for the entry to exist at all): both must be set
// together or neither, mirroring parseDeploymentRateLimit's identical
// "set together or neither" convention for its own TPM pair. Unset (the
// default, and every config written before this field existed) means no
// per-model TPM override for that model — falls through to the key's own
// key-level default TPM bucket, exactly like RateLimitBurst's own
// top-level "0 means unlimited/default" fallback for the RPM dimension.
type ModelRateLimitConfig struct {
	Burst              float64
	RefillPerSecond    float64
	TPMCapacity        float64
	TPMRefillPerSecond float64
}

// TelemetryConfig configures OTel span export, per
// docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md. The whole section is
// optional — a zero-valued TelemetryConfig (Exporter == "") means
// internal/telemetry.Init applies its own "stdout" default, since that's
// an operational default, not a config-shape concern this package owns.
type TelemetryConfig struct {
	// Exporter is "stdout", "otlp", or "none".
	Exporter string
	// OTLPEndpoint is read only when Exporter == "otlp".
	OTLPEndpoint string
}

// BudgetConfig configures restart-durable budget-spend persistence, per
// docs/rfcs/2026-09-03-budget-persistence.md. Optional — a zero-valued
// BudgetConfig (PersistPath == "") means pure in-memory budget tracking,
// exactly as before that RFC: a bare config.yaml with no budget: section
// must behave identically to before this feature existed.
type BudgetConfig struct {
	// PersistPath is the file path for the bbolt-backed budget store.
	// Empty means no persistence.
	PersistPath string
}

// RateLimitConfig configures distributed (Redis-backed) rate limiting,
// per docs/rfcs/2026-09-03-distributed-rate-limiting.md. Optional — a
// zero-valued RateLimitConfig (RedisAddr == "") means pure in-memory
// per-key token buckets, exactly as before this RFC.
type RateLimitConfig struct {
	// RedisAddr is the Redis server address ("host:port"). Empty means
	// no distributed rate limiting.
	RedisAddr string
}

// CacheL2Config configures the L2 (normalized-match) cache layer, per
// docs/rfcs/2026-09-03-cache-l2-normalized-match.md. Optional — a
// zero-valued CacheL2Config means both fields default (75s TTL, 10,000
// max entries).
type CacheL2Config struct {
	TTLSeconds int
	MaxEntries int
	// JitterFraction, per docs/rfcs/2026-09-10-gateway-cache-ttl-jitter.md.
	// <= 0 (absent) resolves to the real 10% default, mirroring
	// TTLSeconds/MaxEntries's own zero-means-default convention.
	JitterFraction float64
}

// CacheL3Config configures the L3-lite (lexical near-duplicate) cache
// layer, per docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md.
// Optional — a zero-valued CacheL3Config means both fields default (5
// minutes TTL, 10,000 max entries — the same inprocess.LexicalCache
// default as L1/L2, applied per tenant rather than globally, since L3's
// capacity bound is structurally per-tenant; see that package's own doc
// comment for why).
type CacheL3Config struct {
	TTLSeconds int
	MaxEntries int
	// JitterFraction, per docs/rfcs/2026-09-10-gateway-cache-ttl-jitter.md.
	// <= 0 (absent) resolves to the real 10% default, mirroring
	// TTLSeconds/MaxEntries's own zero-means-default convention.
	JitterFraction float64
}

// CacheConfig configures the L1 (exact-match) cache layer and nests L2's
// and L3's own configs, per docs/rfcs/2026-09-03-cache-l2-normalized-match.md
// and docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md.
// Optional — a zero-valued CacheConfig means L1 defaults exactly as
// before those RFCs (5-minute TTL), now also capacity-bounded (10,000
// max entries) rather than truly unbounded — a safety improvement applied
// unconditionally, not gated behind opting in.
type CacheConfig struct {
	TTLSeconds int
	MaxEntries int
	// JitterFraction, per docs/rfcs/2026-09-10-gateway-cache-ttl-jitter.md.
	// <= 0 (absent) resolves to the real 10% default, mirroring
	// TTLSeconds/MaxEntries's own zero-means-default convention.
	JitterFraction float64
	L2             CacheL2Config
	L3             CacheL3Config
}

// GuardrailsConfig configures the guardrail pre-call/post-call content
// checks, per docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md.
// Optional — a zero-valued GuardrailsConfig means the default v1
// detector set and policy run under a default "v1" PolicyVersion.
type GuardrailsConfig struct {
	// PolicyVersion is stamped into every cache write (see
	// cache.Key/NormalizedKey's guardrailPolicyVersion parameter and
	// LexicalCandidate.GuardrailPolicyVersion) — bump it whenever
	// detectors or policy change and a new binary is released, so stale
	// cache entries written under the old policy are forced misses.
	// Empty defaults to "v1" (cmd/gateway's own operational default, not
	// a config-shape concern this package owns).
	PolicyVersion string
	// CategoryOverrides maps a Category string ("credential",
	// "contact_info", etc.) to "block" or "warn", overriding this RFC's
	// default policy for that category. Empty means every category uses
	// the RFC's own default (see guardrail.DefaultPolicy).
	CategoryOverrides map[string]string
}

// AdminConfig configures the optional admin HTTP surface (read-only
// config introspection plus live virtual-key mutation), per
// docs/rfcs/2026-09-05-gateway-admin-api.md. Optional — a zero-valued
// AdminConfig (TokenEnv == "") means the admin server is not started at
// all, matching every other optional subsystem's convention (Redis,
// boltstore, OTel).
type AdminConfig struct {
	// ListenAddr is the address the admin HTTP server binds to, on its
	// own separate net.Listener — never the same mux/port as
	// ListenAddr's client-facing gateway. Empty defaults to
	// "127.0.0.1:8081" (loopback-only) when TokenEnv is set — cmd/gateway's
	// own operational default, not a config-shape concern this package
	// owns, mirroring how TelemetryConfig's Exporter default is resolved.
	ListenAddr string
	// TokenEnv is the name of the environment variable holding the admin
	// bearer credential — never a raw secret in config, matching
	// DeploymentConfig.APIKeyEnv's own convention. Required for the admin
	// server to start at all; if set but the named env var resolves
	// empty, cmd/gateway fails startup rather than running an
	// unauthenticated admin surface.
	TokenEnv string
	// ViewerTokenEnv is the name of the environment variable holding an
	// optional, read-only viewer credential, per
	// docs/rfcs/2026-09-09-gateway-admin-viewer-role.md. A viewer token
	// can authenticate GET /admin/config but never a write route
	// (POST/DELETE /admin/virtual_keys/{name}). Empty means no viewer
	// tier is configured — GET /admin/config then requires the admin
	// token exactly as it always has. If set but the named env var
	// resolves empty, cmd/gateway fails startup, mirroring TokenEnv's
	// own "never run with an unauthenticated surface" rule.
	ViewerTokenEnv string
}

// HealthProbeConfig configures the active/synthetic health-probing
// background loop, per
// docs/rfcs/2026-09-07-gateway-active-health-probing.md. Optional — a
// zero-valued HealthProbeConfig (IntervalSeconds == 0) means health
// probing is disabled entirely, matching every other optional
// subsystem's convention (Redis, boltstore, OTel, admin) — every
// deployment stays unconditionally eligible for router.Router.Select,
// exactly as before this feature existed.
type HealthProbeConfig struct {
	// IntervalSeconds is how often ProbeDeployments runs. <= 0 disables
	// probing entirely — cmd/gateway never starts the background loop.
	IntervalSeconds int
	// UnhealthyThreshold is the number of CONSECUTIVE failed probes
	// required before a deployment is excluded from routing. <= 0
	// resolves to router.HealthConfig's own default (3) — an
	// operational default resolved in internal/router, not a
	// config-shape concern this package owns, mirroring
	// AdminConfig.ListenAddr's own deferred-default convention.
	UnhealthyThreshold int
	// HealthyThreshold is the number of CONSECUTIVE successful probes
	// required before an excluded deployment is re-included. <= 0
	// resolves to router.HealthConfig's own default (2).
	HealthyThreshold int
	// RecoveryRampSteps is the number of ADDITIONAL consecutive
	// successful probes required, after the one that re-includes a
	// deployment, before its routing weight finishes ramping back up to
	// its full configured Weight. <= 0 resolves to
	// router.HealthConfig's own default (4).
	RecoveryRampSteps int
	// RecoveryRampInitialPercent is the effective-weight percentage a
	// just-recovered deployment starts at before ramping linearly up to
	// 100 over RecoveryRampSteps further successes. <= 0 resolves to
	// router.HealthConfig's own default (20).
	RecoveryRampInitialPercent int
}

// Config is the gateway's fully-parsed static configuration.
type Config struct {
	// ListenAddr is the address http.ListenAndServe binds to (e.g. ":8080").
	ListenAddr string
	// VirtualKeys are the configured tenants (see internal/identity).
	VirtualKeys []VirtualKeyConfig
	// Deployments are the configured upstream routes.
	Deployments []DeploymentConfig
	// PriceTable is the static per-model cost table.
	PriceTable map[string]ModelPriceConfig
	// Telemetry configures OTel span export. Optional.
	Telemetry TelemetryConfig
	// Budget configures budget-spend persistence. Optional.
	Budget BudgetConfig
	// RateLimit configures distributed rate limiting. Optional.
	RateLimit RateLimitConfig
	// Cache configures the L1/L2 cache layers. Optional.
	Cache CacheConfig
	// Guardrails configures the pre-call/post-call content checks. Optional.
	Guardrails GuardrailsConfig
	// Admin configures the optional admin HTTP surface. Optional.
	Admin AdminConfig
	// HealthProbe configures the optional active/synthetic health-probing
	// background loop. Optional.
	HealthProbe HealthProbeConfig
}

// Load reads and parses the YAML config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("controlplane: reading config %s: %w", path, err)
	}

	root, err := parseYAMLMini(data)
	if err != nil {
		return nil, fmt.Errorf("controlplane: parsing config %s: %w", path, err)
	}

	cfg := &Config{PriceTable: map[string]ModelPriceConfig{}}

	var ok bool
	cfg.ListenAddr, ok = getString(root, "listen_addr")
	if !ok || cfg.ListenAddr == "" {
		return nil, fmt.Errorf("controlplane: config missing required field %q", "listen_addr")
	}
	virtualKeysRaw, ok := getMap(root, "virtual_keys")
	if !ok || len(virtualKeysRaw) == 0 {
		return nil, fmt.Errorf("controlplane: config must declare at least one virtual key under %q", "virtual_keys")
	}
	for name, raw := range virtualKeysRaw {
		vkMap, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("controlplane: virtual key %q must be a mapping", name)
		}
		vk := VirtualKeyConfig{Name: name}
		vk.KeyHash, _ = getString(vkMap, "key_hash")
		if vk.KeyHash == "" {
			return nil, fmt.Errorf("controlplane: virtual key %q is missing required field %q", name, "key_hash")
		}
		vk.BudgetUSD, _ = getDecimal(vkMap, "budget_usd")
		vk.BudgetResetIntervalSeconds, _ = getInt(vkMap, "budget_reset_interval_seconds")
		vk.BudgetWarnPercent, _ = getFloat(vkMap, "budget_warn_percent")
		if rl, ok := getMap(vkMap, "rate_limit"); ok {
			vk.RateLimitBurst, _ = getFloat(rl, "burst")
			vk.RateLimitRefill, _ = getFloat(rl, "refill_per_second")
			vk.TPMCapacity, _ = getFloat(rl, "tpm_capacity")
			vk.TPMRefillPerSecond, _ = getFloat(rl, "tpm_refill_per_second")
			vk.MaxConcurrentRequests, _ = getInt(rl, "max_concurrent_requests")
			if pm, ok := getMap(rl, "per_model"); ok {
				perModel, err := parsePerModelRateLimits(name, pm)
				if err != nil {
					return nil, err
				}
				vk.PerModelRateLimits = perModel
			}
		}
		if am, ok := getMap(vkMap, "allowed_models"); ok {
			for model, v := range am {
				if enabled, ok := v.(bool); ok && enabled {
					vk.AllowedModels = append(vk.AllowedModels, model)
				}
			}
			sort.Strings(vk.AllowedModels)
		}
		cfg.VirtualKeys = append(cfg.VirtualKeys, vk)
	}
	sort.Slice(cfg.VirtualKeys, func(i, j int) bool { return cfg.VirtualKeys[i].Name < cfg.VirtualKeys[j].Name })

	deploymentsRaw, ok := getMap(root, "deployments")
	if !ok || len(deploymentsRaw) == 0 {
		return nil, fmt.Errorf("controlplane: config must declare at least one deployment under %q", "deployments")
	}
	for name, raw := range deploymentsRaw {
		depMap, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("controlplane: deployment %q must be a mapping", name)
		}
		dep := DeploymentConfig{Name: name}
		dep.Model, _ = getString(depMap, "model")
		dep.Provider, _ = getString(depMap, "provider")
		dep.UpstreamModel, _ = getString(depMap, "upstream_model")
		dep.BaseURL, _ = getString(depMap, "base_url")
		dep.APIKeyEnv, _ = getString(depMap, "api_key_env")
		dep.AccessKeyIDEnv, _ = getString(depMap, "access_key_id_env")
		dep.SecretAccessKeyEnv, _ = getString(depMap, "secret_access_key_env")
		dep.SessionTokenEnv, _ = getString(depMap, "session_token_env")
		dep.Region, _ = getString(depMap, "region")
		if dep.Model == "" || dep.Provider == "" || dep.UpstreamModel == "" || dep.BaseURL == "" {
			return nil, fmt.Errorf("controlplane: deployment %q is missing one of model/provider/upstream_model/base_url", name)
		}
		if dep.Provider == "bedrock" {
			if dep.AccessKeyIDEnv == "" || dep.SecretAccessKeyEnv == "" || dep.Region == "" {
				return nil, fmt.Errorf("controlplane: deployment %q (provider bedrock) is missing one of access_key_id_env/secret_access_key_env/region", name)
			}
		} else if dep.APIKeyEnv == "" {
			return nil, fmt.Errorf("controlplane: deployment %q is missing api_key_env", name)
		}
		dep.Weight, _ = getInt(depMap, "weight")
		if dep.Weight < 0 {
			return nil, fmt.Errorf("controlplane: deployment %q has a negative weight %d", name, dep.Weight)
		}
		dep.CostTier, _ = getInt(depMap, "cost_tier")
		if dep.CostTier < 0 {
			return nil, fmt.Errorf("controlplane: deployment %q has a negative cost_tier %d", name, dep.CostTier)
		}
		if fbRaw, ok := getMap(depMap, "fallback_chains"); ok {
			chains, err := parseFallbackChains(name, fbRaw)
			if err != nil {
				return nil, err
			}
			dep.FallbackChains = chains
		}
		dep.DisableCacheControlAutoPopulate, _ = getBool(depMap, "disable_cache_control_auto_populate")
		dep.SharedAcrossTenants, _ = getBool(depMap, "shared_across_tenants")
		if rl, ok := getMap(depMap, "rate_limit"); ok {
			if err := parseDeploymentRateLimit(name, rl, &dep); err != nil {
				return nil, err
			}
		}
		cfg.Deployments = append(cfg.Deployments, dep)
	}
	// Sort for deterministic ordering (map iteration order is random).
	sort.Slice(cfg.Deployments, func(i, j int) bool { return cfg.Deployments[i].Name < cfg.Deployments[j].Name })

	if telemetryRaw, ok := getMap(root, "telemetry"); ok {
		cfg.Telemetry.Exporter, _ = getString(telemetryRaw, "exporter")
		cfg.Telemetry.OTLPEndpoint, _ = getString(telemetryRaw, "otlp_endpoint")
	}

	if budgetRaw, ok := getMap(root, "budget"); ok {
		cfg.Budget.PersistPath, _ = getString(budgetRaw, "persist_path")
	}

	if rateLimitRaw, ok := getMap(root, "rate_limit"); ok {
		cfg.RateLimit.RedisAddr, _ = getString(rateLimitRaw, "redis_addr")
	}

	if cacheRaw, ok := getMap(root, "cache"); ok {
		cfg.Cache.TTLSeconds, _ = getInt(cacheRaw, "ttl_seconds")
		cfg.Cache.MaxEntries, _ = getInt(cacheRaw, "max_entries")
		cfg.Cache.JitterFraction, _ = getFloat(cacheRaw, "jitter_fraction")
		if l2Raw, ok := getMap(cacheRaw, "l2"); ok {
			cfg.Cache.L2.TTLSeconds, _ = getInt(l2Raw, "ttl_seconds")
			cfg.Cache.L2.MaxEntries, _ = getInt(l2Raw, "max_entries")
			cfg.Cache.L2.JitterFraction, _ = getFloat(l2Raw, "jitter_fraction")
		}
		if l3Raw, ok := getMap(cacheRaw, "l3"); ok {
			cfg.Cache.L3.TTLSeconds, _ = getInt(l3Raw, "ttl_seconds")
			cfg.Cache.L3.MaxEntries, _ = getInt(l3Raw, "max_entries")
			cfg.Cache.L3.JitterFraction, _ = getFloat(l3Raw, "jitter_fraction")
		}
	}

	if guardrailsRaw, ok := getMap(root, "guardrails"); ok {
		cfg.Guardrails.PolicyVersion, _ = getString(guardrailsRaw, "policy_version")
		if overridesRaw, ok := getMap(guardrailsRaw, "category_overrides"); ok {
			cfg.Guardrails.CategoryOverrides = make(map[string]string, len(overridesRaw))
			for category := range overridesRaw {
				if action, ok := getString(overridesRaw, category); ok {
					cfg.Guardrails.CategoryOverrides[category] = action
				}
			}
		}
	}

	if adminRaw, ok := getMap(root, "admin"); ok {
		cfg.Admin.ListenAddr, _ = getString(adminRaw, "listen_addr")
		cfg.Admin.TokenEnv, _ = getString(adminRaw, "token_env")
		cfg.Admin.ViewerTokenEnv, _ = getString(adminRaw, "viewer_token_env")
	}

	if healthProbeRaw, ok := getMap(root, "health_probe"); ok {
		cfg.HealthProbe.IntervalSeconds, _ = getInt(healthProbeRaw, "interval_seconds")
		cfg.HealthProbe.UnhealthyThreshold, _ = getInt(healthProbeRaw, "unhealthy_threshold")
		cfg.HealthProbe.HealthyThreshold, _ = getInt(healthProbeRaw, "healthy_threshold")
		cfg.HealthProbe.RecoveryRampSteps, _ = getInt(healthProbeRaw, "recovery_ramp_steps")
		cfg.HealthProbe.RecoveryRampInitialPercent, _ = getInt(healthProbeRaw, "recovery_ramp_initial_percent")
	}

	if priceRaw, ok := getMap(root, "price_table"); ok {
		for model, raw := range priceRaw {
			priceMap, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("controlplane: price_table entry %q must be a mapping", model)
			}
			promptPer, ok := getDecimal(priceMap, "prompt_per_token")
			if !ok {
				return nil, fmt.Errorf("controlplane: price_table entry %q missing or malformed prompt_per_token", model)
			}
			completionPer, ok := getDecimal(priceMap, "completion_per_token")
			if !ok {
				return nil, fmt.Errorf("controlplane: price_table entry %q missing or malformed completion_per_token", model)
			}
			if promptPer.Sign() < 0 {
				return nil, fmt.Errorf("controlplane: price_table entry %q has a negative prompt_per_token", model)
			}
			if completionPer.Sign() < 0 {
				return nil, fmt.Errorf("controlplane: price_table entry %q has a negative completion_per_token", model)
			}
			entry := ModelPriceConfig{
				PromptPerToken:     promptPer,
				CompletionPerToken: completionPer,
			}
			if cacheReadPer, ok := getDecimal(priceMap, "cache_read_per_token"); ok {
				if cacheReadPer.Sign() < 0 {
					return nil, fmt.Errorf("controlplane: price_table entry %q has a negative cache_read_per_token", model)
				}
				entry.CacheReadPerToken = &cacheReadPer
			}
			if cacheCreationPer, ok := getDecimal(priceMap, "cache_creation_per_token"); ok {
				if cacheCreationPer.Sign() < 0 {
					return nil, fmt.Errorf("controlplane: price_table entry %q has a negative cache_creation_per_token", model)
				}
				entry.CacheCreationPerToken = &cacheCreationPer
			}
			cfg.PriceTable[model] = entry
		}
	}

	return cfg, nil
}

// --- minimal, stdlib-only YAML-subset parser ---
//
// Supports scalar "key: value" lines and nested-mapping "key:" lines
// (value on subsequent, more-indented lines), using 2-space-per-level
// indentation. No lists, no flow style, no anchors/aliases, no
// multi-line strings — this config's shape never needs any of those.

// parseYAMLMini parses data into a tree of map[string]any, where leaf
// values are string or bool. Numeric scalars are deliberately left as
// their raw source string (see parseYAMLScalar's doc comment) — this is
// load-bearing for docs/rfcs/2026-09-02-decimal-cost-accounting.md, not
// an oversight: money fields must parse from the original decimal text
// via getDecimal, never through an intermediate float64.
func parseYAMLMini(data []byte) (map[string]any, error) {
	root := map[string]any{}

	type frame struct {
		indent int
		m      map[string]any
	}
	stack := []frame{{indent: -1, m: root}}

	lines := strings.Split(string(data), "\n")
	for i, raw := range lines {
		line := stripYAMLComment(raw)
		trimmedRight := strings.TrimRight(line, " \t\r")
		if strings.TrimSpace(trimmedRight) == "" {
			continue
		}

		indent := len(trimmedRight) - len(strings.TrimLeft(trimmedRight, " "))
		content := strings.TrimSpace(trimmedRight)

		for len(stack) > 1 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1].m

		colonIdx := strings.Index(content, ":")
		if colonIdx < 0 {
			return nil, fmt.Errorf("line %d: expected \"key: value\" or \"key:\", got %q", i+1, content)
		}
		key := unquoteYAMLScalar(strings.TrimSpace(content[:colonIdx]))
		valueStr := strings.TrimSpace(content[colonIdx+1:])

		if valueStr == "" {
			child := map[string]any{}
			parent[key] = child
			stack = append(stack, frame{indent: indent, m: child})
			continue
		}
		parent[key] = parseYAMLScalar(valueStr)
	}

	return root, nil
}

// stripYAMLComment removes a trailing "# ..." comment. This config's
// values never contain a literal "#", so a naive (non-quote-aware) strip
// is sufficient.
func stripYAMLComment(line string) string {
	if idx := strings.Index(line, "#"); idx >= 0 {
		return line[:idx]
	}
	return line
}

// unquoteYAMLScalar strips a single layer of matching quotes, if present.
func unquoteYAMLScalar(s string) string {
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// parseYAMLScalar interprets a scalar value as a bool, or falls back to a
// (possibly quoted) string — deliberately NOT float64. An earlier version
// of this function eagerly converted numeric-looking scalars to float64
// here, which meant every money value round-tripped through binary
// floating point before getDecimal (or even getFloat) ever saw it,
// silently defeating decimal precision at the exact point money enters
// the system. Numeric scalars now stay as their raw source string;
// getFloat/getDecimal each convert from that string at the point of use,
// with their own precision semantics.
//
// Boolean matching is deliberately an explicit set, not
// strconv.ParseBool: ParseBool also accepts "0"/"1"/"t"/"f" as valid
// booleans, which collided with genuinely numeric scalars — a config
// line like "budget_usd: 1" would silently parse as the bool true, fail
// getFloat/getDecimal's type switch, and fall back to a zero value
// (which internal/budget.Tracker's own convention treats as "unlimited"
// budget) — a real bug found while writing
// docs/rfcs/2026-09-02-decimal-cost-accounting.md, fixed here.
func parseYAMLScalar(s string) any {
	if unquoted := unquoteYAMLScalar(s); unquoted != s {
		return unquoted
	}
	switch s {
	case "true", "True", "TRUE":
		return true
	case "false", "False", "FALSE":
		return false
	}
	return s
}

func getString(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func getFloat(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func getInt(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case string:
		i, err := strconv.Atoi(n)
		return i, err == nil
	default:
		return 0, false
	}
}

// getDecimal reads key as a decimal.Decimal, parsed from its raw source
// string via decimal.NewFromString — never via an intermediate float64,
// per docs/rfcs/2026-09-02-decimal-cost-accounting.md. Used only for
// money fields (price_table entries, budget_usd); non-money numeric
// fields (rate_limit burst/refill) stay on getFloat.
func getDecimal(m map[string]any, key string) (decimal.Decimal, bool) {
	v, ok := m[key]
	if !ok {
		return decimal.Zero, false
	}
	s, ok := v.(string)
	if !ok {
		return decimal.Zero, false
	}
	d, err := decimal.NewFromString(s)
	return d, err == nil
}

// getBool reads key as a bool, per parseYAMLScalar's own explicit
// true/false literal matching (never strconv.ParseBool — see that
// function's doc comment for why). Mirrors getString/getFloat/getInt's
// existing (value, ok) shape.
func getBool(m map[string]any, key string) (bool, bool) {
	v, ok := m[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func getMap(m map[string]any, key string) (map[string]any, bool) {
	v, ok := m[key]
	if !ok {
		return nil, false
	}
	child, ok := v.(map[string]any)
	return child, ok
}

// parseFallbackChains parses one deployment's fallback_chains mapping —
// class name -> a comma-separated string of deployment names, in attempt
// order — per
// docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md's
// "Config shape: comma-separated strings, not YAML lists" section: this
// file's parser has no list support at all, so a scalar string is the
// only way to express an ORDER-preserving sequence within its existing
// constraints (unlike allowed_models' mapping-of-bools, whose order
// never matters). deploymentName is only used for the error message.
func parseFallbackChains(deploymentName string, raw map[string]any) (map[string][]string, error) {
	chains := map[string][]string{}
	for class, v := range raw {
		if !validFallbackClasses[class] {
			return nil, fmt.Errorf("controlplane: deployment %q fallback_chains has unknown error class %q (want one of %q, %q, %q)",
				deploymentName, class, fallbackClassContentPolicy, fallbackClassContextWindowExceeded, fallbackClassGeneric)
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("controlplane: deployment %q fallback_chains.%s must be a comma-separated string of deployment names", deploymentName, class)
		}
		var chain []string
		for _, target := range strings.Split(s, ",") {
			target = strings.TrimSpace(target)
			if target != "" {
				chain = append(chain, target)
			}
		}
		if len(chain) > 0 {
			chains[class] = chain
		}
	}
	return chains, nil
}

// parsePerModelRateLimits parses one virtual key's rate_limit.per_model
// mapping — model name -> its own burst/refill_per_second pair, per
// docs/rfcs/2026-09-07-gateway-multi-dimensional-rate-limits.md. Both
// burst and refill_per_second must be positive for a model entry to be
// valid: unlike the key's own top-level burst/refill_per_second (where 0
// resolves to the gateway's operational default, per RateLimitBurst's doc
// comment), a per-model entry has no such fallback to resolve to, so a
// missing or non-positive value here is a config error, caught at load
// time rather than silently producing a zero-capacity bucket that would
// block every request for that model. keyName is only used for the error
// message.
//
// tpm_capacity/tpm_refill_per_second are the optional per-model TPM
// override, per ModelRateLimitConfig's own doc comment — must be set
// together or neither; unlike burst/refill_per_second, omitting both is
// valid (falls through to the key's own key-level default TPM bucket).
func parsePerModelRateLimits(keyName string, raw map[string]any) (map[string]ModelRateLimitConfig, error) {
	out := make(map[string]ModelRateLimitConfig, len(raw))
	for model, v := range raw {
		modelMap, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("controlplane: virtual key %q rate_limit.per_model.%s must be a mapping", keyName, model)
		}
		burst, _ := getFloat(modelMap, "burst")
		refill, _ := getFloat(modelMap, "refill_per_second")
		if burst <= 0 || refill <= 0 {
			return nil, fmt.Errorf("controlplane: virtual key %q rate_limit.per_model.%s must set positive burst and refill_per_second", keyName, model)
		}
		tpmCapacity, _ := getFloat(modelMap, "tpm_capacity")
		tpmRefill, _ := getFloat(modelMap, "tpm_refill_per_second")
		if (tpmCapacity > 0) != (tpmRefill > 0) {
			return nil, fmt.Errorf("controlplane: virtual key %q rate_limit.per_model.%s.tpm_capacity/tpm_refill_per_second must both be set, or neither", keyName, model)
		}
		out[model] = ModelRateLimitConfig{Burst: burst, RefillPerSecond: refill, TPMCapacity: tpmCapacity, TPMRefillPerSecond: tpmRefill}
	}
	return out, nil
}

// parseDeploymentRateLimit parses one deployment's rate_limit mapping
// into dep in place, per docs/upgrade-research/gateway-per-deployment-
// concurrency-2026-09-09.md. Unlike a virtual key's own top-level
// rate_limit.burst/refill_per_second (where 0 silently resolves to the
// gateway's operational default), a deployment has no such default to
// fall back to -- but unlike parsePerModelRateLimits' per-model entries
// (which have no valid "disabled" state at all), a deployment's own
// ceiling genuinely can be absent (0/0, meaning "no deployment-wide cap"
// -- every config written before this feature existed). What's a real
// config error is a HALF-set pair: burst set without refill_per_second,
// or vice versa, which can only be a mistake, never a deliberate
// "disabled" state. The same rule applies independently to the TPM pair.
func parseDeploymentRateLimit(deploymentName string, rl map[string]any, dep *DeploymentConfig) error {
	dep.RateLimitBurst, _ = getFloat(rl, "burst")
	dep.RateLimitRefill, _ = getFloat(rl, "refill_per_second")
	if (dep.RateLimitBurst > 0) != (dep.RateLimitRefill > 0) {
		return fmt.Errorf("controlplane: deployment %q rate_limit must set both burst and refill_per_second together, or neither", deploymentName)
	}
	dep.TPMCapacity, _ = getFloat(rl, "tpm_capacity")
	dep.TPMRefillPerSecond, _ = getFloat(rl, "tpm_refill_per_second")
	if (dep.TPMCapacity > 0) != (dep.TPMRefillPerSecond > 0) {
		return fmt.Errorf("controlplane: deployment %q rate_limit.tpm_capacity/tpm_refill_per_second must both be set, or neither", deploymentName)
	}
	dep.MaxConcurrentRequests, _ = getInt(rl, "max_concurrent_requests")
	return nil
}
