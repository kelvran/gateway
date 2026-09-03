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
	APIKeyEnv string
}

// ModelPriceConfig is the static per-token price for one model.
// Decimal, not float64, per docs/rfcs/2026-09-02-decimal-cost-accounting.md
// — these values are parsed from their original YAML source string via
// getDecimal, never round-tripped through float64 first.
type ModelPriceConfig struct {
	PromptPerToken     decimal.Decimal
	CompletionPerToken decimal.Decimal
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
	// AllowedModels restricts this key to a subset of configured models.
	// Empty means every configured model is allowed.
	AllowedModels []string
	// RateLimitBurst and RateLimitRefill configure this key's own
	// token-bucket rate limiter. Zero means "use the gateway's default"
	// (resolved by cmd/gateway, not here — this package only parses what
	// the config file says, it doesn't own operational defaults).
	RateLimitBurst  float64
	RateLimitRefill float64
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
	L2         CacheL2Config
	L3         CacheL3Config
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
		if rl, ok := getMap(vkMap, "rate_limit"); ok {
			vk.RateLimitBurst, _ = getFloat(rl, "burst")
			vk.RateLimitRefill, _ = getFloat(rl, "refill_per_second")
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
		if dep.Model == "" || dep.Provider == "" || dep.UpstreamModel == "" || dep.BaseURL == "" || dep.APIKeyEnv == "" {
			return nil, fmt.Errorf("controlplane: deployment %q is missing one of model/provider/upstream_model/base_url/api_key_env", name)
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
		if l2Raw, ok := getMap(cacheRaw, "l2"); ok {
			cfg.Cache.L2.TTLSeconds, _ = getInt(l2Raw, "ttl_seconds")
			cfg.Cache.L2.MaxEntries, _ = getInt(l2Raw, "max_entries")
		}
		if l3Raw, ok := getMap(cacheRaw, "l3"); ok {
			cfg.Cache.L3.TTLSeconds, _ = getInt(l3Raw, "ttl_seconds")
			cfg.Cache.L3.MaxEntries, _ = getInt(l3Raw, "max_entries")
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

	if priceRaw, ok := getMap(root, "price_table"); ok {
		for model, raw := range priceRaw {
			priceMap, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("controlplane: price_table entry %q must be a mapping", model)
			}
			promptPer, _ := getDecimal(priceMap, "prompt_per_token")
			completionPer, _ := getDecimal(priceMap, "completion_per_token")
			cfg.PriceTable[model] = ModelPriceConfig{
				PromptPerToken:     promptPer,
				CompletionPerToken: completionPer,
			}
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

func getMap(m map[string]any, key string) (map[string]any, bool) {
	v, ok := m[key]
	if !ok {
		return nil, false
	}
	child, ok := v.(map[string]any)
	return child, ok
}
