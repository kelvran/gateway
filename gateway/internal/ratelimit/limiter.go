package ratelimit

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/kelvran/gateway/gateway/internal/ratelimit/admission"
)

// KeyConfig is one virtual key's rate-limit parameters — the same
// burst/refill values controlplane.VirtualKeyConfig carries, passed here
// without either package depending on the other, matching this
// project's existing budget/identity decoupling rule
// (gateway/ARCHITECTURE.md's dependency-direction table).
type KeyConfig struct {
	ID              string
	Capacity        float64
	RefillPerSecond float64
	// TPMCapacity/TPMRefillPerSecond configure the optional, separate
	// tokens-per-minute dimension per
	// docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md. TPMCapacity <= 0
	// (the default) means no TPM limit for this key — matching this
	// codebase's existing "0/negative = unlimited" convention. Enforced
	// in BOTH in-memory and Redis mode: that RFC's original "in-memory
	// only in v1, a no-op under NewRedisKeyLimiter" scope limit was
	// closed when ReserveTPM/ReconcileTPM gained their GCRA-backed Redis
	// path (RedisBackend.AllowTPM/AdjustTPM).
	TPMCapacity        float64
	TPMRefillPerSecond float64
	// TokensPerDay is the optional tokens-per-day dimension (round-5 Tier S
	// item 24, 2026-10-15; OWASP LLM06:2026's tokens-per-day limit): a
	// second token bucket of capacity TokensPerDay refilling at
	// TokensPerDay/secondsPerDay tokens per second -- a rolling 24 h
	// allowance, never a midnight reset -- on the same reserve-then-
	// reconcile path as the TPM pair above (ReserveTPD/
	// IncreaseReservationTPD/ReconcileTPD). <= 0 (the default) means no
	// daily cap. Key-level only: there is no per-model day cap. Enforced in
	// both in-memory and Redis mode; in Redis mode the bucket lives under
	// its own NUL-tagged key suffix (tpdBackendKey), never the minute
	// dimension's.
	TokensPerDay float64
	// PerModel maps a model name to that model's own, separate RPM
	// (Capacity, RefillPerSecond) override — the "consumer x model"
	// dimension of Kong-style multi-dimensional rate-limit matching (per
	// docs/upgrade-research/gateway-2026-09-06.md's Finding 5 and
	// docs/rfcs/2026-09-07-gateway-multi-dimensional-rate-limits.md),
	// consulted by AllowForModel BEFORE falling back to this key's own
	// default Capacity/RefillPerSecond bucket above. Nil/empty (the
	// default, and every KeyConfig built before this field existed) means
	// no override for any model: AllowForModel and Allow then behave
	// byte-identically. An entry with Capacity <= 0 is treated as absent
	// — falls through to the default bucket — never as "unlimited" for
	// that model; see NewInMemoryKeyLimiter's construction loop. Enforced
	// in BOTH in-memory and Redis-backed mode, like TPMCapacity above
	// (this sentence called TPMCapacity in-memory-only until 2026-10-15,
	// stale since Redis TPM shipped): RedisBackend.Allow already accepts an
	// arbitrary per-call capacity/refill and key string, so no change to
	// that interface, or to internal/ratelimit/redislimiter, was needed
	// to support this second mode.
	PerModel map[string]ModelRateLimit
}

// DefaultKeyBurstCapacity/DefaultKeyRefillPerSecond are the fallback
// rate-limit values applied to any virtual key whose config doesn't
// specify its own burst/refill (both <= 0) -- exported from this package
// specifically so cmd/gateway's static-config path and
// internal/admin's live virtual-key-mutation path share exactly ONE
// definition, per a round-3 backlog-audit finding: admin.go's own doc
// comment already asserted this resolution happened on the admin path
// ("omitting rate_limit entirely already resets burst/refill to zero,
// which UpsertVirtualKey's caller then resolves to the gateway's own
// default") -- a claim that was FALSE, since the only resolution that
// ever existed was a private, unexported pair of constants inside
// cmd/gateway/main.go's buildPipeline, invisible to and unreachable from
// internal/admin (a separate package). A virtual key created via the
// Admin API with no rate_limit section got Capacity=0/RefillPerSecond=0
// forever -- a TokenBucket built with zero capacity never refills above
// zero, so Allow() denies every request permanently (see HasLimit's own
// doc comment for the same "zero-capacity bucket vs. never-registered"
// distinction this exact case falls into).
const (
	DefaultKeyBurstCapacity   = 20
	DefaultKeyRefillPerSecond = 10
)

// ResolveKeyRateLimit returns burst/refill unchanged unless BOTH are
// <= 0 (this package's own "unset" convention for a virtual key,
// distinct from a per-deployment/per-model rate limit's own "0 means no
// ceiling" convention -- see KeyConfig's own field doc comments), in
// which case it returns DefaultKeyBurstCapacity/DefaultKeyRefillPerSecond
// instead. Every caller constructing a KeyConfig (or an
// identity.VirtualKey's own RateLimitBurst/RateLimitRefill fields) for a
// virtual key must call this exactly once, so "a key with no configured
// rate limit" and "a key explicitly configured with a positive rate
// limit" are the only two states any KeyConfig this package ever
// receives can be in -- never a live, permanently-zero bucket.
func ResolveKeyRateLimit(burst, refill float64) (float64, float64) {
	if burst <= 0 && refill <= 0 {
		return DefaultKeyBurstCapacity, DefaultKeyRefillPerSecond
	}
	return burst, refill
}

// ModelRateLimit is one virtual key's per-model RPM override — see
// KeyConfig.PerModel's doc comment for the full design.
//
// TPMCapacity/TPMRefillPerSecond are the equivalent per-model override for
// the TPM dimension, per docs/upgrade-research/gateway-per-deployment-
// concurrency-2026-09-09.md's own Phase 4 follow-on (extending PerModel's
// existing RPM-only shape to TPM for the direct, non-fallback path) —
// mirroring Capacity/RefillPerSecond's exact convention: both must be set
// together or neither (see buildPerModelTPMBuckets), and an entry with
// TPMCapacity <= 0 is treated as "no TPM override for this model," never
// "unlimited for this model" — falls through to the key's own default TPM
// bucket, exactly like Capacity <= 0 falls through to the default RPM
// bucket. Deliberately NOT consulted by checkFallbackTargetRateLimit
// (dataplane/fallback.go) — TPM stays scoped to the single pre-routing
// ReserveTPM call the caller already makes against the client's
// originally-requested model, matching the already-recorded decision that
// TPM mid-chain reservation-transfer across fallback hops has no
// production precedent anywhere (docs/upgrade-research/gateway-tpm-
// permodel-fallback-2026-09-09.md) — this extension only closes the gap
// for the DIRECT (non-fallback) path.
type ModelRateLimit struct {
	Capacity           float64
	RefillPerSecond    float64
	TPMCapacity        float64
	TPMRefillPerSecond float64
}

