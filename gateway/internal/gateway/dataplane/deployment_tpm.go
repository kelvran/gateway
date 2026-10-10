package dataplane

// Deployment-scoped tokens-per-minute: the per-hop reserve-then-reconcile
// gate controlplane.DeploymentConfig.TPMCapacity's doc deferred on
// 2026-09-09, wired 2026-10-07 per docs/upgrade-research/kelvran-deep-
// research-round3-2026-10-07.md (ranked item 1), plus the provider-quota
// weighting (rate_limit.tpm_accounting) that makes the bucket count tokens
// the way Bedrock's own quota does.
//
// Why the gate lives at CALL time rather than in checkDeploymentCapacity:
// that skip-gate returns a bool and is released by deployment NAME
// (attemptFallbackChain's releaseDeploymentCapacity parameter) — fine for
// a concurrency slot, wrong for a TPM reservation, where two concurrent
// hops to the same deployment would be indistinguishable. And
// KeyLimiter.ReserveTPM IS the reservation (there is no non-consuming
// "would it pass" peek), so it is taken immediately before the upstream
// call, wrapping callDeployment/streamDeployment themselves:
// callDeploymentWithTPM / streamDeploymentWithTPM are what every path that
// used to call those two directly now calls — hop 1 via
// *WithCapacityCheck, a fallback_chains hop via attemptFallbackChain's
// call closure, the router-based single fallback via *WithCapacityCheck
// again. Health probes call callDeployment directly and are deliberately
// NOT gated. A TPM rejection inside a fallback chain is a
// DeploymentCapacityError (FallbackClassGeneric), so the chain moves on to
// the next hop, exactly as for any other failed hop.
//
// Known approximation, shared with the per-key dimension by design (see
// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md): the
// reservation is the bucket's own running mean of past real counts (cold
// start: the full balance), not an input+max_tokens estimate of THIS
// request. The reconcile step corrects it to the real weighted count once
// the response is known.

