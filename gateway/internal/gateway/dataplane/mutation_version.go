package dataplane

// Versioning for admin mutations: how a LOCAL mutation derives the version
// it applies and publishes under, and how two versions compare, for the
// last-writer-wins maps virtualKeyVersions and weightVersions that remote,
// event-driven applies share with the local entry points.
//
// The stale guard in each apply path ("an incoming update that does not
// supersede what this replica has already applied is silently a no-op")
// exists for cross-replica convergence: a REMOTE mutation that arrives
// late must not overwrite a newer one, and from this replica's own
// perspective nothing is wrong, so nil is the right answer for the
// subscriber. It is the WRONG answer for a LOCAL admin call, which is by
// definition a new writer: until 2026-10-07 the local entry points read
// p.now() before taking the mutation lock, so two concurrent admin calls
// whose clock reads and lock acquisitions inverted made the loser look
// "stale" and return nil -- two 204s and two audit entries for one
// deletion (internal/admin's TestConcurrentDeleteVirtualKeyRequestsFor-
// SameNameNeverBothSucceed, a CI-only failure first disclosed on
// 2026-09-22 as "not root-caused", root-caused when it failed the
// gateway/v0.17.0 tag run), or a later local upsert / weight update
// silently discarded while its caller was told it succeeded.
//
// The fix is structural rather than a wider lock: a local mutation derives
// its token UNDER the mutation lock and strictly past anything this
// replica has already applied for that target (bumpPast), so a local write
// can never be stale against the replica's own view -- whatever the wall
// clock does, including stepping backwards. The remote-apply entry points
// still carry the originating replica's own version and still hit the
// guard, which is exactly the convergence rule they need.
//
// Bumping has one consequence the plain wall clock did not: two lagging
// replicas that both inherited the same version V from an ahead-clock
// replica, and both take a local write to the same target before seeing
// each other's event, BOTH publish token V+1. Under a bare "strictly older
// is stale" rule each would apply the other's event (equal is not older)
// and the two would end up holding each other's value -- permanent,
// undetected divergence. So a version is (token, origin), and
// mutationVersion.supersedes breaks an equal-token tie by origin
// deterministically: every replica, in every delivery order, converges on
// the same writer. An equal token from the SAME origin still supersedes
// (applies), which is the idempotent re-apply configpropagation's
// CanaryPercent promotion depends on (see MutationEvent's doc comment).
// Covered deterministically by virtualkey_mutation_version_test.go, which
// injects a clock that reads backwards and simulates several replicas by
// giving pipelines distinct instanceIDs.

// mutationVersion is the last-writer-wins version recorded per target (a
// virtual key ID, or a (model, deploymentName) pair) and carried on every
// published MutationEvent: token is MutationEvent.PublishedAtUnixNano and
// origin is MutationEvent.OriginInstanceID. The zero value means "never
// applied".
type mutationVersion struct {
	token  int64
	origin string
}

// supersedes reports whether incoming must REPLACE recorded: a strictly
// greater token always wins; on an equal token the lexically greater
// origin wins, and the SAME origin re-applies (idempotent). The
// comparison is a total order over (token, origin), which is what makes
// every replica pick the same winner regardless of delivery order.
func (incoming mutationVersion) supersedes(recorded mutationVersion) bool {
	if incoming.token != recorded.token {
		return incoming.token > recorded.token
	}
	return incoming.origin >= recorded.origin
}

// localVirtualKeyVersionLocked returns the version a LOCAL virtual-key
// mutation of id applies and publishes under. The caller holds
// virtualKeyMutationMu.
func (p *Pipeline) localVirtualKeyVersionLocked(id string) mutationVersion {
	return mutationVersion{
		token:  bumpPast(p.now().UnixNano(), p.virtualKeyVersions[id].token),
		origin: p.instanceID,
	}
}

// localWeightVersionLocked returns the version a LOCAL deployment-weight
// update of key applies and publishes under. The caller holds
// weightVersionsMu.
func (p *Pipeline) localWeightVersionLocked(key weightVersionKey) mutationVersion {
	return mutationVersion{
		token:  bumpPast(p.now().UnixNano(), p.weightVersions[key].token),
		origin: p.instanceID,
	}
}

// bumpPast returns now, or prev+1 when now would not be strictly newer
// than the token already recorded -- a strictly greater token is what
// makes supersedes true for the local caller regardless of origin.
func bumpPast(now, prev int64) int64 {
	if now <= prev {
		return prev + 1
	}
	return now
}