// RedisBackend is implemented by internal/ratelimit/redislimiter.Limiter.
// Optional — a KeyLimiter built via NewInMemoryKeyLimiter never touches
// this, mirroring internal/budget.Store's relationship to
// internal/budget/boltstore: the interface lives here, in the consumer
// package, and the implementation package does not import this one.
//
// AllowTPM/AdjustTPM close a real, previously-disclosed v1 scope limit:
// TPM used to be genuinely in-memory-only (a Redis-mode KeyLimiter had
// no TPM enforcement at all — every ReserveTPM call was an unconditional
// no-op). key is whatever Redis key suffix the caller chooses (a plain
// keyID, or a perModelBackendKey variant) — mirroring Allow's own key
// parameter, just under redislimiter's own separate "ratelimit:tpm:"
// namespace so the two dimensions never collide.
type RedisBackend interface {
	Allow(ctx context.Context, keyID string, capacity, refillPerSecond float64) (bool, error)
	AllowTPM(ctx context.Context, key string, burst, rate, periodSec, cost float64) (allowed bool, retryAfterSec float64, err error)
	AdjustTPM(ctx context.Context, key string, deltaIncrement float64) error
	// ReserveTPM and SettleTPM (2026-10-13) are the reserve-then-reconcile
	// pair for a request's TPM reservation, sized from the key's own usage
	// history kept in the backend: ReserveTPM reserves coldStartCost only
	// while the key has no settled history and the running mean of settled
	// real tokens afterwards, returning what it reserved; SettleTPM moves
	// the arrival time by deltaIncrement and, when realTokens is non-nil,
	// records it in that history. AllowTPM/AdjustTPM stay for the
	// mid-stream top-up path (IncreaseReservationTPM), which changes a
	// reservation without any real usage to learn from.
	ReserveTPM(ctx context.Context, key string, burst, rate, periodSec, coldStartCost float64) (allowed bool, retryAfterSec, reservedCost float64, err error)
	SettleTPM(ctx context.Context, key string, deltaIncrement float64, realTokens *float64) error
	// Admit is the one-round-trip gate over RPM, TPM and tokens-per-day
	// (round-5 item 10 phase 2b, 2026-10-17): it evaluates the three in that
	// order and writes a reservation only when all three admit, so a refused
	// request consumes nothing in any dimension. KeyLimiter.Admit builds the
	// request from the key's config and maps the result.
	Admit(ctx context.Context, req admission.Request) (admission.Result, error)
	// SettleTokens settles up to two token dimensions in one round trip --
	// the paired form of SettleTPM, which the chat path no longer calls.
	// No entries means no call.
	SettleTokens(ctx context.Context, settlements []admission.Settlement) error
	Close() error
}

// The dimension names Admission.RefusedBy and admission.Result.RefusedBy use.
const (
	RefusedByRPM = "rpm"
	RefusedByTPM = "tpm"
	RefusedByTPD = "tpd"
)

// TokenReservation is one token dimension's reservation as Admit granted it
// and SettleTokens settles it: Reserved false with Tokens 0 when the dimension
// is off or the gate failed open; Epoch is the in-memory bucket's reset epoch
// (always 0 in Redis mode).
type TokenReservation struct {
	Reserved bool
	Tokens   float64
	Epoch    int64
}

// Admission is Admit's decision over the three rate-limit dimensions.
type Admission struct {
	Allowed bool
	// RefusedBy is RefusedByRPM, RefusedByTPM or RefusedByTPD when !Allowed.
	RefusedBy string
	// RetryAfter is the tokens-per-day wait on a RefusedByTPD refusal (0 for
	// a cold refusal, as ReserveTPD reported it); RPM and TPM refusals carry
	// no wait and leave the caller's backoff to decide, as before.
	RetryAfter time.Duration
	TPM, TPD   TokenReservation
}

// KeyLimiter is the single per-Pipeline rate limiter, backed by either
// in-memory TokenBuckets (default, per NewInMemoryKeyLimiter) or a
// RedisBackend (per NewRedisKeyLimiter, per
// docs/rfcs/2026-09-03-distributed-rate-limiting.md). Callers never need
// to know which — both are driven through the same Allow method.
//
// mu guards configs/buckets against concurrent access between Allow and
// Register — the two maps were originally build-once-at-construction and
// read-only for the rest of the process lifetime, which needed no lock;
// Register (per docs/rfcs/2026-09-05-gateway-admin-api.md's live virtual-
// key mutation) is what first makes them live-mutable.
type KeyLimiter struct {
	mu         sync.RWMutex
	configs    map[string]KeyConfig
	buckets    map[string]*TokenBucket // RPM, non-nil in in-memory mode only
	tpmBuckets map[string]*TokenBucket // TPM, in-memory mode only; absent entirely in Redis mode
	tpdBuckets map[string]*TokenBucket // tokens-per-day (KeyConfig.TokensPerDay), in-memory mode only; absent entirely in Redis mode
	// perModelBuckets is keyID -> model -> that model's own TokenBucket,
	// in-memory mode only (non-nil whenever backend == nil, mirroring
	// buckets/tpmBuckets above). Redis mode instead reads cfg.PerModel
	// straight out of configs on every AllowForModel call — no separate
	// bucket-tracking structure needed there, since RedisBackend.Allow
	// takes capacity/refill per call rather than owning bucket state
	// itself.
	perModelBuckets map[string]map[string]*TokenBucket
	// perModelTPMBuckets mirrors perModelBuckets exactly, one dimension
	// over: keyID -> model -> that model's own TPM TokenBucket, in-memory
	// mode only, absent entirely in Redis mode — there the TPM state lives
	// in Redis (ReserveTPM/IncreaseReservationTPM/ReconcileTPM go through
	// RedisBackend.AllowTPM/AdjustTPM; this sentence said TPM had "no
	// Redis-backed path" until 2026-10-08, stale since Redis TPM shipped),
	// so this stays nil forever whenever backend != nil, exactly like
	// tpmBuckets itself.
	perModelTPMBuckets map[string]map[string]*TokenBucket
	backend            RedisBackend // non-nil in Redis mode only
}

// NewInMemoryKeyLimiter builds a KeyLimiter backed by one TokenBucket per
// key, eagerly constructed here — the exact behavior
// dataplane.NewPipeline built inline before this RFC. A key with
// TPMCapacity > 0 also gets a second TokenBucket for the TPM dimension
// (per docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md); one with
// TPMCapacity <= 0 gets none at all, so AllowTPM/RecordTokens can treat
// "no entry" as "unlimited" rather than misreading a zero-capacity
// bucket (always empty) as "always blocked."
func NewInMemoryKeyLimiter(keys []KeyConfig) *KeyLimiter {
	configs := make(map[string]KeyConfig, len(keys))
	buckets := make(map[string]*TokenBucket, len(keys))
	tpmBuckets := make(map[string]*TokenBucket, len(keys))
	tpdBuckets := make(map[string]*TokenBucket, len(keys))
	perModelBuckets := make(map[string]map[string]*TokenBucket, len(keys))
	perModelTPMBuckets := make(map[string]map[string]*TokenBucket, len(keys))
	for _, k := range keys {
		configs[k.ID] = k
		buckets[k.ID] = NewTokenBucket(k.Capacity, k.RefillPerSecond)
		if k.TPMCapacity > 0 {
			tpmBuckets[k.ID] = NewTokenBucket(k.TPMCapacity, k.TPMRefillPerSecond)
		}
		if k.TokensPerDay > 0 {
			tpdBuckets[k.ID] = newDayBucket(k.TokensPerDay)
		}
		if models := buildPerModelBuckets(k.PerModel); len(models) > 0 {
			perModelBuckets[k.ID] = models
		}
		if models := buildPerModelTPMBuckets(k.PerModel); len(models) > 0 {
			perModelTPMBuckets[k.ID] = models
		}
	}
	// configs is tracked in in-memory mode too (not just Redis mode,
	// which already needed it for allow()'s backend branch) specifically
	// so HasLimit below has a reliable source of truth — Allow/
	// AllowForModel's own zero-capacity TokenBucket (Capacity <= 0, the
	// default for a caller that never resolves it to a positive fallback,
	// e.g. a deployment-scoped limiter per docs/upgrade-research/gateway-
	// per-deployment-concurrency-2026-09-09.md) is indistinguishable from
	// a real, deliberately-exhausted bucket by inspecting buckets alone —
	// both have zero tokens and both return false from Allow().
	return &KeyLimiter{configs: configs, buckets: buckets, tpmBuckets: tpmBuckets, tpdBuckets: tpdBuckets, perModelBuckets: perModelBuckets, perModelTPMBuckets: perModelTPMBuckets}
}