import (
	"context"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// deploymentTPMTokens is dep's quota-weighted token count for one
// completed call — the number its TPM bucket is debited by. Formula per
// controlplane.DeploymentConfig.TPMOutputTokenMultiplier's doc comment:
// inclusive prompt tokens, minus cache reads when excluded, plus
// completion tokens times the multiplier (0 = unset = 1; anything below
// 1 is treated as 1 defensively — the config parser already rejects it).
// Worked example (Bedrock, x10, exclude reads): inclusive input 100 (20
// fresh + 60 cache-read + 20 cache-write), output 20 ->
// 100 - 60 + 20*10 = 240 = Bedrock's own 20 + 20 + 200.
func deploymentTPMTokens(dep Deployment, u adapter.Usage) float64 {
	multiplier := dep.TPMOutputTokenMultiplier
	if multiplier < 1 {
		multiplier = 1
	}
	prompt := float64(u.PromptTokens)
	if dep.TPMExcludeCacheReadTokens {
		// CacheReadTokens is a subset of PromptTokens by adapter.Usage's
		// contract; the clamp only guards a misbehaving upstream.
		prompt = max(prompt-float64(u.CacheReadTokens), 0)
	}
	return prompt + float64(u.CompletionTokens)*multiplier
}

// deploymentTPMReservation is one hop's reservation against dep's TPM
// bucket. The zero value means nothing was reserved (no bucket configured
// for dep, or the limiter failed open) and reconciling it is a no-op.
type deploymentTPMReservation struct {
	reserved bool
	tokens   float64
	epoch    int64
}

// reserveDeploymentTPM reserves against dep's TPM bucket immediately
// before a call. ok=false means the bucket is exhausted and the call must
// not be made. Fails OPEN on a limiter backend error, mirroring
// checkDeploymentRateLimit's policy and rationale (an infra problem with
// the limiter itself must not take the deployment down) — the in-memory
// limiter cmd/gateway always wires never errors, so that branch is
// defensive.
func (p *Pipeline) reserveDeploymentTPM(ctx context.Context, dep Deployment) (deploymentTPMReservation, bool) {
	if p.deploymentLimiter == nil {
		return deploymentTPMReservation{}, true
	}
	allowed, reserved, tokens, epoch, err := p.deploymentLimiter.ReserveTPM(ctx, dep.Name, "")
	if err != nil {
		p.logger.Warn("deployment_tpm_backend_unavailable", append(traceLogFields(ctx), "deployment", dep.Name, "error", err.Error())...)
		return deploymentTPMReservation{}, true
	}
	if !allowed {
		return deploymentTPMReservation{}, false
	}
	return deploymentTPMReservation{reserved: reserved, tokens: tokens, epoch: epoch}, true
}

// reconcileDeploymentTPM settles a reservation once the hop is over: with
// usage, the bucket is debited the weighted real count (and that count
// feeds the bucket's running mean for the next reservation); with nil
// (the call failed), the reservation is released and nothing is debited,
// so a failed hop never consumes the deployment's quota. A no-op when
// nothing was reserved.
func (p *Pipeline) reconcileDeploymentTPM(ctx context.Context, dep Deployment, r deploymentTPMReservation, usage *adapter.Usage) {
	if !r.reserved || p.deploymentLimiter == nil {
		return
	}
	var real *float64
	if usage != nil {
		v := deploymentTPMTokens(dep, *usage)
		real = &v
	}
	p.deploymentLimiter.ReconcileTPM(ctx, dep.Name, "", r.tokens, r.epoch, real)
}

// callDeploymentWithTPM is callDeployment behind dep's own TPM gate — the
// buffered-path entry every per-hop caller uses (see the file comment).
//
// The reconcile is DEFERRED, with usage left nil (= release) until the
// call has demonstrably succeeded, so the ratelimit.TokenBucket contract
// ("every ReserveTPM that returned allowed MUST reach a matching
// ReconcileTPM on every return path") holds through a panic in an adapter
// or upstream caller too — the same reason the concurrency slot is
// released by defer in callDeploymentWithCapacityCheck. Without it a
// panicking adapter would leak its reservation and, repeated, drain the
// bucket for requests that never consumed provider quota.
func (p *Pipeline) callDeploymentWithTPM(ctx context.Context, dep Deployment, req adapter.ChatRequest) (adapter.ChatResponse, error) {
	r, ok := p.reserveDeploymentTPM(ctx, dep)
	if !ok {
		return adapter.ChatResponse{}, &DeploymentCapacityError{Deployment: dep.Name, Reason: "tpm"}
	}
	var usage *adapter.Usage
	defer func() { p.reconcileDeploymentTPM(ctx, dep, r, usage) }()
	resp, err := p.callDeployment(ctx, dep, req)
	if err != nil {
		return resp, err
	}
	usage = &resp.Usage
	return resp, nil
}

// streamDeploymentWithTPM is streamDeployment behind the same gate, with
// the same deferred, panic-safe reconcile. A stream that fails — including
// one cut off after chunks already flowed — releases its reservation
// rather than debiting a partial count: the request as a whole failed,
// matching how the per-key TPM dimension treats a non-billable outcome,
// and erring towards admitting the next caller rather than charging the
// deployment for output that may never have been produced.
func (p *Pipeline) streamDeploymentWithTPM(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw streaming.ChunkSink, firstChunkSent *bool, keyID string, msr midStreamReservation, blocked *bool) (adapter.ChatResponse, bool, error) {
	r, ok := p.reserveDeploymentTPM(ctx, dep)
	if !ok {
		return adapter.ChatResponse{}, false, &DeploymentCapacityError{Deployment: dep.Name, Reason: "tpm"}
	}
	var usage *adapter.Usage
	defer func() { p.reconcileDeploymentTPM(ctx, dep, r, usage) }()
	resp, estimated, err := p.streamDeployment(ctx, dep, req, sw, firstChunkSent, keyID, msr, blocked)
	if err != nil {
		return resp, estimated, err
	}
	usage = &resp.Usage
	return resp, estimated, nil
}
