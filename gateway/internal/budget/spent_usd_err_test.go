package budget

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// SpentUSDErr is the error-returning twin of SpentUSD (RFC-3 decision 5):
// the admin list route uses it to report `spend_unavailable` instead of a
// false zero. SpentUSD keeps its fail-open log-and-zero contract on top.

func TestSpentUSDErrReturnsTheBackendErrorAndSpentUSDStillFailsOpen(t *testing.T) {
	boom := errors.New("redis: connection refused")
	backend := &fakeRedisBudgetBackend{
		spentNanoUSDFunc: func(ctx context.Context, keyID string) (int64, error) { return 0, boom },
	}
	var logBuf bytes.Buffer
	tr := NewRedisTracker(backend, slog.New(slog.NewTextHandler(&logBuf, nil)))

	spent, err := tr.SpentUSDErr(context.Background(), "k", 0)
	if !errors.Is(err, boom) || !spent.IsZero() {
		t.Fatalf("SpentUSDErr on a backend error = (%s, %v), want (0, the backend error)", spent, err)
	}
	if strings.Contains(logBuf.String(), "budget_redis_backend_unavailable") {
		t.Errorf("SpentUSDErr must not log; the caller decides how to surface the error: %s", logBuf.String())
	}
	if got := tr.SpentUSD(context.Background(), "k", 0); !got.IsZero() {
		t.Errorf("SpentUSD on a backend error = %s, want 0 (fail-open)", got)
	}
	if !strings.Contains(logBuf.String(), "budget_redis_backend_unavailable") || !strings.Contains(logBuf.String(), "op=spent_usd") {
		t.Errorf("SpentUSD must keep logging the fail-open: %s", logBuf.String())
	}
}

func TestSpentUSDErrRedisModeConvertsUnits(t *testing.T) {
	backend := &fakeRedisBudgetBackend{
		spentNanoUSDFunc: func(ctx context.Context, keyID string) (int64, error) { return 3_500_000_000, nil },
	}
	tr := NewRedisTracker(backend, nil)
	spent, err := tr.SpentUSDErr(context.Background(), "k", 0)
	if err != nil || !spent.Equal(d("3.5")) {
		t.Errorf("SpentUSDErr = (%s, %v), want (3.5, nil)", spent, err)
	}
}

func TestSpentUSDErrInMemoryNeverErrors(t *testing.T) {
	tr := NewTracker()
	spent, err := tr.SpentUSDErr(context.Background(), "never-seen", 0)
	if err != nil || !spent.IsZero() {
		t.Errorf("in-memory SpentUSDErr = (%s, %v), want (0, nil)", spent, err)
	}
}