// secondsPerDay is the tokens-per-day dimension's refill window: a bucket of
// capacity TokensPerDay refills at TokensPerDay/secondsPerDay tokens per
// second, so a key that spends its whole allowance is whole again 24 h
// later -- rolling, not reset at midnight (round-5 Tier S item 24).
const secondsPerDay = 86400.0

// newDayBucket builds the in-memory bucket for KeyConfig.TokensPerDay.
func newDayBucket(tokensPerDay float64) *TokenBucket {
	return NewTokenBucket(tokensPerDay, tokensPerDay/secondsPerDay)
}

// tpdBackendKey builds the Redis-mode key suffix for keyID's tokens-per-day
// bucket, distinct from the minute dimension's plain keyID (and from every
// perModelBackendKey) under the SAME "ratelimit:tpm:" namespace and Lua
// scripts -- the same NUL-tagged field shape perModelBackendKey uses, for
// the same collision reasons that function's doc comment gives. Its
// ":hist" running mean is derived from this suffix too, so the day
// dimension keeps its own usage history.
func tpdBackendKey(keyID string) string {
	return keyID + "\x00dim=day"
}

// tpdRedisParams resolves cfg's tokens-per-day bucket as GCRA parameters
// at the 1 s period every other backend call uses: burst = TokensPerDay,
// rate = TokensPerDay/secondsPerDay tokens per second. Passing periodSec
// 86400 with rate = TokensPerDay would be numerically identical inside
// the script (emission_interval = period*1000/rate) but ReconcileTPD's
// settle delta, like ReconcileTPM's, hard-codes the 1 s period -- so the
// period stays 1.0 and the rate is derived.
func tpdRedisParams(cfg KeyConfig) (burst, rate float64) {
	return cfg.TokensPerDay, cfg.TokensPerDay / secondsPerDay
}

// dayColdStartHours bounds what a day bucket with NO usage history may
// reserve for one request: one hour's share of the day, TokensPerDay/24
// (what the bucket refills in an hour). Without it a cold day-only key
// (tokens_per_day with no tpm_capacity) would reserve its whole day on the
// first request and admit one request at a time until it settled -- the
// 2026-10-08 F2 serialisation at day scale -- and, in Redis mode, tell a
// concurrent sibling to wait a day for pressure that clears in seconds.
// With history the running mean sizes the reservation and this bound is
// not consulted (TokenBucket.ReserveTokens; tpmReserveLuaSrc).
const dayColdStartHours = 24.0

// dayColdStartCap is the cold-start reservation for a day bucket of
// tokensPerDay: one hour's share, or the same request's minute reservation
// when that is smaller (0 = no minute reservation: TPM not configured for
// the key, or its check failed open).
func dayColdStartCap(tokensPerDay, minuteReservation float64) float64 {
	cold := tokensPerDay / dayColdStartHours
	if minuteReservation > 0 && minuteReservation < cold {
		cold = minuteReservation
	}
	return cold
}

// isColdRefusal reports whether the cost a refusing reserve script
// reported is the cold-start cost the caller passed, within the relative
// tolerance of the script's %.14g number formatting -- see ReserveTPD.
func isColdRefusal(reportedCost, coldStart float64) bool {
	return math.Abs(reportedCost-coldStart) <= 1e-9*math.Max(1, coldStart)
}

// secondsDuration converts the backend's retry_after_sec to a Duration.
func secondsDuration(sec float64) time.Duration {
	return time.Duration(sec * float64(time.Second))
}

// buildPerModelBuckets constructs one TokenBucket per PerModel entry with
// a positive Capacity, skipping any entry with Capacity <= 0 — shared by
// NewInMemoryKeyLimiter and Register so the "absent or non-positive means
// no override" rule can't drift between the two construction sites.
func buildPerModelBuckets(perModel map[string]ModelRateLimit) map[string]*TokenBucket {
	if len(perModel) == 0 {
		return nil
	}
	models := make(map[string]*TokenBucket, len(perModel))
	for model, mrl := range perModel {
		if mrl.Capacity > 0 {
			models[model] = NewTokenBucket(mrl.Capacity, mrl.RefillPerSecond)
		}
	}
	return models
}

// buildPerModelTPMBuckets mirrors buildPerModelBuckets exactly, one
// dimension over: one TokenBucket per PerModel entry with a positive
// TPMCapacity, skipping any entry with TPMCapacity <= 0 — shared by
// NewInMemoryKeyLimiter and Register so the "absent or non-positive means
// no per-model TPM override" rule can't drift between the two
// construction sites, matching buildPerModelBuckets' own precedent.
func buildPerModelTPMBuckets(perModel map[string]ModelRateLimit) map[string]*TokenBucket {
	if len(perModel) == 0 {
		return nil
	}
	models := make(map[string]*TokenBucket, len(perModel))
	for model, mrl := range perModel {
		if mrl.TPMCapacity > 0 {
			models[model] = NewTokenBucket(mrl.TPMCapacity, mrl.TPMRefillPerSecond)
		}
	}
	return models
}

// updatePerModelTPMBuckets is buildPerModelTPMBuckets' Register-only
// sibling: for each model in perModel with a positive TPMCapacity, it
// reuses (via TokenBucket.Reset) the existing bucket at
// existing[model] when one is already present, rather than always
// constructing a brand-new object the way buildPerModelTPMBuckets
// (used only for a KeyLimiter's initial construction, where no
// existing bucket could ever be in use by an outstanding reservation)
// does — see Register's own call site and TokenBucket.Reset's doc
// comment for the full reservation-race rationale this closes.
func updatePerModelTPMBuckets(existing map[string]*TokenBucket, perModel map[string]ModelRateLimit) map[string]*TokenBucket {
	if len(perModel) == 0 {
		return nil
	}
	models := make(map[string]*TokenBucket, len(perModel))
	for model, mrl := range perModel {
		if mrl.TPMCapacity <= 0 {
			continue
		}
		if bucket := existing[model]; bucket != nil {
			bucket.Reset(mrl.TPMCapacity, mrl.TPMRefillPerSecond)
			models[model] = bucket
		} else {
			models[model] = NewTokenBucket(mrl.TPMCapacity, mrl.TPMRefillPerSecond)
		}
	}
	return models
}

// NewRedisKeyLimiter builds a KeyLimiter that delegates every Allow call
// to backend, passing each key's own Capacity/RefillPerSecond through on
// every call (backend holds no per-key state itself — Redis does).
func NewRedisKeyLimiter(keys []KeyConfig, backend RedisBackend) *KeyLimiter {
	configs := make(map[string]KeyConfig, len(keys))
	for _, k := range keys {
		configs[k.ID] = k
	}
	return &KeyLimiter{configs: configs, backend: backend}
}

// Allow reports whether one request against keyID may proceed. In Redis
// mode, a non-nil error means the backend call itself failed (e.g. a
// network error) — internal/ratelimit.KeyLimiter does not decide what to
// do about that; per docs/rfcs/2026-09-03-distributed-rate-limiting.md,
// that policy decision (fail-open, with a warning log) belongs to the
// caller (dataplane.Pipeline.checkRateLimit), not this package, so this
// method stays a faithful pass-through rather than baking in one
// caller's specific policy.
func (l *KeyLimiter) Allow(ctx context.Context, keyID string) (bool, error) {
	return l.allow(ctx, keyID, "")
}

// AllowForModel is exactly like Allow, except it first checks keyID's
// PerModel[model] override (see KeyConfig.PerModel's doc comment) and, if
// one is configured, decides against THAT bucket instead of — never in
// addition to — keyID's own default bucket: a virtual key with a
// per-model override for one model gets a rate limit for that model's
// traffic that is entirely separate from (and never drains, nor is
// drained by) its default bucket, which every OTHER model still shares.
// Falls back to exactly Allow's own behavior when model has no
// configured override (or model is ""), which is what makes a key with
// zero PerModel entries behave byte-identically whichever of the two
// entry points a caller uses — per
// docs/rfcs/2026-09-07-gateway-multi-dimensional-rate-limits.md.
func (l *KeyLimiter) AllowForModel(ctx context.Context, keyID, model string) (bool, error) {
	return l.allow(ctx, keyID, model)
}

