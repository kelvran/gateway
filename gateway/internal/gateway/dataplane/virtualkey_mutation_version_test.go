package dataplane

// Root cause of the CI-only failure TestConcurrentDeleteVirtualKeyRequests-
// ForSameNameNeverBothSucceed (internal/admin) kept hitting -- recorded as
// "not root-caused" on 2026-09-22 and finally pinned on 2026-10-07 when it
// failed the gateway/v0.17.0 tag run: the LOCAL admin mutation entry points
// (UpsertVirtualKey / DeleteVirtualKey / RotateVirtualKey /
// UpdateDeploymentWeight) read p.now() BEFORE taking the mutation lock, and
// the stale-version guard inside the locked apply path (publishedAt <
// versions[id] -> return nil) -- which exists for cross-replica
// last-writer-wins convergence and is right for a REMOTE mutation arriving
// late -- then treats a local call whose clock read lost the race to the
// lock as a successful no-op. For a delete that is two 204s and two audit
// entries for one deletion; for an upsert it is a later-arriving local
// write silently discarded while its caller is told it succeeded.
//
// These tests make the window deterministic by injecting a clock that reads
// BACKWARDS between two sequential local calls -- the same guard misfire
// the concurrent race produces, without needing the race. Before the fix
// every local-only test here fails; after it a local mutation's version is
// derived under the lock and bumped past anything this replica has already
// applied, so a local write can never be stale against the replica's own
// view. The cross-replica tests simulate several replicas in one process by
// giving pipelines distinct instanceIDs and feeding each one's captured
// published events to the others, exactly as cmd/gateway's subscriber
// does.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/configpropagation"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// backwardsClock returns a clock whose successive reads go DOWN by one
// second -- the pathological ordering a lost race to the mutation lock (or
// a wall-clock step) produces between two local calls.
func backwardsClock(start time.Time) func() time.Time {
	t := start
	return func() time.Time {
		t = t.Add(-time.Second)
		return t
	}
}

// capturingPublisher records every MutationEvent a pipeline publishes, so a
// test can read the version a LOCAL mutation went out under and replay the
// event into another pipeline as if it were a remote replica.
type capturingPublisher struct {
	mu     sync.Mutex
	events []configpropagation.MutationEvent
}

func (c *capturingPublisher) Publish(_ context.Context, event configpropagation.MutationEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
	return nil
}

// last returns the most recently published event of the given type.
func (c *capturingPublisher) last(t *testing.T, eventType string) configpropagation.MutationEvent {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.events) - 1; i >= 0; i-- {
		if c.events[i].Type == eventType {
			return c.events[i]
		}
	}
	t.Fatalf("no %s event was published (got %d events)", eventType, len(c.events))
	return configpropagation.MutationEvent{}
}

// versionClockStart is the first value the backwards clock hands out (minus
// one second); farFutureToken is a remote version comfortably ahead of it,
// standing in for a replica whose clock runs ahead of this one.
var (
	versionClockStart = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	farFutureToken    = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
)

