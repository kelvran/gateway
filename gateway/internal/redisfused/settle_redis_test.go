package redisfused

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/kelvran/gateway/gateway/internal/budget/redisbudget"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/admission"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/redislimiter"
	"github.com/kelvran/gateway/gateway/internal/spendledger"
)

const daySuffix = "\x00dim=day"

type tokenSpec struct {
	suffix string
	delta  float64
	real   *float64
}

// scenario is one settlement, run once fused (under id A) and once through the
// three standalone calls (under id B) from the same seeded state.
type scenario struct {
	name   string
	seed   func(t *testing.T, ctx context.Context, c *redis.Client, r *rig, id string, nowMS int64)
	budget *BudgetPart // KeyID is filled per run
	// wantSpent, when nonzero, is the exact spend the fused reply must report;
	// wantLedgerHas, when set, a substring of the ledger hash after the call.
	wantSpent     int64
	wantLedgerHas string
	tokens        []tokenSpec
	ledger        *spendledger.Entry // Key is filled per run
	// floorTAT: a seeded TAT in the past floors to "now", which differs by a few
	// ms between the two runs, so TATs compare within a tolerance.
	floorTAT bool
}

func seedBudget(t *testing.T, ctx context.Context, c *redis.Client, id string, spent, epoch, billed int64) {
	t.Helper()
	k, _ := redisbudget.PlanAdjust(id, 0, 0, false)
	if err := c.HSet(ctx, k[0], "spent", spent, "epoch", epoch, "billed_count", billed).Err(); err != nil {
		t.Fatalf("seeding budget: %v", err)
	}
	c.PExpire(ctx, k[0], time.Hour)
}

func seedTokens(t *testing.T, ctx context.Context, c *redis.Client, key string, tat int64, histSum float64, histCount int64) {
	t.Helper()
	k, _ := redislimiter.PlanSettleTokens([]admission.Settlement{{Key: key}})
	if err := c.Set(ctx, k[0], tat, time.Hour).Err(); err != nil {
		t.Fatalf("seeding TAT: %v", err)
	}
	if err := c.HSet(ctx, k[1], "sum", histSum, "count", histCount).Err(); err != nil {
		t.Fatalf("seeding history: %v", err)
	}
	c.PExpire(ctx, k[1], time.Hour)
}

func (s scenario) request(id string, led *spendledger.Entry) Request {
	var req Request
	if s.budget != nil {
		b := *s.budget
		b.KeyID = id
		req.Budget = &b
	}
	for _, tk := range s.tokens {
		req.Tokens = append(req.Tokens, settle(id+tk.suffix, tk.delta, tk.real))
	}
	if led != nil {
		req.Ledger = led
	}
	return req
}

func (s scenario) tokenKeys(id string) []string {
	var keys []string
	for _, tk := range s.tokens {
		keys = append(keys, id+tk.suffix)
	}
	return keys
}

func (s scenario) entry(id string) *spendledger.Entry {
	if s.ledger == nil {
		return nil
	}
	e := *s.ledger
	e.Key = id
	return &e
}

// runStandalone is today's path: the three scripts, one call each.
func (r *rig) runStandalone(t *testing.T, ctx context.Context, req Request) (spent int64) {
	t.Helper()
	if req.Budget != nil {
		b := req.Budget
		var err error
		if spent, err = r.budget.Adjust(ctx, b.KeyID, b.DeltaNanoUSD, b.Epoch, b.Billed); err != nil {
			t.Fatalf("standalone Adjust: %v", err)
		}
	}
	if err := r.limiter.SettleTokens(ctx, req.Tokens); err != nil {
		t.Fatalf("standalone SettleTokens: %v", err)
	}
	if req.Ledger != nil {
		if err := r.ledger.Record(ctx, *req.Ledger); err != nil {
			t.Fatalf("standalone Record: %v", err)
		}
	}
	return spent
}

func ttlClose(a, b int64) bool {
	if a <= 0 || b <= 0 {
		return a == b
	}
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= 5000
}

func sameState(t *testing.T, sa, sb map[string]string, ta, tb map[string]int64, floorTAT bool) {
	t.Helper()
	for role, va := range sa {
		vb := sb[role]
		if va == vb {
		} else if floorTAT && strings.HasPrefix(role, "tat") && va != "-" && vb != "-" {
			x, _ := strconv.ParseInt(strings.TrimPrefix(va, "s:"), 10, 64)
			y, _ := strconv.ParseInt(strings.TrimPrefix(vb, "s:"), 10, 64)
			if math.Abs(float64(x-y)) > 5000 {
				t.Errorf("%s differs beyond the floor tolerance: fused %s, standalone %s", role, va, vb)
			}
		} else {
			t.Errorf("%s differs: fused %q, standalone %q", role, va, vb)
		}
		if !ttlClose(ta[role], tb[role]) {
			t.Errorf("%s TTL differs: fused %d ms, standalone %d ms", role, ta[role], tb[role])
		}
	}
}