// HasLimit reports whether keyID has a real, configured rate limit
// (Capacity > 0) — as opposed to "never registered at all" or
// "registered with Capacity <= 0". This is the query a caller needs
// BEFORE calling Allow/AllowForModel when, unlike a virtual key (which
// always resolves to a positive fallback capacity before construction —
// see dataplane's own defaultBurstCapacity/defaultRefillPerSecond),
// "unconfigured" is meant to be a real, valid "no cap at all" state, not
// "deny everything": Allow's own nil-bucket/zero-capacity branch returns
// false for BOTH "never registered" and "registered with Capacity 0" —
// it cannot distinguish the two, since a zero-capacity TokenBucket and a
// bucket that was never created both simply have zero tokens. A caller
// wanting "0/unconfigured means unlimited" (per docs/upgrade-research/
// gateway-per-deployment-concurrency-2026-09-09.md's deployment-scoped
// ceiling) must check HasLimit first and skip the Allow call entirely
// when it's false, exactly like AllowTPM/AllowForModel's own
// PerModel-absent branch already treats "no entry" as "unlimited" rather
// than misreading a zero-capacity bucket as "always blocked."
func (l *KeyLimiter) HasLimit(keyID string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.configs[keyID].Capacity > 0
}

func (l *KeyLimiter) allow(ctx context.Context, keyID, model string) (bool, error) {
	if l.backend != nil {
		l.mu.RLock()
		cfg := l.configs[keyID]
		l.mu.RUnlock()
		if model != "" {
			if override, ok := cfg.PerModel[model]; ok && override.Capacity > 0 {
				return l.backend.Allow(ctx, perModelBackendKey(keyID, model), override.Capacity, override.RefillPerSecond)
			}
		}
		return l.backend.Allow(ctx, keyID, cfg.Capacity, cfg.RefillPerSecond)
	}
	l.mu.RLock()
	var bucket *TokenBucket
	if model != "" {
		bucket = l.perModelBuckets[keyID][model]
	}
	if bucket == nil {
		bucket = l.buckets[keyID]
	}
	l.mu.RUnlock()
	// bucket is nil for a keyID nothing ever registered — an unreachable
	// case for a normally-built config (identity.Verifier and KeyLimiter
	// are always built from the same key list), but Register (see below)
	// is what first makes it possible for the two to diverge, so this
	// stays a graceful "always deny," never the nil-pointer panic
	// TokenBucket.Allow would otherwise hit dereferencing its own mutex.
	if bucket == nil {
		return false, nil
	}
	return bucket.Allow(), nil
}

// perModelBackendKey builds a Redis-mode key for keyID's PerModel[model]
// override that is materially distinct from both keyID's own default key
// ("ratelimit:" + keyID, built inside redislimiter.Limiter.Allow) and any
// OTHER model's override — using an explicit, NUL-byte-tagged field
// separator, mirroring internal/cache/key.go's Key/NormalizedKey own
// precedent for exactly this problem (that file's own doc comment: a
// leading tag plus NUL-separated, explicitly-named fields so two distinct
// logical keys can never collide, even given adversarially-chosen
// component strings). Naive ":"-joining a virtual key ID and a model name
// — both caller-controlled, variable-length strings — would risk exactly
// that: e.g. keyID "a:model=b" colliding with keyID "a", model "b". The
// NUL byte can't appear in either component via this project's YAML/JSON
// config surfaces, so this stays collision-free in practice, not just in
// theory.
func perModelBackendKey(keyID, model string) string {
	return keyID + "\x00model=" + model
}

// AllowTPM reports whether keyID's token-bucket balance is currently
// positive — the TPM dimension's decision, made from PAST usage only,
// since the request being decided hasn't run yet and its own real cost
// is unknown (see RecordTokens). Returns true unconditionally when TPM
// isn't configured for keyID (no entry in tpmBuckets — either
// TPMCapacity <= 0, or a Redis-mode KeyLimiter, whose TPM decisions are
// made by ReserveTPM against Redis instead of this in-memory bucket; the
// "deliberate no-op in v1" this comment described until 2026-10-08 ended
// when Redis-mode TPM shipped).
func (l *KeyLimiter) AllowTPM(keyID string) bool {
	l.mu.RLock()
	bucket := l.tpmBuckets[keyID]
	l.mu.RUnlock()
	if bucket == nil {
		return true
	}
	return bucket.HasBalance()
}

// RecordTokens debits keyID's TPM bucket by tokens, once real usage is
// known — never part of AllowTPM's pre-request decision. A no-op when
// TPM isn't configured for keyID (see AllowTPM).
func (l *KeyLimiter) RecordTokens(keyID string, tokens int) {
	l.mu.RLock()
	bucket := l.tpmBuckets[keyID]
	l.mu.RUnlock()
	if bucket == nil {
		return
	}
	bucket.Debit(float64(tokens))
}

// ReserveTPM is AllowTPM's concurrency-safe replacement for real
// request-handling code, per
// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md — see
// TokenBucket.ReserveTPM for the full mechanism. allowed is true
// unconditionally (reserved false, reservedTokens 0 — nothing is ever
// reserved) when TPM isn't configured for keyID at all (no per-model
// override for model, and no key-level default either), exactly
// mirroring AllowTPM's own "no entry means unlimited" behavior. reserved
// is false whenever nothing was actually reserved against a real bucket
// — either TPM isn't configured, or the bucket's own check rejected the
// request — so a caller can gate whether a later ReconcileTPM call is
// even meaningful without needing to inspect reservedTokens itself.
//
// model resolves keyID's own per-model TPM override first (see
// ModelRateLimit.TPMCapacity's doc comment) — mirroring AllowForModel's
// identical RPM-dimension resolution order exactly — falling back to
// keyID's key-level default TPM bucket whenever model has no configured
// override (or model is ""). This is the direct (non-fallback) path
// only: checkFallbackTargetRateLimit deliberately never calls this with
// a fallback-hop's own model, per ModelRateLimit's own doc comment.
//
// Every true `reserved` return MUST be paired with exactly one
// ReconcileTPM call for the same keyID/model/reservedTokens, even on an
// error/timeout path — model must be the SAME value passed to the
// ReserveTPM call being reconciled, so the reconciliation lands on the
// exact bucket the reservation was actually taken from.
//
// In Redis mode (backend != nil), this now genuinely enforces TPM via
// GCRA (RedisBackend.AllowTPM) instead of the prior unconditional no-op
// — see resolveTPMRedisParams's own doc comment for the reservation
// estimate this uses, and reservationEpoch's own note below for why
// Redis mode never needs the in-memory dimension's reset-epoch guard. A
// Redis error fails open (err is logged by the caller, per Allow's own
// identical policy for the RPM dimension) — returned via err here so
// checkRateLimit's caller can apply that exact policy itself, rather
// than this package baking one caller's choice in.
func (l *KeyLimiter) ReserveTPM(ctx context.Context, keyID, model string) (allowed bool, reserved bool, reservedTokens float64, reservationEpoch int64, err error) {
	l.mu.RLock()
	if l.backend != nil {
		cfg := l.configs[keyID]
		l.mu.RUnlock()
		burst, rate, key := resolveTPMRedisParams(keyID, model, cfg)
		if burst <= 0 {
			return true, false, 0, 0, nil
		}
		// The backend sizes the reservation from the key's own usage
		// history, mirroring TokenBucket.reservationAmountLocked: the
		// running mean of settled real tokens once the key has history,
		// and the full configured burst -- the cold-start cost passed
		// here -- until then, so a cold key's concurrent burst can never
		// overshoot its cap (docs/rfcs/2026-09-08-gateway-budget-
		// ratelimit-toctou-fix.md). Before 2026-10-13 Redis mode kept no
		// history and every reservation was the full burst, which
		// admitted one request per key at a time (the 2026-10-08 live
		// run's F2). reservedCost is what was actually reserved; the
		// caller hands it back to ReconcileTPM, which settles the delta.
		allowedRedis, _, reservedCost, tpmErr := l.backend.ReserveTPM(ctx, key, burst, rate, 1.0, burst)
		if tpmErr != nil {
			return true, false, 0, 0, tpmErr
		}
		if !allowedRedis {
			return false, false, 0, 0, nil
		}
		return true, true, reservedCost, 0, nil
	}
	bucket := l.resolveTPMBucket(keyID, model)
	l.mu.RUnlock()
	if bucket == nil {
		return true, false, 0, 0, nil
	}
	allowed, reservedTokens, reservationEpoch = bucket.ReserveTPM()
	return allowed, allowed, reservedTokens, reservationEpoch, nil
}

