package ratelimit

import (
	"context"
	"testing"
)

// The Redis-mode settlement hands the backend the real count so it can learn
// the key's running mean; a release passes nil and the top-up path
// (AdjustTPM) is never used for a settlement.
func TestReconcileTPMRedisModeSettlesWithTheRealCountAndWithNil(t *testing.T) {
	fb := &fakeBackend{reserveTPMReserved: 1000}
	l := NewRedisKeyLimiter([]KeyConfig{{ID: "k", TPMCapacity: 1000, TPMRefillPerSecond: 100}}, fb)
	real := 100.0
	l.ReconcileTPM(context.Background(), "k", "", 1000, 0, &real)
	if fb.lastSettleReal == nil || *fb.lastSettleReal != 100 || fb.lastSettleDelta != (1000.0/100)*(100-1000) {
		t.Fatalf("settle = (delta %v, real %v), want delta %v and real 100", fb.lastSettleDelta, fb.lastSettleReal, (1000.0/100)*(100-1000))
	}
	l.ReconcileTPM(context.Background(), "k", "", 1000, 0, nil)
	if fb.lastSettleReal != nil || fb.lastSettleDelta != (1000.0/100)*(0-1000) {
		t.Fatalf("release = (delta %v, real %v), want the full release and no real count", fb.lastSettleDelta, fb.lastSettleReal)
	}
	if fb.adjustCalls != 0 {
		t.Errorf("a settlement must not go through AdjustTPM (the top-up path): %d calls", fb.adjustCalls)
	}
}
