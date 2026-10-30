package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/budget/redisbudget"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/admission"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/redislimiter"
	"github.com/kelvran/gateway/gateway/internal/spendledger"
	"github.com/kelvran/gateway/gateway/internal/spendledger/redisledger"
)

// Round-5 item 10 phase 2c-2 end to end: over the shared client a billable
// request's three post-call writes are ONE Redis command.

// fakeLedgerWithoutPlanner is a ledger that is not the Redis store.
type fakeLedgerWithoutPlanner struct{ spendledger.Ledger }

func TestNewFusedSettlerNeedsTheSharedClientAndARedisLedger(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	shared := &sharedRedis{client: client}
	defer func() { _ = client.Close() }()
	store := redisledger.NewOn(client, spendledger.Options{}, shared.acquire())

	if got := newFusedSettler(nil, store); got != nil {
		t.Errorf("no shared client built a settler: %v", got)
	}
	if got := newFusedSettler(shared, nil); got != nil {
		t.Errorf("a nil ledger built a settler: %v", got)
	}
	if got := newFusedSettler(shared, fakeLedgerWithoutPlanner{spendledger.NewMemory(spendledger.Options{})}); got != nil {
		t.Errorf("a ledger that is not the Redis store built a settler: %v", got)
	}
	// The nil results above must be genuine nil interfaces: a typed nil would
	// pass Config.Fused != nil and panic in finalize.
	holdsBefore := shared.holds
	got := newFusedSettler(shared, store)
	if got == nil {
		t.Fatal("shared client and a Redis ledger built no settler")
	}
	if shared.holds != holdsBefore+1 {
		t.Errorf("holds = %d, want %d: the settler takes a hold of its own", shared.holds, holdsBefore+1)
	}
	if err := got.Close(); err != nil || shared.holds != holdsBefore {
		t.Errorf("Close = %v with %d holds, want nil and %d", err, shared.holds, holdsBefore)
	}
}

func namedKeys(name, secret string) []controlplane.VirtualKeyConfig {
	return []controlplane.VirtualKeyConfig{{
		Name: name, KeyHash: testKeyHash(secret),
		RateLimitBurst: 1000, RateLimitRefill: 1000, TPMCapacity: 10_000_000, TPMRefillPerSecond: 10_000_000,
		TokensPerDay: 100_000_000, BudgetUSD: decimal.NewFromInt(1_000_000),
	}}
}

// redisStateOfKey reads what a key's requests left in Redis: the budget's spend
// and billed count, both token histories, and the ledger's totals.
func redisStateOfKey(t *testing.T, c *redis.Client, name string) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	bk, _ := redisbudget.PlanAdjust(name, 0, 0, false)
	for _, f := range []string{"spent", "billed_count"} {
		out["budget."+f] = c.HGet(ctx, bk[0], f).Val()
	}
	for dim, key := range map[string]string{"tpm": name, "tpd": name + "\x00dim=day"} {
		tk, _ := redislimiter.PlanSettleTokens([]admission.Settlement{{Key: key}})
		for _, f := range []string{"sum", "count"} {
			out[dim+"."+f] = c.HGet(ctx, tk[1], f).Val()
		}
	}
	var spend, requests int64
	iter := c.Scan(ctx, 0, "ledger:*:"+url.QueryEscape(name), 100).Iterator()
	for iter.Next(ctx) {
		for f, v := range c.HGetAll(ctx, iter.Val()).Val() {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatalf("ledger field %s=%q: %v", f, v, err)
			}
			if strings.HasSuffix(f, "#n") {
				requests += n
			} else {
				spend += n
			}
		}
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("SCAN: %v", err)
	}
	out["ledger.spend"], out["ledger.requests"] = strconv.FormatInt(spend, 10), strconv.FormatInt(requests, 10)
	return out
}