// resolveTPMRedisParams resolves keyID's TPM burst/rate for model in
// Redis mode — its own per-model override (cfg.PerModel[model], if
// configured with a positive TPMCapacity) or keyID's key-level default,
// mirroring resolveTPMBucket's identical per-model-then-default order
// for the in-memory dimension. key is the Redis key SUFFIX
// AllowTPM/AdjustTPM should use (perModelBackendKey's tagged form for a
// per-model override, plain keyID otherwise) — mirrors allow()'s
// identical choice for the RPM dimension.
func resolveTPMRedisParams(keyID, model string, cfg KeyConfig) (burst, rate float64, key string) {
	if model != "" {
		if override, ok := cfg.PerModel[model]; ok && override.TPMCapacity > 0 {
			return override.TPMCapacity, override.TPMRefillPerSecond, perModelBackendKey(keyID, model)
		}
	}
	return cfg.TPMCapacity, cfg.TPMRefillPerSecond, keyID
}

// ReconcileTPM undoes a previous ReserveTPM call's provisional debit and,
// if realTokens is non-nil, debits the real usage in its place — a
// no-op, matching RecordTokens' own existing "no TPM bucket for keyID"
// behavior, when TPM isn't configured for keyID/model (a Redis-mode
// KeyLimiter is no longer the exception: it reconciles through the
// backend's AdjustTPM; this sentence called Redis TPM "a deliberate v1
// no-op" until 2026-10-08, stale since Redis-mode TPM shipped) — safe to call
// unconditionally with whatever reservedTokens/reservationEpoch a prior
// ReserveTPM call returned, even 0, since ReconcileTPM(0, epoch, nil)
// against a real bucket is itself a genuine no-op (tokens += 0). model
// MUST be the exact value passed to the ReserveTPM call this reconciles
// — see that method's own doc comment. reservationEpoch MUST be the
// exact value that same call (or the latest IncreaseReservationTPM call
// for this reservation) returned — see TokenBucket.resetEpoch's own
// field comment for why.
//
// Disclosed, not fixed, remaining gap: reservationEpoch only protects
// against a Reset call on the SAME bucket object ReserveTPM resolved to.
// If, between this reservation's ReserveTPM call and this ReconcileTPM
// call, an admin Register call removes model's per-model override
// entirely (or drops its TPMCapacity to <=0), resolveTPMBucket below
// falls through to keyID's key-level default bucket instead — a
// DIFFERENT object with its own, unrelated resetEpoch — so this still
// misapplies the reservation's credit/debit there rather than safely
// no-op'ing. Fully closing this needs retaining a removed per-model
// bucket object (a tombstone) long enough for any reservation already
// taken against it to reconcile, a disproportionately larger change
// than this narrow (an admin must edit exactly the model a request is
// mid-flight against), self-healing (the leaked credit is bounded and
// absorbed by the default bucket's own capacity clamp) race justifies
// for this pass.
// In Redis mode, reservationEpoch is always 0 (see ReserveTPM) and is
// never checked here: unlike the in-memory bucket, which can have its
// entire object replaced wholesale by a concurrent Register/Reset call
// (orphaning any in-flight reservation's reference to the OLD object),
// Redis mode's "bucket" is just a key string that persists unchanged
// across Register calls — there is no analogous reset event to guard
// against. AdjustTPM's delta-based design (always ADDING onto whatever
// the current TAT is, never a snapshot compare-and-swap) is safe under
// arbitrary concurrent interleaving by construction. A Redis error here
// is a disclosed best-effort no-op (never surfaced to this method's
// caller, which has no error return to give it to) — see AdjustTPM's
// own doc comment: the stored TAT simply keeps reflecting the
// ESTIMATE rather than the real cost for this one reservation, which
// self-heals on the key's own next real request regardless.
func (l *KeyLimiter) ReconcileTPM(ctx context.Context, keyID, model string, reservedTokens float64, reservationEpoch int64, realTokens *float64) {
	l.mu.RLock()
	if l.backend != nil {
		cfg := l.configs[keyID]
		l.mu.RUnlock()
		burst, rate, key := resolveTPMRedisParams(keyID, model, cfg)
		if burst <= 0 {
			return
		}
		// effectiveRealTokens is 0 for a release-with-no-replacement
		// (realTokens == nil) — the SAME unified formula covers both
		// cases: releasing reservedTokens (moving the TAT backward by
		// emissionInterval*reservedTokens) and, when realTokens is
		// known, simultaneously re-debiting the real cost, exactly
		// mirroring the in-memory bucket's own two-step
		// "tokens += reservedTokens; tokens -= *realTokens" net effect
		// (there, increasing tokens = decreasing TAT here, and vice
		// versa). Omitting this call entirely when realTokens is nil
		// would leak the full reservation's capacity permanently — the
		// exact bug ReconcileTPM's own doc comment above warns every
		// true `reserved` return from ReserveTPM must avoid.
		var effectiveRealTokens float64
		if realTokens != nil {
			effectiveRealTokens = *realTokens
		}
		// 1000.0/rate, not 1.0/rate: the period ReserveTPM's own AllowTPM
		// call always passes is 1.0 SECOND, but the stored TAT this
		// delta adjusts is in MILLISECONDS -- matching
		// tpmLuaSrc's own required *1000 conversion exactly. Getting
		// this wrong (as an earlier draft of this fix did) makes every
		// adjustment negligibly small, silently defeating GCRA's own
		// admission math entirely.
		emissionInterval := 1000.0 / rate
		deltaIncrement := emissionInterval * (effectiveRealTokens - reservedTokens)
		// SettleTPM, not AdjustTPM: the same arrival-time move, plus the
		// real count recorded in the key's usage history when known, so
		// the next ReserveTPM is sized from it (a nil realTokens teaches
		// nothing). Errors fail open as before, uncounted (FAILURE-MODES).
		_ = l.backend.SettleTPM(ctx, key, deltaIncrement, realTokens)
		return
	}
	bucket := l.resolveTPMBucket(keyID, model)
	l.mu.RUnlock()
	if bucket == nil {
		return
	}
	bucket.ReconcileTPM(reservedTokens, reservationEpoch, realTokens)
}