func (s scenario) run(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	idA, idB := uniqueID(t)+"-A", uniqueID(t)+"-B"
	now := time.Now().UnixMilli()
	if s.seed != nil {
		s.seed(t, ctx, r.client, r, idA, now)
		s.seed(t, ctx, r.client, r, idB, now)
	}
	reqA := s.request(idA, s.entry(idA))
	reqB := s.request(idB, s.entry(idB))
	res := r.settler.Settle(ctx, reqA)
	spentB := r.runStandalone(t, ctx, reqB)
	if hasParts := reqA.Budget != nil || len(reqA.Tokens) > 0 || reqA.Ledger != nil; res.Sent != hasParts {
		t.Errorf("Sent = %v for a request with parts = %v", res.Sent, hasParts)
	}

	for name, p := range map[string]Part{"budget": res.Budget, "tokens": res.Tokens, "ledger": res.Ledger} {
		if p.Err != nil {
			t.Fatalf("%s part failed: %v", name, p.Err)
		}
	}
	if s.budget != nil && (res.Budget.Status != StatusApplied || res.SpentAfterNanoUSD != spentB) {
		t.Errorf("budget status %v spent_after %d, standalone Adjust returned %d", res.Budget.Status, res.SpentAfterNanoUSD, spentB)
	}
	if s.budget == nil && res.Budget.Status != StatusSkipped {
		t.Errorf("absent budget part has status %v", res.Budget.Status)
	}
	if (len(s.tokens) > 0) != (res.Tokens.Status == StatusApplied) {
		t.Errorf("token status %v for %d settlements", res.Tokens.Status, len(s.tokens))
	}
	if (s.ledger != nil) != (res.Ledger.Status == StatusApplied) {
		t.Errorf("ledger status %v for ledger=%v", res.Ledger.Status, s.ledger != nil)
	}
	entry := entryAt("x", "s1", 0)
	if s.ledger != nil {
		entry = *s.ledger
	}
	if s.wantSpent != 0 && res.SpentAfterNanoUSD != s.wantSpent {
		t.Errorf("spend reported = %d, want exactly %d", res.SpentAfterNanoUSD, s.wantSpent)
	}
	sa, ta := stateOf(t, ctx, r.client, idA, s.tokenKeys(idA), entry, r.ledger)
	if s.wantLedgerHas != "" && !strings.Contains(sa["ledger.hash"], s.wantLedgerHas) {
		t.Errorf("ledger hash = %q, want it to hold %q", sa["ledger.hash"], s.wantLedgerHas)
	}
	sb, tb := stateOf(t, ctx, r.client, idB, s.tokenKeys(idB), entry, r.ledger)
	sameState(t, sa, sb, ta, tb, s.floorTAT)
}

func ledgerEntry(session string, spend int64) *spendledger.Entry {
	e := entryAt("", session, spend)
	return &e
}

