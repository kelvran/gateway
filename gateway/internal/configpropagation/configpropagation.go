// Package configpropagation pushes a live admin-API mutation made on one
// gateway instance to every OTHER instance sharing the same Redis
// address, via a single pub/sub channel — closing the real gap named in
// gateway/ARCHITECTURE.md: every existing admin mutation (virtual keys,
// prompts, deployment weight) is in-memory-only and single-process, so
// instance A applying a mutation via its own Admin API leaves instances
// B..N with no way to learn about it at all.
//
// Deliberately a push design, not the poll-based alternative a prior
// research pass recommended (docs/upgrade-research/progressive-rollout-
// canary-config-2026-09-15.md Finding 4, citing LiteLLM's own precedent)
// — a real, disclosed trade-off: Redis pub/sub delivery is fire-and-
// forget with no replay, so an instance disconnected during a publish
// permanently misses that specific message (see Subscribe's own doc
// comment). Chosen anyway per an explicit operator decision favoring
// lower propagation latency over the poll design's own eventual-
// consistency guarantee.
//
// v1 scope named deployment-weight mutations (TypeDeploymentWeight) as
// the only real Type — the envelope (MutationEvent) was deliberately
// generic enough that virtual-key-mutation propagation would be a
// natural, disclosed follow-on (same channel, new Type cases, same
// apply-via-existing-local-function shape), not a redesign. That
// follow-on is now real: TypeVirtualKeyUpsert/TypeVirtualKeyDelete close
// the identical live cross-instance gap for
// dataplane.Pipeline.UpsertVirtualKey/DeleteVirtualKey/RotateVirtualKey
// — found necessary (not merely nice-to-have) while migrating identity's
// own hot state to Redis: identity.Store (see internal/identity/
// redisstore) only ever answers "what does a freshly (re)started
// replica load," never "how does an already-running replica learn about
// another instance's live admin mutation" — the Verify hot path resolves
// against a purely LOCAL, already-built *identity.Verifier per
// instance, so a stale replica would keep authenticating against a
// revoked/rotated credential, or keep rejecting a brand-new one, until
// its own restart. Prompt-mutation propagation remains the one
// still-undone piece of the original follow-on note.
//
// This package never imports gateway/internal/gateway/dataplane,
// internal/identity, or internal/ratelimit — the same interface-lives-
// in-the-consumer idiom internal/ratelimit/redislimiter and
// internal/idempotency/inprocess already establish: dataplane.Pipeline
// holds this package's Publisher/Subscriber interfaces (and does the
// VirtualKey/KeyConfig <-> payload conversion at its own call sites),
// never the other way around.
//
// Every MutationEvent is HMAC-signed and verified with a shared secret
// (see MutationEvent.Signature) — Publish/Subscribe both refuse to run
// without one. This closes a CRITICAL gap this package originally
// shipped with: the channel carries live virtual-key mutation authority
// (a forged event can mint/hijack/delete any tenant's credential), but
// had no authentication at all until this was fixed, since Redis
// network access alone was sufficient to publish an event every
// subscribed instance would apply unconditionally.
package configpropagation

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
)

// channelName is the single, fixed Redis pub/sub channel every gateway
// instance publishes to and subscribes on — no per-deployment or
// per-model channel fan-out, since the volume of admin mutations (a
// human-driven, low-frequency operation) never approaches a scale where
// channel-per-topic would matter.
const channelName = "kelvran:config:mutations"

// TypeDeploymentWeight is MutationEvent.Type's only real value in v1 —
// see this package's own doc comment for why the envelope is scoped
// narrower than its own shape allows.
const TypeDeploymentWeight = "deployment_weight"

