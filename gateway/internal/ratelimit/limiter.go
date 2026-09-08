package ratelimit

import (
	"context"
	"sync"
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
	// codebase's existing "0/negative = unlimited" convention. In-memory
	// mode only in v1; a no-op under NewRedisKeyLimiter, per that RFC's
	// explicit scope limit.
	TPMCapacity        float64
	TPMRefillPerSecond float64
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
	// in BOTH in-memory and Redis-backed mode (unlike TPMCapacity above,
	// which is in-memory-only): RedisBackend.Allow already accepts an
	// arbitrary per-call capacity/refill and key string, so no change to
	// that interface, or to internal/ratelimit/redislimiter, was needed
	// to support this second mode.
	PerModel map[string]ModelRateLimit
}

// ModelRateLimit is one virtual key's per-model RPM override — see
// KeyConfig.PerModel's doc comment for the full design.
type ModelRateLimit struct {
	Capacity        float64
	RefillPerSecond float64
}

// RedisBackend is implemented by internal/ratelimit/redislimiter.Limiter.
// Optional — a KeyLimiter built via NewInMemoryKeyLimiter never touches
// this, mirroring internal/budget.Store's relationship to
// internal/budget/boltstore: the interface lives here, in the consumer
// package, and the implementation package does not import this one.
type RedisBackend interface {
	Allow(ctx context.Context, keyID string, capacity, refillPerSecond float64) (bool, error)
	Close() error
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
	// perModelBuckets is keyID -> model -> that model's own TokenBucket,
	// in-memory mode only (non-nil whenever backend == nil, mirroring
	// buckets/tpmBuckets above). Redis mode instead reads cfg.PerModel
	// straight out of configs on every AllowForModel call — no separate
	// bucket-tracking structure needed there, since RedisBackend.Allow
	// takes capacity/refill per call rather than owning bucket state
	// itself.
	perModelBuckets map[string]map[string]*TokenBucket
	backend         RedisBackend // non-nil in Redis mode only
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
	perModelBuckets := make(map[string]map[string]*TokenBucket, len(keys))
	for _, k := range keys {
		configs[k.ID] = k
		buckets[k.ID] = NewTokenBucket(k.Capacity, k.RefillPerSecond)
		if k.TPMCapacity > 0 {
			tpmBuckets[k.ID] = NewTokenBucket(k.TPMCapacity, k.TPMRefillPerSecond)
		}
		if models := buildPerModelBuckets(k.PerModel); len(models) > 0 {
			perModelBuckets[k.ID] = models
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
	return &KeyLimiter{configs: configs, buckets: buckets, tpmBuckets: tpmBuckets, perModelBuckets: perModelBuckets}
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
// TPMCapacity <= 0, or a Redis-mode KeyLimiter, where TPM is a deliberate
// no-op in v1 per docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md's scope
// limit).
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
// reserved) when TPM isn't configured for keyID, exactly mirroring
// AllowTPM's own "no entry means unlimited" behavior. reserved is false
// whenever nothing was actually reserved against a real bucket — either
// TPM isn't configured, or the bucket's own check rejected the request —
// so a caller can gate whether a later ReconcileTPM call is even
// meaningful without needing to inspect reservedTokens itself.
//
// Every true `reserved` return MUST be paired with exactly one
// ReconcileTPM call for the same keyID/reservedTokens, even on an
// error/timeout path.
func (l *KeyLimiter) ReserveTPM(keyID string) (allowed bool, reserved bool, reservedTokens float64) {
	l.mu.RLock()
	bucket := l.tpmBuckets[keyID]
	l.mu.RUnlock()
	if bucket == nil {
		return true, false, 0
	}
	allowed, reservedTokens = bucket.ReserveTPM()
	return allowed, allowed, reservedTokens
}

// ReconcileTPM undoes a previous ReserveTPM call's provisional debit and,
// if realTokens is non-nil, debits the real usage in its place — a
// no-op, matching RecordTokens' own existing "no TPM bucket for keyID"
// behavior, when TPM isn't configured for keyID (including a Redis-mode
// KeyLimiter, where TPM is a deliberate v1 no-op per
// docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md) — safe to call
// unconditionally with whatever reservedTokens a prior ReserveTPM call
// returned, even 0, since ReconcileTPM(0, nil) against a real bucket is
// itself a genuine no-op (tokens += 0).
func (l *KeyLimiter) ReconcileTPM(keyID string, reservedTokens float64, realTokens *float64) {
	l.mu.RLock()
	bucket := l.tpmBuckets[keyID]
	l.mu.RUnlock()
	if bucket == nil {
		return
	}
	bucket.ReconcileTPM(reservedTokens, realTokens)
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
	if cfg.TPMCapacity > 0 {
		l.tpmBuckets[cfg.ID] = NewTokenBucket(cfg.TPMCapacity, cfg.TPMRefillPerSecond)
	} else {
		// An update disabling TPM (or one that never had it) must not
		// leave a stale bucket behind — a previously-registered TPM
		// bucket the caller no longer configures must actually stop
		// being enforced, not silently keep applying an old limit.
		delete(l.tpmBuckets, cfg.ID)
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
}

// Close releases the backend's resources, if any. A no-op in in-memory
// mode (TokenBucket owns nothing that needs closing).
func (l *KeyLimiter) Close() error {
	if l.backend != nil {
		return l.backend.Close()
	}
	return nil
}