func TestFusedSettleMatchesTheStandaloneScripts(t *testing.T) {
	const epoch = int64(1_700_000_123_456_789)
	budgetSeed := func(t *testing.T, ctx context.Context, c *redis.Client, _ *rig, id string, _ int64) {
		seedBudget(t, ctx, c, id, 1_000_000_000, epoch, 2)
	}
	tokenSeed := func(t *testing.T, ctx context.Context, c *redis.Client, _ *rig, id string, now int64) {
		seedTokens(t, ctx, c, id, now+3_600_000, 400, 3)
		seedTokens(t, ctx, c, id+daySuffix, now+7_200_000, 900, 5)
	}
	scenarios := []scenario{
		{name: "budget billed", seed: budgetSeed, budget: &BudgetPart{DeltaNanoUSD: 500, Epoch: epoch, Billed: true}},
		{name: "budget release", seed: budgetSeed, budget: &BudgetPart{DeltaNanoUSD: -300, Epoch: epoch}},
		{name: "budget release below zero floors at zero", seed: budgetSeed, budget: &BudgetPart{DeltaNanoUSD: -9_000_000_000, Epoch: epoch}},
		{name: "stale epoch is a no-op that reports the current spend", seed: budgetSeed, budget: &BudgetPart{DeltaNanoUSD: 500, Epoch: epoch + 1, Billed: true}},
		{name: "gone window creates nothing and reports zero", budget: &BudgetPart{DeltaNanoUSD: 500, Epoch: epoch, Billed: true}},
		{name: "tokens minute only, real count", seed: tokenSeed, tokens: []tokenSpec{{"", 60_000, f64(120)}}},
		{name: "tokens day only fills the first slot, release", seed: tokenSeed, tokens: []tokenSpec{{daySuffix, -60_000, nil}}},
		{name: "tokens both dimensions", seed: tokenSeed, tokens: []tokenSpec{{"", 60_000, f64(120)}, {daySuffix, 30_000, f64(80)}}},
		{name: "tokens with no TAT still learn the history", tokens: []tokenSpec{{"", 60_000, f64(50)}}},
		{name: "tokens past TAT floors at now", floorTAT: true, tokens: []tokenSpec{{"", 1, nil}},
			seed: func(t *testing.T, ctx context.Context, c *redis.Client, _ *rig, id string, now int64) {
				seedTokens(t, ctx, c, id, now-10_000, 10, 1)
			}},
		{name: "ledger first write", ledger: ledgerEntry("s1", 123_456_789)},
		{name: "ledger session beyond the cap folds", ledger: ledgerEntry("s3", 5),
			seed: func(t *testing.T, ctx context.Context, c *redis.Client, r *rig, id string, _ int64) {
				for _, sess := range []string{"s1", "s2"} {
					e := entryAt(id, sess, 1)
					if err := r.ledger.Record(ctx, e); err != nil {
						t.Fatalf("seeding ledger: %v", err)
					}
				}
			}},
		{name: "ledger existing cell accumulates", ledger: ledgerEntry("s1", 7),
			seed: func(t *testing.T, ctx context.Context, c *redis.Client, r *rig, id string, _ int64) {
				if err := r.ledger.Record(ctx, entryAt(id, "s1", 100)); err != nil {
					t.Fatalf("seeding ledger: %v", err)
				}
			}},
		{name: "all three parts", budget: &BudgetPart{DeltaNanoUSD: 250, Epoch: epoch, Billed: true},
			tokens: []tokenSpec{{"", 60_000, f64(120)}, {daySuffix, 30_000, f64(80)}}, ledger: ledgerEntry("s1", 250),
			seed: func(t *testing.T, ctx context.Context, c *redis.Client, r *rig, id string, now int64) {
				budgetSeed(t, ctx, c, r, id, now)
				tokenSeed(t, ctx, c, r, id, now)
			}},
		{name: "a 15-digit budget spend past 1e14 and a ledger amount of 2^53+1 stay exact",
			budget:        &BudgetPart{DeltaNanoUSD: 9_000_000_000_002, Epoch: epoch, Billed: true},
			ledger:        ledgerEntry("s1", 9_007_199_254_740_993),
			wantSpent:     159_000_000_000_003, // %.14g would turn this into 1.59e+14
			wantLedgerHas: "=9007199254740993",
			seed: func(t *testing.T, ctx context.Context, c *redis.Client, _ *rig, id string, _ int64) {
				seedBudget(t, ctx, c, id, 150_000_000_000_001, epoch, 40)
			}},
	}
	for _, s := range scenarios {
		t.Run(s.name, s.run)
	}
}

// A seeded random walk over the same differential: any layout or encoding slip
// the named scenarios miss shows up here.
func TestFusedSettleSeededRandomDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(20261030)) //nolint:gosec // G404: a seeded generator is the point, the walk must be reproducible
	const epoch = int64(1_700_000_555_000_001)
	for i := 0; i < 60; i++ {
		s := scenario{name: fmt.Sprintf("case-%02d", i)}
		hasBudget, hasLedger := rng.Intn(3) > 0, rng.Intn(3) > 0
		nDims := rng.Intn(3)
		spent := rng.Int63n(5_000_000_000_000)
		if hasBudget {
			s.budget = &BudgetPart{DeltaNanoUSD: rng.Int63n(2_000_000_000) - 1_000_000_000, Epoch: epoch + int64(rng.Intn(2)), Billed: rng.Intn(2) == 0}
		}
		for d := 0; d < nDims; d++ {
			var real *float64
			if rng.Intn(2) == 0 {
				real = f64(float64(rng.Intn(5000)) + 0.25)
			}
			suffix := ""
			if d == 1 {
				suffix = daySuffix
			}
			s.tokens = append(s.tokens, tokenSpec{suffix, float64(rng.Intn(200_000)-100_000) + 0.5, real})
		}
		if hasLedger {
			s.ledger = ledgerEntry([]string{"s1", "s2", "s3", ""}[rng.Intn(4)], rng.Int63n(1_000_000_000_000))
		}
		seedTAT := rng.Intn(2) == 0
		s.floorTAT = true
		s.seed = func(t *testing.T, ctx context.Context, c *redis.Client, r *rig, id string, now int64) {
			seedBudget(t, ctx, c, id, spent, epoch, 3)
			if seedTAT {
				seedTokens(t, ctx, c, id, now+1_800_000, 77.5, 4)
				seedTokens(t, ctx, c, id+daySuffix, now+3_600_000, 88.5, 6)
			}
		}
		t.Run(s.name, s.run)
	}
}
