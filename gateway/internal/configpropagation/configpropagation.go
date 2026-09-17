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
// v1 scope: only deployment-weight mutations (TypeDeploymentWeight) —
// the envelope (MutationEvent) is deliberately generic enough that
// virtual-key/prompt-mutation propagation is a natural, disclosed
// follow-on (same channel, new Type cases, same apply-via-existing-
// local-function shape), not a redesign.
//
// This package never imports gateway/internal/gateway/dataplane — the
// same interface-lives-in-the-consumer idiom internal/ratelimit/
// redislimiter and internal/idempotency/inprocess already establish:
// dataplane.Pipeline holds this package's Publisher/Subscriber
// interfaces, never the other way around.
package configpropagation

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/redis/go-redis/v9"
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
type MutationEvent struct {
	Type             string          `json:"type"`
	OriginInstanceID string          `json:"origin_instance_id"`
	Payload          json.RawMessage `json:"payload"`
}

// DeploymentWeightPayload is MutationEvent.Payload's shape when Type ==
// TypeDeploymentWeight.
type DeploymentWeightPayload struct {
	Model          string `json:"model"`
	DeploymentName string `json:"deployment_name"`
	Weight         int    `json:"weight"`
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
	client *redis.Client
}

// Open constructs a Publisher/Subscriber pair against the Redis server
// at addr ("host:port"). go-redis dials lazily — mirroring
// internal/ratelimit/redislimiter.Open's own identical "never fails on
// an unreachable addr" contract, confirmed by that package's own
// TestOpenNeverFailsOnUnreachableAddr — so an unreachable addr does not
// make Open itself fail; the first real connection attempt happens on
// the first Publish/Subscribe call.
func Open(addr string) *PubSub {
	return &PubSub{client: redis.NewClient(&redis.Options{Addr: addr})}
}

// Close releases the underlying *redis.Client's own resources, mirroring
// internal/ratelimit/redislimiter.Limiter.Close's identical convention.
func (r *PubSub) Close() error {
	return r.client.Close()
}

// Publish marshals event as JSON and publishes it on channelName.
func (r *PubSub) Publish(ctx context.Context, event MutationEvent) error {
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
			onEvent(event)
		}
	}
}