// IncreaseReservationTPM is ReserveTPM's mid-stream top-up sibling — see
// TokenBucket.IncreaseReservation's own doc comment for the shared
// design rationale. model resolves the same per-model-then-default
// bucket ReserveTPM/ReconcileTPM already use — MUST be the same model
// value the original ReserveTPM call for this reservation used, exactly
// like ReconcileTPM's own contract. A no-op, always allowed, when TPM
// isn't configured for keyID/model at all (nil bucket), mirroring
// ReserveTPM's own "no entry means unlimited" behavior. reservationEpoch/
// newReservationEpoch mirror ReconcileTPM's own epoch contract — thread
// whichever epoch this returns into the eventual ReconcileTPM call.
// In Redis mode, this tops up the reservation by issuing a fresh
// AllowTPM admission check for exactly the delta (newReservedTokens -
// currentReservedTokens) — GCRA's TAT-advance IS the reservation, so
// "reserve more" is just another AllowTPM call for the additional
// amount, atomically checked against the SAME key's current TAT. A
// Redis error fails open (err logged by the caller, mirroring
// ReserveTPM's identical policy).
func (l *KeyLimiter) IncreaseReservationTPM(ctx context.Context, keyID, model string, currentReservedTokens, newReservedTokens float64, reservationEpoch int64) (allowed bool, appliedTokens float64, newReservationEpoch int64, err error) {
	if newReservedTokens <= currentReservedTokens {
		return true, currentReservedTokens, reservationEpoch, nil
	}
	l.mu.RLock()
	if l.backend != nil {
		cfg := l.configs[keyID]
		l.mu.RUnlock()
		burst, rate, key := resolveTPMRedisParams(keyID, model, cfg)
		if burst <= 0 {
			return true, currentReservedTokens, reservationEpoch, nil
		}
		delta := newReservedTokens - currentReservedTokens
		allowedRedis, _, tpmErr := l.backend.AllowTPM(ctx, key, burst, rate, 1.0, delta)
		if tpmErr != nil {
			return true, currentReservedTokens, reservationEpoch, tpmErr
		}
		if !allowedRedis {
			return false, currentReservedTokens, reservationEpoch, nil
		}
		return true, newReservedTokens, 0, nil
	}
	bucket := l.resolveTPMBucket(keyID, model)
	l.mu.RUnlock()
	if bucket == nil {
		return true, currentReservedTokens, reservationEpoch, nil
	}
	allowed, appliedTokens, newReservationEpoch = bucket.IncreaseReservation(currentReservedTokens, newReservedTokens, reservationEpoch)
	return allowed, appliedTokens, newReservationEpoch, nil
}

// ReserveTPD is ReserveTPM's tokens-per-day sibling (round-5 Tier S item
// 24, 2026-10-15): a reservation against keyID's day bucket
// (KeyConfig.TokensPerDay), sized like the TPM one -- the bucket's own
// running mean once it has history, else a cold-start reservation of one
// hour's share of the day, or the same request's minute reservation when
// that is smaller (dayColdStartCap; minuteReservation 0 means none), so
// a cold key never reserves its whole day on one request. Key-level
// only: there is no per-model day bucket, so no model parameter. allowed
// is true unconditionally (reserved false) when tokens_per_day isn't
// configured for keyID. retryAfter is set only on a refusal the bucket's
// own usage history decided: in memory the time until the balance is
// positive again; in Redis mode the GCRA script's wait until a mean-sized
// reservation fits. A refusal decided with NO history (the script sized
// the attempt at the cold-start cost) carries no wait: that wait would
// count the provisional reservations of requests still in flight, which
// settle within seconds -- the caller then keeps the per-key backoff, as
// for every RPM and TPM refusal. The caller floors the client's
// Retry-After on a non-zero wait -- the backoff alone would tell a
// day-exhausted client to retry within 30 s all day. Every true
// `reserved` MUST be paired with exactly one ReconcileTPD call, on every
// return path. A Redis error fails open, returned for the caller's own
// policy exactly like ReserveTPM.
func (l *KeyLimiter) ReserveTPD(ctx context.Context, keyID string, minuteReservation float64) (allowed bool, reserved bool, reservedTokens float64, reservationEpoch int64, retryAfter time.Duration, err error) {
	l.mu.RLock()
	if l.backend != nil {
		cfg := l.configs[keyID]
		l.mu.RUnlock()
		burst, rate := tpdRedisParams(cfg)
		if burst <= 0 {
			return true, false, 0, 0, 0, nil
		}
		coldStart := dayColdStartCap(burst, minuteReservation)
		allowedRedis, retryAfterSec, reservedCost, tpdErr := l.backend.ReserveTPM(ctx, tpdBackendKey(keyID), burst, rate, 1.0, coldStart)
		if tpdErr != nil {
			return true, false, 0, 0, 0, tpdErr
		}
		if !allowedRedis {
			// The script reports the cost it sized the attempt at: the
			// running mean when the key has history, the cold-start cost
			// otherwise. A cold refusal carries no wait (see above). The
			// comparison tolerates the script's number formatting: Lua
			// 5.1 prints with %.14g, so 2000000/24 comes back as
			// 83333.333333333, not 83333.33333333333 -- an exact compare
			// would surface the inflated wait for most real caps. A
			// running mean within that tolerance of the cold-start cost
			// is read as cold too and keeps the backoff: a coincidence
			// that costs one refusal its wait, never a wrong admission.
			wait := secondsDuration(retryAfterSec)
			if isColdRefusal(reservedCost, coldStart) {
				wait = 0
			}
			return false, false, 0, 0, wait, nil
		}
		return true, true, reservedCost, 0, 0, nil
	}
	cfg := l.configs[keyID]
	bucket := l.tpdBuckets[keyID]
	l.mu.RUnlock()
	if bucket == nil {
		return true, false, 0, 0, 0, nil
	}
	allowed, reservedTokens, reservationEpoch, retryAfter = bucket.ReserveTokens(dayColdStartCap(cfg.TokensPerDay, minuteReservation))
	return allowed, allowed, reservedTokens, reservationEpoch, retryAfter, nil
}

// ReconcileTPD is ReconcileTPM for the tokens-per-day bucket: the same
// contract (reservedTokens/reservationEpoch exactly as ReserveTPD or the
// latest IncreaseReservationTPD returned; realTokens nil releases without
// teaching the history), the same Redis settle arithmetic at the 1 s
// period (1000/rate ms per token, rate = TokensPerDay/secondsPerDay),
// a no-op when tokens_per_day isn't configured.
func (l *KeyLimiter) ReconcileTPD(ctx context.Context, keyID string, reservedTokens float64, reservationEpoch int64, realTokens *float64) {
	l.mu.RLock()
	if l.backend != nil {
		cfg := l.configs[keyID]
		l.mu.RUnlock()
		burst, rate := tpdRedisParams(cfg)
		if burst <= 0 {
			return
		}
		var effectiveRealTokens float64
		if realTokens != nil {
			effectiveRealTokens = *realTokens
		}
		emissionInterval := 1000.0 / rate
		deltaIncrement := emissionInterval * (effectiveRealTokens - reservedTokens)
		_ = l.backend.SettleTPM(ctx, tpdBackendKey(keyID), deltaIncrement, realTokens)
		return
	}
	bucket := l.tpdBuckets[keyID]
	l.mu.RUnlock()
	if bucket == nil {
		return
	}
	bucket.ReconcileTPM(reservedTokens, reservationEpoch, realTokens)
}

// IncreaseReservationTPD is IncreaseReservationTPM for the tokens-per-day
// bucket -- the mid-stream top-up: a fresh AllowTPM for the delta on the
// day key in Redis mode, TokenBucket.IncreaseReservation in memory, a
// no-op (always allowed, values unchanged) when tokens_per_day isn't
// configured for keyID.
func (l *KeyLimiter) IncreaseReservationTPD(ctx context.Context, keyID string, currentReservedTokens, newReservedTokens float64, reservationEpoch int64) (allowed bool, appliedTokens float64, newReservationEpoch int64, err error) {
	if newReservedTokens <= currentReservedTokens {
		return true, currentReservedTokens, reservationEpoch, nil
	}
	l.mu.RLock()
	if l.backend != nil {
		cfg := l.configs[keyID]
		l.mu.RUnlock()
		burst, rate := tpdRedisParams(cfg)
		if burst <= 0 {
			return true, currentReservedTokens, reservationEpoch, nil
		}
		delta := newReservedTokens - currentReservedTokens
		allowedRedis, _, tpdErr := l.backend.AllowTPM(ctx, tpdBackendKey(keyID), burst, rate, 1.0, delta)
		if tpdErr != nil {
			return true, currentReservedTokens, reservationEpoch, tpdErr
		}
		if !allowedRedis {
			return false, currentReservedTokens, reservationEpoch, nil
		}
		return true, newReservedTokens, 0, nil
	}
	bucket := l.tpdBuckets[keyID]
	l.mu.RUnlock()
	if bucket == nil {
		return true, currentReservedTokens, reservationEpoch, nil
	}
	allowed, appliedTokens, newReservationEpoch = bucket.IncreaseReservation(currentReservedTokens, newReservedTokens, reservationEpoch)
	return allowed, appliedTokens, newReservationEpoch, nil
}

