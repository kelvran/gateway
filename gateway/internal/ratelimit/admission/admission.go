// Package admission holds the value types the rate-limit gate exchanges with
// its Redis backend (round-5 item 10 phase 2b, 2026-10-17): one Request for
// the three dimensions a chat request is admitted through -- the RPM token
// bucket, the TPM reservation and the tokens-per-day reservation -- one
// Result, and the Settlement entries the post-call settle takes.
//
// It is a leaf on purpose. ratelimit.RedisBackend lives in the consumer and
// ratelimit/redislimiter satisfies it structurally without importing
// ratelimit (see .go-arch-lint.yml's note on the idiom); a method that
// carries eight numbers and three keys needs named types, and a leaf both
// sides import keeps that idiom intact.
package admission

// Dimension is one GCRA token dimension of a Request. Burst <= 0 means the
// dimension is not configured for the key: the gate skips it and the Result
// reports 0 for it.
type Dimension struct {
	// Key is the backend's logical key (the virtual key id, or its per-model
	// or per-day form); the backend derives the Redis key names from it.
	Key string
	// Burst is the GCRA burst (tpm_capacity or tokens_per_day), Rate the
	// refill per second.
	Burst, Rate float64
	// ColdStart is the reservation for a key with no settled history: the
	// burst for the minute dimension; for the day dimension the cap
	// (tokens_per_day / 24) that the gate lowers to the minute dimension's
	// reservation when that is smaller -- ratelimit.dayColdStartCap, inside
	// the script, because the day decision needs the minute decision's cost.
	ColdStart float64
}

// Request is one admission over the three dimensions, in today's order:
// RPM, then TPM, then tokens-per-day. The gate reserves nothing unless all
// three admit -- no RPM token taken, no TAT written -- so a refused request
// consumes nothing; the RPM hash's refreshed state is still persisted on
// every call (a TTL refresh), as the single RPM script always did.
type Request struct {
	RPMKey             string
	RPMCapacity        float64
	RPMRefillPerSecond float64
	TPM                Dimension
	TPD                Dimension
}

// Result is the gate's decision.
type Result struct {
	Allowed bool
	// RefusedBy names the first dimension that refused ("rpm", "tpm" or
	// "tpd"); empty when Allowed.
	RefusedBy string
	// RetryAfterSec is the refusing GCRA dimension's wait; 0 for an RPM
	// refusal (the caller's backoff decides) and on an admit.
	RetryAfterSec float64
	// TPMCost and TPDCost are the costs the two dimensions evaluated -- the
	// settled mean, or the cold start -- and reserved when Allowed; 0 for a
	// dimension that is off or was not reached.
	TPMCost, TPDCost float64
	// TPDColdStart is the cold start the day dimension used, so the caller
	// can recognise a cold refusal (ratelimit.isColdRefusal) and report no
	// wait for it, as before.
	TPDColdStart float64
}

// Settlement is one dimension's post-call settlement: the TAT moves by
// DeltaIncrement (emission interval times real minus reserved tokens) and,
// when RealTokens is set, the settled history learns the real count.
type Settlement struct {
	Key            string
	DeltaIncrement float64
	RealTokens     *float64
}