// virtualKeyVersionTestReplica builds one simulated replica: a pipeline with
// its own instance ID, a backwards-reading clock and a capturing publisher.
func virtualKeyVersionTestReplica(t *testing.T, instanceID string) (*Pipeline, *capturingPublisher) {
	t.Helper()
	keys := []identity.VirtualKey{
		{ID: "vk-a", KeyHash: testHashOf("vk-a-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		{ID: "vk-b", KeyHash: testHashOf("vk-b-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	publisher := &capturingPublisher{}
	p := newVirtualKeyPropagationTestPipeline(t, keys, publisher)
	p.now = backwardsClock(versionClockStart)
	p.instanceID = instanceID
	return p, publisher
}

func virtualKeyVersionTestPipeline(t *testing.T) *Pipeline {
	t.Helper()
	p, _ := virtualKeyVersionTestReplica(t, "replica-a")
	return p
}

func upsertPayload(vk identity.VirtualKey) configpropagation.VirtualKeyUpsertPayload {
	return configpropagation.VirtualKeyUpsertPayload{VirtualKey: virtualKeyToPayload(vk)}
}

func decodeUpsertPayload(t *testing.T, event configpropagation.MutationEvent) configpropagation.VirtualKeyUpsertPayload {
	t.Helper()
	var payload configpropagation.VirtualKeyUpsertPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("decoding %s payload: %v", event.Type, err)
	}
	return payload
}

func keyHashOf(t *testing.T, p *Pipeline, id string) string {
	t.Helper()
	vk, ok := p.GetVirtualKey(id)
	if !ok {
		t.Fatalf("%s missing", id)
	}
	return vk.KeyHash
}

// TestDeleteVirtualKeyTwiceIsNotFoundEvenWhenTheClockReadsBackwards: the
// second local delete of an already-deleted key must report
// ErrVirtualKeyNotFound, never a silent success -- the invariant the admin
// package's concurrent-delete test asserts (exactly one 204).
func TestDeleteVirtualKeyTwiceIsNotFoundEvenWhenTheClockReadsBackwards(t *testing.T) {
	p := virtualKeyVersionTestPipeline(t)
	if err := p.DeleteVirtualKey("vk-a"); err != nil {
		t.Fatalf("first DeleteVirtualKey: %v", err)
	}
	err := p.DeleteVirtualKey("vk-a")
	if !errors.Is(err, ErrVirtualKeyNotFound) {
		t.Fatalf("second DeleteVirtualKey(vk-a) = %v, want ErrVirtualKeyNotFound — a stale-looking local call must not be reported as a successful no-op", err)
	}
	if _, ok := p.GetVirtualKey("vk-a"); ok {
		t.Fatal("vk-a still present after delete")
	}
}

// TestLaterLocalUpsertWinsEvenWhenTheClockReadsBackwards: a local upsert
// that arrives after another local upsert of the same key must be applied
// (last local writer wins), not dropped as "stale" because its wall-clock
// read happened to be older -- and must certainly not be dropped while
// returning nil.
func TestLaterLocalUpsertWinsEvenWhenTheClockReadsBackwards(t *testing.T) {
	p := virtualKeyVersionTestPipeline(t)
	rl := ratelimit.KeyConfig{ID: "vk-a", Capacity: 100, RefillPerSecond: 100}
	hashFirst, hashSecond := testHashOf("vk-a-rotated-first"), testHashOf("vk-a-rotated-second")
	if err := p.UpsertVirtualKey(identity.VirtualKey{ID: "vk-a", KeyHash: hashFirst, RateLimitBurst: 100, RateLimitRefill: 100}, rl); err != nil {
		t.Fatalf("first UpsertVirtualKey: %v", err)
	}
	if err := p.UpsertVirtualKey(identity.VirtualKey{ID: "vk-a", KeyHash: hashSecond, RateLimitBurst: 100, RateLimitRefill: 100}, rl); err != nil {
		t.Fatalf("second UpsertVirtualKey: %v", err)
	}
	if got := keyHashOf(t, p, "vk-a"); got != hashSecond {
		t.Fatalf("vk-a KeyHash after two sequential local upserts = %q, want the SECOND upsert's hash %q — the later local write was silently dropped as stale", got, hashSecond)
	}
}

// TestRotateThenDeleteVirtualKeyWhenTheClockReadsBackwards: a rotate
// followed by a delete of the same key must delete it (the delete is the
// second local call here; the rotate-as-second-call case is the next test).
func TestRotateThenDeleteVirtualKeyWhenTheClockReadsBackwards(t *testing.T) {
	p := virtualKeyVersionTestPipeline(t)
	if err := p.RotateVirtualKey("vk-a", testHashOf("vk-a-new"), time.Minute); err != nil {
		t.Fatalf("RotateVirtualKey: %v", err)
	}
	if err := p.DeleteVirtualKey("vk-a"); err != nil {
		t.Fatalf("DeleteVirtualKey after rotate: %v", err)
	}
	if _, ok := p.GetVirtualKey("vk-a"); ok {
		t.Fatal("vk-a still present after rotate-then-delete — the delete was dropped as a stale no-op")
	}
}

// TestRotateAppliesAfterAnUpsertEvenWhenTheClockReadsBackwards makes
// RotateVirtualKey the SECOND local call, so it is the rotate that would
// have hit the (now removed) stale guard: the key must carry the rotated
// hash, with the upserted hash kept as the grace-period previous hash.
func TestRotateAppliesAfterAnUpsertEvenWhenTheClockReadsBackwards(t *testing.T) {
	p, published := virtualKeyVersionTestReplica(t, "replica-a")
	hashUpserted, hashRotated := testHashOf("vk-a-upserted"), testHashOf("vk-a-rotated")
	rl := ratelimit.KeyConfig{ID: "vk-a", Capacity: 100, RefillPerSecond: 100}
	if err := p.UpsertVirtualKey(identity.VirtualKey{ID: "vk-a", KeyHash: hashUpserted, RateLimitBurst: 100, RateLimitRefill: 100}, rl); err != nil {
		t.Fatalf("UpsertVirtualKey: %v", err)
	}
	upsertEvent := published.last(t, configpropagation.TypeVirtualKeyUpsert)
	if err := p.RotateVirtualKey("vk-a", hashRotated, time.Minute); err != nil {
		t.Fatalf("RotateVirtualKey after upsert: %v", err)
	}
	vk, ok := p.GetVirtualKey("vk-a")
	if !ok {
		t.Fatal("vk-a missing after upsert-then-rotate")
	}
	if vk.KeyHash != hashRotated {
		t.Fatalf("KeyHash after upsert-then-rotate = %q, want the rotated hash %q — the rotate was dropped as a stale no-op", vk.KeyHash, hashRotated)
	}
	if vk.PreviousKeyHash != hashUpserted {
		t.Fatalf("PreviousKeyHash after rotate = %q, want the upserted hash %q", vk.PreviousKeyHash, hashUpserted)
	}
	// The rotation goes out as an upsert event under the SAME version it
	// was recorded under -- strictly past the upsert's -- so remote
	// replicas order it after the upsert exactly as this one did.
	rotateEvent := published.last(t, configpropagation.TypeVirtualKeyUpsert)
	p.virtualKeyMutationMu.Lock()
	recorded := p.virtualKeyVersions["vk-a"]
	p.virtualKeyMutationMu.Unlock()
	if rotateEvent.PublishedAtUnixNano != recorded.token || rotateEvent.OriginInstanceID != recorded.origin || recorded.origin != "replica-a" {
		t.Fatalf("rotate published (%d, %q), want the recorded version (%d, %q) from replica-a", rotateEvent.PublishedAtUnixNano, rotateEvent.OriginInstanceID, recorded.token, recorded.origin)
	}
	if rotateEvent.PublishedAtUnixNano <= upsertEvent.PublishedAtUnixNano {
		t.Fatalf("rotate published token %d, want > the upsert's %d", rotateEvent.PublishedAtUnixNano, upsertEvent.PublishedAtUnixNano)
	}
}

// TestLaterLocalWeightUpdateIsAppliedEvenWhenTheClockReadsBackwards covers
// the deployment-weight twin (UpdateDeploymentWeight / applyWeightIfNewer-
// Locked, guarded by weightVersions): a second local weight update must be
// applied -- observable as the recorded version for that (model,
// deployment) moving forward, since the version is recorded only for a
// mutation that was actually applied -- not dropped as stale while
// returning nil.
func TestLaterLocalWeightUpdateIsAppliedEvenWhenTheClockReadsBackwards(t *testing.T) {
	p := virtualKeyVersionTestPipeline(t)
	key := weightVersionKey{model: "gpt-4o", deploymentName: "d1"}
	if err := p.UpdateDeploymentWeight(context.Background(), "d1", 5); err != nil {
		t.Fatalf("first UpdateDeploymentWeight: %v", err)
	}
	p.weightVersionsMu.Lock()
	first := p.weightVersions[key].token
	p.weightVersionsMu.Unlock()
	if err := p.UpdateDeploymentWeight(context.Background(), "d1", 7); err != nil {
		t.Fatalf("second UpdateDeploymentWeight: %v", err)
	}
	p.weightVersionsMu.Lock()
	second := p.weightVersions[key].token
	p.weightVersionsMu.Unlock()
	if second <= first {
		t.Fatalf("weight version after the second local update = %d, want > %d — the later local update was dropped as stale (and its caller told it succeeded)", second, first)
	}
}

// TestLocalDeleteBumpsPastARemoteVersionAndPublishesANewerOne is the
// cross-replica half, and the security-relevant case: a replica whose clock
// runs ahead of ours upserted vk-a; revoking the key here must still take
// effect locally (not "204, key stays live") AND go out under a version the
// ahead replica will accept, i.e. strictly past the one it published.
func TestLocalDeleteBumpsPastARemoteVersionAndPublishesANewerOne(t *testing.T) {
	p, published := virtualKeyVersionTestReplica(t, "replica-a")
	remote := identity.VirtualKey{ID: "vk-a", KeyHash: testHashOf("vk-a-from-ahead-replica"), RateLimitBurst: 100, RateLimitRefill: 100}
	if err := p.ApplyVirtualKeyUpsertFromEvent(upsertPayload(remote), farFutureToken, "replica-ahead"); err != nil {
		t.Fatalf("ApplyVirtualKeyUpsertFromEvent: %v", err)
	}
	if err := p.DeleteVirtualKey("vk-a"); err != nil {
		t.Fatalf("DeleteVirtualKey after a far-future remote upsert: %v", err)
	}
	if _, ok := p.GetVirtualKey("vk-a"); ok {
		t.Fatal("vk-a still live after a local delete that reported success")
	}
	event := published.last(t, configpropagation.TypeVirtualKeyDelete)
	if event.PublishedAtUnixNano <= farFutureToken {
		t.Fatalf("published delete version = %d, want > the remote version %d so the ahead replica applies it", event.PublishedAtUnixNano, farFutureToken)
	}
	if event.OriginInstanceID != "replica-a" {
		t.Fatalf("published OriginInstanceID = %q, want this replica's own instance ID", event.OriginInstanceID)
	}
}

// TestLocalWeightUpdateBumpsPastARemoteVersionAndPublishesANewerOne is the
// deployment-weight twin of the previous test.
func TestLocalWeightUpdateBumpsPastARemoteVersionAndPublishesANewerOne(t *testing.T) {
	p, published := virtualKeyVersionTestReplica(t, "replica-a")
	if err := p.ApplyDeploymentWeightFromEvent("gpt-4o", "d1", 3, farFutureToken, "replica-ahead"); err != nil {
		t.Fatalf("ApplyDeploymentWeightFromEvent: %v", err)
	}
	if err := p.UpdateDeploymentWeight(context.Background(), "d1", 7); err != nil {
		t.Fatalf("UpdateDeploymentWeight after a far-future remote update: %v", err)
	}
	key := weightVersionKey{model: "gpt-4o", deploymentName: "d1"}
	p.weightVersionsMu.Lock()
	recorded := p.weightVersions[key]
	p.weightVersionsMu.Unlock()
	if recorded.token <= farFutureToken || recorded.origin != "replica-a" {
		t.Fatalf("recorded weight version = %+v, want token > %d from replica-a — the local update was dropped as stale", recorded, farFutureToken)
	}
	event := published.last(t, configpropagation.TypeDeploymentWeight)
	if event.PublishedAtUnixNano != recorded.token || event.OriginInstanceID != "replica-a" {
		t.Fatalf("published weight event version = (%d, %q), want the recorded (%d, %q)", event.PublishedAtUnixNano, event.OriginInstanceID, recorded.token, recorded.origin)
	}
}

// TestEqualTokenFromTwoReplicasConvergesByOriginTiebreak: two lagging
// replicas that both inherited version V from an ahead replica and both
// write vk-a locally publish the SAME token V+1 (proven below). Without a
// tie-break each would apply the other's event and they would swap values;
// with it every replica -- the two writers and bystanders receiving the two
// events in either order -- converges on the lexically greater origin.
func TestEqualTokenFromTwoReplicasConvergesByOriginTiebreak(t *testing.T) {
	a, publishedA := virtualKeyVersionTestReplica(t, "replica-a")
	b, publishedB := virtualKeyVersionTestReplica(t, "replica-b")
	bystanderAB, _ := virtualKeyVersionTestReplica(t, "replica-c")
	bystanderBA, _ := virtualKeyVersionTestReplica(t, "replica-d")
	inherited := identity.VirtualKey{ID: "vk-a", KeyHash: testHashOf("vk-a-inherited"), RateLimitBurst: 100, RateLimitRefill: 100}
	for _, p := range []*Pipeline{a, b, bystanderAB, bystanderBA} {
		if err := p.ApplyVirtualKeyUpsertFromEvent(upsertPayload(inherited), farFutureToken, "replica-ahead"); err != nil {
			t.Fatalf("ApplyVirtualKeyUpsertFromEvent(inherited): %v", err)
		}
	}

	rl := ratelimit.KeyConfig{ID: "vk-a", Capacity: 100, RefillPerSecond: 100}
	hashA, hashB := testHashOf("vk-a-written-on-a"), testHashOf("vk-a-written-on-b")
	if err := a.UpsertVirtualKey(identity.VirtualKey{ID: "vk-a", KeyHash: hashA, RateLimitBurst: 100, RateLimitRefill: 100}, rl); err != nil {
		t.Fatalf("UpsertVirtualKey on a: %v", err)
	}
	if err := b.UpsertVirtualKey(identity.VirtualKey{ID: "vk-a", KeyHash: hashB, RateLimitBurst: 100, RateLimitRefill: 100}, rl); err != nil {
		t.Fatalf("UpsertVirtualKey on b: %v", err)
	}
	eventA := publishedA.last(t, configpropagation.TypeVirtualKeyUpsert)
	eventB := publishedB.last(t, configpropagation.TypeVirtualKeyUpsert)
	if eventA.PublishedAtUnixNano != farFutureToken+1 || eventB.PublishedAtUnixNano != farFutureToken+1 {
		t.Fatalf("published tokens = (%d, %d), want both exactly %d — the tie this test exists for did not occur", eventA.PublishedAtUnixNano, eventB.PublishedAtUnixNano, farFutureToken+1)
	}

	// Cross-deliver, as the subscriber would.
	if err := b.ApplyVirtualKeyUpsertFromEvent(decodeUpsertPayload(t, eventA), eventA.PublishedAtUnixNano, eventA.OriginInstanceID); err != nil {
		t.Fatalf("b applying a's event: %v", err)
	}
	if err := a.ApplyVirtualKeyUpsertFromEvent(decodeUpsertPayload(t, eventB), eventB.PublishedAtUnixNano, eventB.OriginInstanceID); err != nil {
		t.Fatalf("a applying b's event: %v", err)
	}
	// Bystanders see the two events in opposite orders.
	for _, step := range []struct {
		p     *Pipeline
		first configpropagation.MutationEvent
		then  configpropagation.MutationEvent
	}{{bystanderAB, eventA, eventB}, {bystanderBA, eventB, eventA}} {
		for _, e := range []configpropagation.MutationEvent{step.first, step.then} {
			if err := step.p.ApplyVirtualKeyUpsertFromEvent(decodeUpsertPayload(t, e), e.PublishedAtUnixNano, e.OriginInstanceID); err != nil {
				t.Fatalf("bystander %s applying %s's event: %v", step.p.instanceID, e.OriginInstanceID, err)
			}
		}
	}

	for _, p := range []*Pipeline{a, b, bystanderAB, bystanderBA} {
		if got := keyHashOf(t, p, "vk-a"); got != hashB {
			t.Errorf("%s converged on %q, want replica-b's write %q (lexically greater origin wins the equal-token tie)", p.instanceID, got, hashB)
		}
	}
}

// TestEqualTokenTieBreakRules pins the two edges of mutationVersion.supersedes
// that the convergence test relies on: an equal token from the SAME origin
// re-applies (the idempotent re-apply configpropagation's canary promotion
// republishes), and an equal token from a lexically SMALLER origin is
// discarded.
func TestEqualTokenTieBreakRules(t *testing.T) {
	p := virtualKeyVersionTestPipeline(t)
	hash1, hash2, hash3 := testHashOf("vk-a-1"), testHashOf("vk-a-2"), testHashOf("vk-a-3")
	apply := func(hash, origin string) {
		t.Helper()
		vk := identity.VirtualKey{ID: "vk-a", KeyHash: hash, RateLimitBurst: 100, RateLimitRefill: 100}
		if err := p.ApplyVirtualKeyUpsertFromEvent(upsertPayload(vk), farFutureToken, origin); err != nil {
			t.Fatalf("ApplyVirtualKeyUpsertFromEvent(%s): %v", origin, err)
		}
	}
	apply(hash1, "replica-m")
	apply(hash2, "replica-m") // same token, same origin: re-apply
	if got := keyHashOf(t, p, "vk-a"); got != hash2 {
		t.Fatalf("after a same-origin equal-token re-apply KeyHash = %q, want %q", got, hash2)
	}
	apply(hash3, "replica-a") // same token, smaller origin: discarded
	if got := keyHashOf(t, p, "vk-a"); got != hash2 {
		t.Fatalf("after an equal-token event from a lexically smaller origin KeyHash = %q, want the retained %q", got, hash2)
	}
	apply(hash3, "replica-z") // same token, greater origin: wins
	if got := keyHashOf(t, p, "vk-a"); got != hash3 {
		t.Fatalf("after an equal-token event from a lexically greater origin KeyHash = %q, want %q", got, hash3)
	}
}

func TestSupersedesIsATotalOrderOverTokenThenOrigin(t *testing.T) {
	cases := []struct {
		name               string
		incoming, recorded mutationVersion
		want               bool
	}{
		{"greater token wins regardless of origin", mutationVersion{2, "a"}, mutationVersion{1, "z"}, true},
		{"smaller token loses regardless of origin", mutationVersion{1, "z"}, mutationVersion{2, "a"}, false},
		{"equal token, greater origin wins", mutationVersion{1, "b"}, mutationVersion{1, "a"}, true},
		{"equal token, smaller origin loses", mutationVersion{1, "a"}, mutationVersion{1, "b"}, false},
		{"equal token, same origin re-applies", mutationVersion{1, "a"}, mutationVersion{1, "a"}, true},
		{"anything supersedes the never-applied zero value", mutationVersion{1, ""}, mutationVersion{}, true},
	}
	for _, c := range cases {
		if got := c.incoming.supersedes(c.recorded); got != c.want {
			t.Errorf("%s: %+v supersedes %+v = %v, want %v", c.name, c.incoming, c.recorded, got, c.want)
		}
	}
}