// Admit decides RPM, TPM and tokens-per-day in one step, in that order
// (round-5 item 10 phase 2b, 2026-10-17). In Redis mode it is ONE backend
// call: the gate script evaluates the three dimensions and reserves nothing
// unless all three admit (the RPM state is persisted on every call). In
// memory it is the three buckets as before, with one
// change for parity: a refused request consumes nothing -- the RPM token is
// refunded and the minute reservation released when a later dimension
// refuses. A backend error returns the zero Admission with the error; the
// caller fails open (nothing was reserved) and counts it once.
func (l *KeyLimiter) Admit(ctx context.Context, keyID, model string) (Admission, error) {
	if l.backend != nil {
		return l.admitRedis(ctx, keyID, model)
	}
	allowed, err := l.allow(ctx, keyID, model)
	if err != nil {
		return Admission{}, err
	}
	if !allowed {
		return Admission{RefusedBy: RefusedByRPM}, nil
	}
	tpmOK, tpmReserved, tpmTokens, tpmEpoch, _ := l.ReserveTPM(ctx, keyID, model)
	if !tpmOK {
		l.creditRPM(keyID, model)
		return Admission{RefusedBy: RefusedByTPM}, nil
	}
	dayOK, tpdReserved, tpdTokens, tpdEpoch, wait, _ := l.ReserveTPD(ctx, keyID, tpmTokens)
	if !dayOK {
		if tpmReserved {
			l.ReconcileTPM(ctx, keyID, model, tpmTokens, tpmEpoch, nil)
		}
		l.creditRPM(keyID, model)
		return Admission{RefusedBy: RefusedByTPD, RetryAfter: wait}, nil
	}
	return Admission{
		Allowed: true,
		TPM:     TokenReservation{Reserved: tpmReserved, Tokens: tpmTokens, Epoch: tpmEpoch},
		TPD:     TokenReservation{Reserved: tpdReserved, Tokens: tpdTokens, Epoch: tpdEpoch},
	}, nil
}

// PlanAdmit builds the gate request for keyID and model from the key's
// config -- the RPM and TPM parts honour the per-model overrides exactly as
// allow and ReserveTPM do, the day part is key-level under tpdBackendKey with
// the 1/24 cold cap (dayColdStartCap's first term; the script lowers it to the
// minute reservation it has just sized). ok is false in memory mode, which
// has no Redis request. Admit sends this request; the fused admission planned for
// round-5 item 10 phase 2c-3 will compose it with the budget's.
func (l *KeyLimiter) PlanAdmit(keyID, model string) (admission.Request, bool) {
	if l.backend == nil {
		return admission.Request{}, false
	}
	l.mu.RLock()
	cfg := l.configs[keyID]
	l.mu.RUnlock()
	req := admission.Request{RPMKey: keyID, RPMCapacity: cfg.Capacity, RPMRefillPerSecond: cfg.RefillPerSecond}
	if model != "" {
		if override, ok := cfg.PerModel[model]; ok && override.Capacity > 0 {
			req.RPMKey, req.RPMCapacity, req.RPMRefillPerSecond = perModelBackendKey(keyID, model), override.Capacity, override.RefillPerSecond
		}
	}
	tpmBurst, tpmRate, tpmKey := resolveTPMRedisParams(keyID, model, cfg)
	req.TPM = admission.Dimension{Key: tpmKey}
	if tpmBurst > 0 {
		req.TPM = admission.Dimension{Key: tpmKey, Burst: tpmBurst, Rate: tpmRate, ColdStart: tpmBurst}
	}
	tpdBurst, tpdRate := tpdRedisParams(cfg)
	req.TPD = admission.Dimension{Key: tpdBackendKey(keyID)}
	if tpdBurst > 0 {
		req.TPD = admission.Dimension{Key: tpdBackendKey(keyID), Burst: tpdBurst, Rate: tpdRate, ColdStart: tpdBurst / dayColdStartHours}
	}
	return req, true
}

// MapAdmission turns the gate script's result for req into an Admission,
// including ReserveTPD's cold-refusal rule for the wait: a refusal whose
// reported cost is the cold start (within the %.14g rendering's tolerance)
// carries no wait, any other tokens-per-day refusal carries the script's.
// RPM and TPM refusals carry none. A reservation is reported only for a
// dimension that is on (Burst > 0).
func MapAdmission(req admission.Request, r admission.Result) Admission {
	if !r.Allowed {
		a := Admission{RefusedBy: r.RefusedBy}
		if r.RefusedBy == RefusedByTPD && !isColdRefusal(r.TPDCost, r.TPDColdStart) {
			a.RetryAfter = secondsDuration(r.RetryAfterSec)
		}
		return a
	}
	a := Admission{Allowed: true}
	if req.TPM.Burst > 0 {
		a.TPM = TokenReservation{Reserved: true, Tokens: r.TPMCost}
	}
	if req.TPD.Burst > 0 {
		a.TPD = TokenReservation{Reserved: true, Tokens: r.TPDCost}
	}
	return a
}

// admitRedis is the gate in one backend call: the planned request, the
// script, the mapped result.
func (l *KeyLimiter) admitRedis(ctx context.Context, keyID, model string) (Admission, error) {
	req, _ := l.PlanAdmit(keyID, model)
	r, err := l.backend.Admit(ctx, req)
	if err != nil {
		return Admission{}, err
	}
	return MapAdmission(req, r), nil
}

// creditRPM returns the RPM token a refused in-memory admission took, so the
// in-memory path matches the gate script: a refused request consumes nothing.
func (l *KeyLimiter) creditRPM(keyID, model string) {
	l.mu.RLock()
	var bucket *TokenBucket
	if model != "" {
		bucket = l.perModelBuckets[keyID][model]
	}
	if bucket == nil {
		bucket = l.buckets[keyID]
	}
	l.mu.RUnlock()
	if bucket != nil {
		bucket.Credit(1)
	}
}

// SettleTokens settles the TPM and tokens-per-day reservations of one request
// in one backend call (round-5 item 10 phase 2b): each configured dimension
// that holds a reservation, or has a real count to record even without one
// (the gate failed open), is settled -- a release when realTokens is nil --
// with ReconcileTPM's and ReconcileTPD's own arithmetic; nothing to settle
// means no call. In memory it is the two bucket reconciles. Backend errors
// are discarded as the two single settles' were (FAILURE-MODES, known gaps).
func (l *KeyLimiter) SettleTokens(ctx context.Context, keyID, model string, tpm, tpd TokenReservation, realTokens *float64) {
	if l.backend == nil {
		if tpm.Reserved || realTokens != nil {
			l.ReconcileTPM(ctx, keyID, model, tpm.Tokens, tpm.Epoch, realTokens)
		}
		if tpd.Reserved || realTokens != nil {
			l.ReconcileTPD(ctx, keyID, tpd.Tokens, tpd.Epoch, realTokens)
		}
		return
	}
	entries := l.PlanSettle(keyID, model, tpm, tpd, realTokens)
	if len(entries) == 0 {
		return
	}
	_ = l.backend.SettleTokens(ctx, entries)
}