// The strongest end-to-end check: the same six requests through a gateway whose
// stores share one client (the fused path) and through one whose stores run on
// separate clients (today's three scripts) leave the same state in Redis.
func TestIntegrationFusedSettlementLeavesTheSameStateAsTheSeparateClients(t *testing.T) {
	redisAddr := openTestRedisForMain(t)
	host, port, err := splitHostPortForTest(redisAddr)
	if err != nil {
		t.Fatal(err)
	}
	var other string
	switch host {
	case "localhost":
		other = "127.0.0.1"
	case "127.0.0.1":
		other = "localhost"
	default:
		t.Skipf("the test Redis is at %q, which has no loopback alias to spell it two ways", host)
	}
	upstream, _ := newMockUpstream(t)
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	sharedName, sepName := "parity-shared-"+nonce, "parity-separate-"+nonce
	gwShared, pShared := newSharedRedisIntegrationServer(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_PARITY_A", redisAddr, redisAddr, namedKeys(sharedName, "parity-shared-secret"))
	t.Cleanup(func() { _ = pShared.Close() })
	gwSep, pSep := newSharedRedisIntegrationServer(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_PARITY_B", host+":"+port, other+":"+port, namedKeys(sepName, "parity-separate-secret"))
	t.Cleanup(func() { _ = pSep.Close() })

	for i := 0; i < 6; i++ {
		if code, body := doDistinctChatRequest(t, gwShared, "parity-shared-secret", i); code != 200 {
			t.Fatalf("shared request %d = %d %s", i, code, body)
		}
		if code, body := doDistinctChatRequest(t, gwSep, "parity-separate-secret", i); code != 200 {
			t.Fatalf("separate request %d = %d %s", i, code, body)
		}
	}
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	a, b := redisStateOfKey(t, c, sharedName), redisStateOfKey(t, c, sepName)
	if a["budget.billed_count"] != "6" || a["tpm.count"] != "6" || a["tpd.count"] != "6" || a["ledger.requests"] != "6" {
		t.Fatalf("the fused path left %v, want six billable requests recorded everywhere", a)
	}
	for k, va := range a {
		if b[k] != va {
			t.Errorf("%s: fused %q, three scripts %q", k, va, b[k])
		}
	}
}

// A ledger row that cannot be written (a foreign value squats on the hour's
// hash) loses only the ledger part: the request is served, and the budget and
// token settlements of the same call still land.
func TestIntegrationLedgerWrongTypeKeepsBudgetAndTokenSettlement(t *testing.T) {
	redisAddr := openTestRedisForMain(t)
	upstream, _ := newMockUpstream(t)
	var logs bytes.Buffer
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	name := "ledger-wrongtype-" + nonce
	gw, pipeline := newSharedRedisIntegrationServerWithLogger(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_WRONGTYPE", redisAddr, redisAddr,
		namedKeys(name, "wrongtype-secret"), slog.New(slog.NewJSONHandler(&logs, nil)))

	t.Cleanup(func() { _ = pipeline.Close() })
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	planner := redisledger.NewOn(c, spendledger.Options{}, func() error { return nil })
	for _, at := range []time.Time{time.Now(), time.Now().Add(time.Hour)} { // the hour may roll over mid-test
		keys, _, err := planner.PlanRecord(spendledger.Entry{Key: name, Model: "gpt-4o", At: at, Requests: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Set(ctx, keys[0], "squatter", 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if code, body := doDistinctChatRequest(t, gw, "wrongtype-secret", 1); code != 200 {
		t.Fatalf("request = %d %s, want 200: a lost ledger row never fails the request", code, body)
	}
	bk, _ := redisbudget.PlanAdjust(name, 0, 0, false)
	if got := c.HGet(ctx, bk[0], "billed_count").Val(); got != "1" {
		t.Errorf("billed_count = %q, want 1: the budget part must land although the ledger part failed", got)
	}
	tk, _ := redislimiter.PlanSettleTokens([]admission.Settlement{{Key: name}})
	if got := c.HGet(ctx, tk[1], "count").Val(); got != "1" {
		t.Errorf("token history count = %q, want 1", got)
	}
	out := logs.String()
	if !strings.Contains(out, `"msg":"ledger_record_failed"`) {
		t.Errorf("no ledger_record_failed line: %s", out)
	}
	if strings.Contains(out, "budget_redis_backend_unavailable") {
		t.Errorf("the budget part was reported lost although it landed: %s", out)
	}
}

// A cache hit reserves, then releases, in the one fused call: gate, budget
// reserve and one settlement.
func TestIntegrationCacheHitCostsThreeRoundTripsOverASharedClient(t *testing.T) {
	redisAddr := openTestRedisForMain(t)
	upstream, _ := newMockUpstream(t)
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	name := "cachehit-" + nonce
	gw, pipeline := newSharedRedisIntegrationServer(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_CACHEHIT", redisAddr, redisAddr, namedKeys(name, "cachehit-secret"))
	t.Cleanup(func() { _ = pipeline.Close() })
	reader := cmdGatewayMetricsReaderForTest()
	if code, body := doDistinctChatRequest(t, gw, "cachehit-secret", 0); code != 200 { // warms every script, bills once
		t.Fatalf("first request = %d %s", code, body)
	}
	before := redisRoundTripsByStore(t, reader)["shared"]
	if code, body := doDistinctChatRequest(t, gw, "cachehit-secret", 0); code != 200 { // same body: a cache hit
		t.Fatalf("repeat request = %d %s", code, body)
	}
	if got := redisRoundTripsByStore(t, reader)["shared"] - before; got != 3 {
		t.Errorf("a cache hit cost %d Redis round trips, want 3 (gate, reserve, one fused release)", got)
	}
}

// After SCRIPT FLUSH (or a failover) each script pays an EVALSHA miss and an
// EVAL once: the gate, the reserve and the fused settlement, then back to 3.
func TestIntegrationScriptFlushCostsOneExtraRoundTripPerScript(t *testing.T) {
	redisAddr := openTestRedisForMain(t)
	upstream, _ := newMockUpstream(t)
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	name := "scriptflush-" + nonce
	gw, pipeline := newSharedRedisIntegrationServer(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_FLUSH", redisAddr, redisAddr, namedKeys(name, "flush-secret"))
	t.Cleanup(func() { _ = pipeline.Close() })
	reader := cmdGatewayMetricsReaderForTest()
	if code, body := doDistinctChatRequest(t, gw, "flush-secret", 0); code != 200 {
		t.Fatalf("warm-up request = %d %s", code, body)
	}
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	if err := c.ScriptFlush(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	cost := func(n int) int64 {
		before := redisRoundTripsByStore(t, reader)["shared"]
		if code, body := doDistinctChatRequest(t, gw, "flush-secret", n); code != 200 {
			t.Fatalf("request %d = %d %s", n, code, body)
		}
		return redisRoundTripsByStore(t, reader)["shared"] - before
	}
	if got := cost(1); got != 6 {
		t.Errorf("the first request after SCRIPT FLUSH cost %d round trips, want 6 (three scripts, two each)", got)
	}
	if got := cost(2); got != 3 {
		t.Errorf("the next request cost %d round trips, want 3", got)
	}
}

// The contrast the changelog states: on separate clients, which keep the three
// standalone post-call scripts, the same cache hit costs 4 round trips (the
// gate, the budget reserve, the budget release and the token release).
func TestIntegrationCacheHitCostsFourRoundTripsOnSeparateClients(t *testing.T) {
	redisAddr := openTestRedisForMain(t)
	host, port, err := splitHostPortForTest(redisAddr)
	if err != nil {
		t.Fatal(err)
	}
	var other string
	switch host {
	case "localhost":
		other = "127.0.0.1"
	case "127.0.0.1":
		other = "localhost"
	default:
		t.Skipf("the test Redis is at %q, which has no loopback alias to spell it two ways", host)
	}
	upstream, _ := newMockUpstream(t)
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	name := "cachehit-separate-" + nonce
	gw, pipeline := newSharedRedisIntegrationServer(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_CACHEHIT_SEP", host+":"+port, other+":"+port, namedKeys(name, "cachehit-sep-secret"))
	t.Cleanup(func() { _ = pipeline.Close() })
	reader := cmdGatewayMetricsReaderForTest()
	if code, body := doDistinctChatRequest(t, gw, "cachehit-sep-secret", 0); code != 200 {
		t.Fatalf("first request = %d %s", code, body)
	}
	total := func() (n int64) {
		for _, v := range redisRoundTripsByStore(t, reader) {
			n += v
		}
		return n
	}
	before := total()
	if code, body := doDistinctChatRequest(t, gw, "cachehit-sep-secret", 0); code != 200 {
		t.Fatalf("repeat request = %d %s", code, body)
	}
	if got := total() - before; got != 4 {
		t.Errorf("a cache hit on separate clients cost %d Redis round trips, want 4 (gate, reserve, budget release, token release)", got)
	}
}