// MutationEvent is the discriminated envelope published on channelName.
// OriginInstanceID (telemetry.InstanceID, set by the publisher) lets a
// subscriber skip an event it itself originated — the instance that
// made the mutation already applied it locally before ever publishing,
// so re-applying its own echo would be redundant, not merely harmless.
//
// PublishedAtUnixNano, added 2026-09-20, is a cross-cutting last-writer-
// wins ordering token (the publishing instance's own wall-clock time,
// nanosecond resolution, at the moment the mutation was decided) —
// deliberately placed on this generic envelope, not duplicated inside
// each Type's own payload, mirroring OriginInstanceID's own placement:
// both are properties of "when/where this mutation happened," equally
// applicable to whichever Type a future mutation kind adds, not specific
// to deployment-weight payloads. Closes a real gap found by this
// session's own end-to-end audit: without ANY ordering token, two
// concurrent mutations to the same target could leave different
// instances converged on different final values, with zero detection —
// see dataplane.Pipeline.applyWeightIfNewer, this token's one real
// consumer today.
// Signature is an HMAC-SHA256 MAC over
// Type+"."+OriginInstanceID+"."+PublishedAtUnixNano+"."+Payload, computed
// and verified with a shared secret every gateway instance holds — see
// signEvent/verifyEvent. Format ("v1,<base64>") deliberately reuses
// internal/alerting's own Standard-Webhooks-style convention
// (signPayload) rather than inventing a fresh one.
//
// Added 2026-09-21 closing a CRITICAL finding from this session's own
// end-to-end production audit: this channel previously carried
// TypeVirtualKeyUpsert/TypeVirtualKeyDelete mutations with ZERO
// authentication — any process with network access to the shared Redis
// (no AUTH/TLS existed on any Redis-backed subsystem at the time) could
// publish a forged MutationEvent and have every OTHER instance apply it
// via applyVirtualKeyUpsert's last-writer-wins merge, fully bypassing
// every admin.go bearer-token tier. Open/Publish/Subscribe below now all
// treat a missing signing secret as a hard error rather than a silently
// degraded posture — unlike internal/alerting's OWN "signing optional"
// stance, an unsigned receiver here is never a legitimate choice, since
// the only subscriber is this same codebase, not a third-party endpoint
// with no verification concept of its own.
type MutationEvent struct {
	Type                string          `json:"type"`
	OriginInstanceID    string          `json:"origin_instance_id"`
	PublishedAtUnixNano int64           `json:"published_at_unix_nano"`
	Payload             json.RawMessage `json:"payload"`
	Signature           string          `json:"signature,omitempty"`
}

// signingInput returns the exact byte sequence signEvent/verifyEvent MAC
// over — every field of event EXCEPT Signature itself, joined with "."
// separators, mirroring internal/alerting.signPayload's identical
// "id.timestamp.body" canonicalization.
func signingInput(event MutationEvent) []byte {
	return []byte(event.Type + "." + event.OriginInstanceID + "." + strconv.FormatInt(event.PublishedAtUnixNano, 10) + "." + string(event.Payload))
}

func signEvent(secret []byte, event MutationEvent) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(signingInput(event))
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// verifyEvent reports whether event.Signature is a valid MAC for its own
// content under secret. hmac.Equal (constant-time) guards against a
// timing side-channel on the comparison itself.
func verifyEvent(secret []byte, event MutationEvent) bool {
	if event.Signature == "" {
		return false
	}
	return hmac.Equal([]byte(signEvent(secret, event)), []byte(event.Signature))
}

// DeploymentWeightPayload is MutationEvent.Payload's shape when Type ==
// TypeDeploymentWeight.
type DeploymentWeightPayload struct {
	Model          string `json:"model"`
	DeploymentName string `json:"deployment_name"`
	Weight         int    `json:"weight"`
}

// TypeVirtualKeyUpsert is MutationEvent.Type's value for a live
// virtual-key create/update — published by BOTH
// dataplane.Pipeline.UpsertVirtualKey and RotateVirtualKey, since a
// rotation's net effect on the receiving end is identical to an
// upsert: "this ID's virtual key now looks exactly like this," never a
// distinct operation a remote replica needs to replay semantically —
// see VirtualKeyPayload.RateLimitConfig's own doc comment for why both
// origins send it non-nil.
const TypeVirtualKeyUpsert = "virtual_key_upsert"

// TypeVirtualKeyDelete is MutationEvent.Type's value for a live
// virtual-key deletion.
const TypeVirtualKeyDelete = "virtual_key_delete"