// RedisBacked reports whether the limiter's counters live in Redis (a limiter
// built by NewRedisKeyLimiter), as opposed to this process's memory.
func (l *KeyLimiter) RedisBacked() bool { return l.backend != nil }

// PlanSettle returns the settlement entries SettleTokens sends in Redis
// mode: each configured dimension that holds a reservation, or has a real
// count to record without one, with ReconcileTPM's and ReconcileTPD's own
// arithmetic (a release when realTokens is nil). Nil when nothing needs
// settling, and in memory mode, which has no Redis settlement. The fused
// settlement (internal/redisfused, round-5 item 10 phase 2c-2) composes these
// entries with the budget's and the ledger's.
func (l *KeyLimiter) PlanSettle(keyID, model string, tpm, tpd TokenReservation, realTokens *float64) []admission.Settlement {
	if l.backend == nil {
		return nil
	}
	l.mu.RLock()
	cfg := l.configs[keyID]
	l.mu.RUnlock()
	var real float64
	if realTokens != nil {
		real = *realTokens
	}
	var entries []admission.Settlement
	if burst, rate, key := resolveTPMRedisParams(keyID, model, cfg); burst > 0 && (tpm.Reserved || realTokens != nil) {
		entries = append(entries, admission.Settlement{Key: key, DeltaIncrement: (1000.0 / rate) * (real - tpm.Tokens), RealTokens: realTokens})
	}
	if burst, rate := tpdRedisParams(cfg); burst > 0 && (tpd.Reserved || realTokens != nil) {
		entries = append(entries, admission.Settlement{Key: tpdBackendKey(keyID), DeltaIncrement: (1000.0 / rate) * (real - tpd.Tokens), RealTokens: realTokens})
	}
	return entries
}

// resolveTPMBucket resolves keyID's TPM bucket for model — its own
// per-model override if one is configured, else keyID's key-level
// default TPM bucket, else nil (no TPM configured at all). Callers MUST
// already hold l.mu (read or write) — mirrors allow()'s identical
// per-model-then-default resolution pattern for the RPM dimension.
func (l *KeyLimiter) resolveTPMBucket(keyID, model string) *TokenBucket {
	var bucket *TokenBucket
	if model != "" {
		bucket = l.perModelTPMBuckets[keyID][model]
	}
	if bucket == nil {
		bucket = l.tpmBuckets[keyID]
	}
	return bucket
}

// Register upserts cfg's rate-limit parameters for one key, live: a new
// in-memory TokenBucket (in-memory mode, replacing any existing bucket for
// this ID outright — an explicit admin update resetting the key to full
// burst capacity is the correct, intended effect, not a bug) or a new
// configs entry (Redis mode, read by every subsequent Allow call for this
// ID). Thread-safe — see docs/rfcs/2026-09-05-gateway-admin-api.md for why
// this exists: a virtual key added or updated via the live admin API must
// have its rate-limit entry registered here BEFORE the corresponding
// identity.Verifier swap that makes the ID resolvable at all, closing the
// nil-bucket/zero-capacity hazard Allow's own doc comment above names.
func (l *KeyLimiter) Register(cfg KeyConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.backend != nil {
		l.configs[cfg.ID] = cfg
		return
	}
	l.configs[cfg.ID] = cfg
	l.buckets[cfg.ID] = NewTokenBucket(cfg.Capacity, cfg.RefillPerSecond)
	// TPM specifically (never RPM above) uses Reset on the EXISTING
	// bucket object, when one already exists, rather than replacing it
	// outright — a real bug an audit found: ReserveTPM/ReconcileTPM/
	// IncreaseReservationTPM all re-resolve "the current bucket for
	// keyID/model" via a fresh map lookup on every call (see
	// resolveTPMBucket), rather than holding the specific *TokenBucket
	// instance an in-flight reservation was actually taken from — TPM
	// reservations stay open for a request's/stream's ENTIRE duration
	// (opened once in checkRateLimit, reconciled only in finalize), a
	// window a live admin Register call can easily land inside. If
	// Register replaced the map entry with a brand-new object, a
	// ReconcileTPM call landing after that swap would misapply its
	// credit/debit to the NEW (wrong) object while the real, correctly-
	// debited old object was silently discarded, unreconciled forever.
	// Reset produces the exact same "resets the key to full burst
	// capacity" effect the RPM bucket's own replacement already
	// documents as deliberate, just without discarding the object
	// identity any outstanding reservation already resolved to. RPM
	// itself has no equivalent hazard — Allow/AllowForModel are
	// synchronous, single-call checks with no reserve-now/reconcile-
	// later window, so replacing its bucket outright on every Register
	// call remains correct and unchanged here.
	if cfg.TPMCapacity > 0 {
		if existing := l.tpmBuckets[cfg.ID]; existing != nil {
			existing.Reset(cfg.TPMCapacity, cfg.TPMRefillPerSecond)
		} else {
			l.tpmBuckets[cfg.ID] = NewTokenBucket(cfg.TPMCapacity, cfg.TPMRefillPerSecond)
		}
	} else {
		// An update disabling TPM (or one that never had it) must not
		// leave a stale bucket behind — a previously-registered TPM
		// bucket the caller no longer configures must actually stop
		// being enforced, not silently keep applying an old limit.
		delete(l.tpmBuckets, cfg.ID)
	}
	// The tokens-per-day bucket follows the TPM rules exactly, one
	// dimension over: Reset in place (its reservations stay open for a
	// whole request too), and an update without tokens_per_day removes it.
	if cfg.TokensPerDay > 0 {
		if existing := l.tpdBuckets[cfg.ID]; existing != nil {
			existing.Reset(cfg.TokensPerDay, cfg.TokensPerDay/secondsPerDay)
		} else {
			l.tpdBuckets[cfg.ID] = newDayBucket(cfg.TokensPerDay)
		}
	} else {
		delete(l.tpdBuckets, cfg.ID)
	}
	// Same "an update must not leave a stale entry behind" rule as TPM
	// above, applied per-model: cfg.PerModel is always treated as the
	// COMPLETE, authoritative set for cfg.ID going forward, so a model
	// present in the old registration but absent (or now Capacity <= 0)
	// in this one must stop being enforced, not keep applying its old
	// limit indefinitely.
	if models := buildPerModelBuckets(cfg.PerModel); len(models) > 0 {
		l.perModelBuckets[cfg.ID] = models
	} else {
		delete(l.perModelBuckets, cfg.ID)
	}
	// Same rule, same reason, for the per-model TPM dimension — and the
	// same Reset-in-place fix as the key-level TPM bucket above, per
	// model, for the identical reservation-race reason.
	if models := updatePerModelTPMBuckets(l.perModelTPMBuckets[cfg.ID], cfg.PerModel); len(models) > 0 {
		l.perModelTPMBuckets[cfg.ID] = models
	} else {
		delete(l.perModelTPMBuckets, cfg.ID)
	}
}

// Config returns the currently-registered KeyConfig for keyID, and
// whether one exists at all — exposes the same l.configs lookup
// allow/AllowForModel already do internally, for a caller that needs to
// read back a key's REAL, currently-effective rate-limit config (e.g.
// dataplane.Pipeline.RotateVirtualKey's own publish path, which must
// republish the real config rather than guessing or omitting it — see
// that method's own doc comment). Always backed by the in-memory
// l.configs map, even in Redis mode (Redis only stores token-bucket
// STATE; the config itself lives here regardless — see allow's own
// l.backend != nil branch), so this is accurate in either mode.
func (l *KeyLimiter) Config(keyID string) (KeyConfig, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cfg, ok := l.configs[keyID]
	return cfg, ok
}

// Close releases the backend's resources, if any. A no-op in in-memory
// mode (TokenBucket owns nothing that needs closing).
func (l *KeyLimiter) Close() error {
	if l.backend != nil {
		return l.backend.Close()
	}
	return nil
}