// VirtualKeyPayload mirrors identity.VirtualKey's own field set
// structurally — this package deliberately never imports
// internal/identity (see this package's own doc comment); dataplane.go
// converts to/from the real type at its own call sites. decimal.Decimal/
// time.Duration/time.Time are vendor/stdlib types, not a project-
// internal dependency, so using them here directly (rather than
// re-encoding as a plain string/int64) loses no precision and needs no
// extra conversion step of its own.
//
// AllowedModels/AllowedRegions are []string here, not
// identity.VirtualKey's own map[string]struct{} — a set's membership,
// not its Go representation, is what needs to cross the wire; dataplane.go
// converts between the two shapes at its own call sites.
type VirtualKeyPayload struct {
	ID                       string          `json:"id"`
	KeyHash                  string          `json:"key_hash"`
	BudgetUSD                decimal.Decimal `json:"budget_usd"`
	BudgetResetInterval      time.Duration   `json:"budget_reset_interval"`
	BudgetWarnPercent        float64         `json:"budget_warn_percent"`
	AllowedModels            []string        `json:"allowed_models,omitempty"`
	AllowedRegions           []string        `json:"allowed_regions,omitempty"`
	RateLimitBurst           float64         `json:"rate_limit_burst"`
	RateLimitRefill          float64         `json:"rate_limit_refill"`
	MaxConcurrentRequests    int             `json:"max_concurrent_requests"`
	PreviousKeyHash          string          `json:"previous_key_hash,omitempty"`
	PreviousKeyHashExpiresAt time.Time       `json:"previous_key_hash_expires_at,omitempty"`
	BillingSubjectID         string          `json:"billing_subject_id,omitempty"`
}

// ModelRateLimitPayload mirrors ratelimit.ModelRateLimit's own field set
// structurally, for the identical never-import-the-consumer-package
// reason VirtualKeyPayload's own doc comment gives.
type ModelRateLimitPayload struct {
	Capacity           float64 `json:"capacity"`
	RefillPerSecond    float64 `json:"refill_per_second"`
	TPMCapacity        float64 `json:"tpm_capacity"`
	TPMRefillPerSecond float64 `json:"tpm_refill_per_second"`
}

// KeyConfigPayload mirrors ratelimit.KeyConfig's own field set
// structurally, for the identical reason. ID is deliberately omitted —
// VirtualKeyUpsertPayload.VirtualKey.ID is already the same value
// ratelimit.KeyConfig.ID must carry; dataplane.go's own conversion
// reuses it rather than encoding it twice.
type KeyConfigPayload struct {
	Capacity           float64                          `json:"capacity"`
	RefillPerSecond    float64                          `json:"refill_per_second"`
	TPMCapacity        float64                          `json:"tpm_capacity"`
	TPMRefillPerSecond float64                          `json:"tpm_refill_per_second"`
	PerModel           map[string]ModelRateLimitPayload `json:"per_model,omitempty"`
}

// VirtualKeyUpsertPayload is MutationEvent.Payload's shape when Type ==
// TypeVirtualKeyUpsert.
type VirtualKeyUpsertPayload struct {
	VirtualKey VirtualKeyPayload `json:"virtual_key"`
	// RateLimitConfig, when non-nil, is the FULL rate-limit config to
	// register on every OTHER instance. Both a real UpsertVirtualKey-
	// originated event AND a RotateVirtualKey-originated one now send
	// this non-nil (RotateVirtualKey reads back its own currently-
	// registered config via ratelimit.KeyLimiter.Config before
	// publishing — rotation never changes rate-limit config, so the
	// originating instance's own copy is always the authoritative
	// current value). Fixed 2026-09-21 closing a real gap this session's
	// own end-to-end audit found: a receiving replica that never
	// independently learned about this key ID (configpropagation's
	// pub/sub is fire-and-forget with no replay) had NO rate-limit
	// registration to fall back on when a rotation-originated event
	// carried nil, and ratelimit.KeyLimiter.Allow's own nil-bucket/
	// zero-capacity branch cannot distinguish "never registered" from
	// "registered with Capacity 0" — every request from that key was
	// silently denied on that one replica, despite authenticating fine.
	// Nil is still a valid, handled input on the receiving end
	// (applyVirtualKeyUpsert never overwrites an already-registered
	// config with nil) — kept as a defensive property, not because any
	// current publisher still sends it.
	RateLimitConfig *KeyConfigPayload `json:"rate_limit_config,omitempty"`
}

// VirtualKeyDeletePayload is MutationEvent.Payload's shape when Type ==
// TypeVirtualKeyDelete.
type VirtualKeyDeletePayload struct {
	ID string `json:"id"`
}

// Publisher publishes a MutationEvent for every other subscribed
// instance to receive. A Publish failure (a Redis network/timeout error)
// must never block or fail the LOCAL mutation that triggered it — see
// dataplane.Pipeline.UpdateDeploymentWeight's own call site, which
// mirrors internal/ratelimit/redislimiter's established fail-open
// posture (docs/rfcs/2026-09-03-distributed-rate-limiting.md) for the
// identical reason: a down Redis is a propagation-latency problem for
// OTHER instances, never a reason to reject a request against THIS one.
type Publisher interface {
	Publish(ctx context.Context, event MutationEvent) error
}

// Subscriber delivers every MutationEvent received on channelName to
// onEvent, blocking until ctx is canceled. A malformed/undecodable
// message is skipped (logged by the caller, not this package, which has
// no logger dependency) rather than aborting the whole subscription —
// one corrupted event must never silently stop this instance from
// receiving every subsequent one.
//
// Fire-and-forget, no replay: an event published while this instance's
// subscription was down (process restart, network partition) is
// permanently missed — there is no backlog/history channel to catch up
// from. This is the real, accepted cost of choosing push over the
// poll-based alternative (see this package's own doc comment) — a
// disclosed limitation, not silently glossed over.
type Subscriber interface {
	Subscribe(ctx context.Context, onEvent func(MutationEvent)) error
}

// PubSub implements both Publisher and Subscriber over a single
// shared *redis.Client.
type PubSub struct {
	client        *redis.Client
	signingSecret []byte
}

// Open constructs a Publisher/Subscriber pair against the Redis server
// described by opts (go-redis's own canonical connection-options type —
// see internal/ratelimit/redislimiter.Open's identical doc comment for
// why AUTH/TLS support lives here as the full redis.Options, not a bare
// addr string). go-redis dials lazily — mirroring
// internal/ratelimit/redislimiter.Open's own identical "never fails on
// an unreachable addr" contract, confirmed by that package's own
// TestOpenNeverFailsOnUnreachableAddr — so an unreachable addr does not
// make Open itself fail; the first real connection attempt happens on
// the first Publish/Subscribe call.
//
// signingSecret is the shared HMAC key every MutationEvent is signed
// and verified with (see MutationEvent.Signature's own doc comment). An
// empty signingSecret is accepted here (Open, like the rest of this
// constructor family, never fails) but makes both Publish and Subscribe
// refuse to run — cmd/gateway's own startup validation is the intended
// place a misconfiguration is caught, mirroring AdminConfig.TokenEnv's
// established "resolves empty -> fail startup" convention; this
// in-package refusal is defense-in-depth for any other caller.
func Open(opts redis.Options, signingSecret string) *PubSub {
	return &PubSub{client: redis.NewClient(&opts), signingSecret: []byte(signingSecret)}
}

// Close releases the underlying *redis.Client's own resources, mirroring
// internal/ratelimit/redislimiter.Limiter.Close's identical convention.
func (r *PubSub) Close() error {
	return r.client.Close()
}

// Publish signs event with signingSecret, marshals it as JSON, and
// publishes it on channelName. Refuses (without touching Redis at all)
// when signingSecret is empty — see Open's own doc comment for why an
// unsigned publish is never an accepted degraded mode for this channel;
// per Publisher's own doc comment, this error is expected to be logged
// by the caller, never treated as a reason to fail the LOCAL mutation
// that triggered it.
func (r *PubSub) Publish(ctx context.Context, event MutationEvent) error {
	if len(r.signingSecret) == 0 {
		return fmt.Errorf("configpropagation: refusing to publish an unsigned mutation event -- no signing secret configured")
	}
	event.Signature = signEvent(r.signingSecret, event)
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("configpropagation: marshaling event: %w", err)
	}
	if err := r.client.Publish(ctx, channelName, body).Err(); err != nil {
		return fmt.Errorf("configpropagation: publishing: %w", err)
	}
	return nil
}

// Subscribe blocks, delivering every MutationEvent received on
// channelName to onEvent, until ctx is canceled — see Subscriber's own
// doc comment for the fire-and-forget/no-replay contract this
// implements.
func (r *PubSub) Subscribe(ctx context.Context, onEvent func(MutationEvent)) error {
	if len(r.signingSecret) == 0 {
		return fmt.Errorf("configpropagation: refusing to subscribe without a signing secret -- would apply unauthenticated remote mutations")
	}
	sub := r.client.Subscribe(ctx, channelName)
	defer func() { _ = sub.Close() }()

	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			var event MutationEvent
			if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
				continue
			}
			// An invalid/missing signature is treated exactly like a
			// malformed/undecodable message (skipped, not applied) --
			// this is the actual fix for the CRITICAL finding this
			// field exists to close: a forged event with no valid MAC
			// under this instance's shared secret must never reach
			// onEvent, since onEvent's real callers (dataplane.go's
			// applyVirtualKeyUpsert/applyVirtualKeyDelete) apply it with
			// full admin authority.
			if !verifyEvent(r.signingSecret, event) {
				continue
			}
			onEvent(event)
		}
	}
}
