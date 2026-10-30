package redisfused

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/redis/go-redis/v9"

	"github.com/kelvran/gateway/gateway/internal/budget/redisbudget"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/admission"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/redislimiter"
	"github.com/kelvran/gateway/gateway/internal/spendledger"
	"github.com/kelvran/gateway/gateway/internal/spendledger/redisledger"
)

// Redis for these tests: a real server, never a mock (docs/testing/TESTING.md
// section 4). By default one redis:7-alpine container per test binary, like
// the three stores' own tests. KELVRAN_TEST_REDIS_ADDR points the binary at
// an already running SCRATCH server instead (a developer without Docker), and
// KELVRAN_TEST_REDIS_REPLICA_ADDR at a scratch server that is a read-only
// replica; both are refused unless they are loopback and not port 6379, which
// is where a pilot's Redis lives. No test flushes a database: every test uses
// its own key ids. Two tests issue SCRIPT FLUSH, which empties the server's
// script cache and changes the round trips another binary counts, so run the
// packages that share a scratch server one at a time (go test -p 1).

const (
	envRedisAddr   = "KELVRAN_TEST_REDIS_ADDR"
	envReplicaAddr = "KELVRAN_TEST_REDIS_REPLICA_ADDR"
)

// redisProvider starts the test Redis on first use. A failure to start it is
// recorded, not raised inside the Once: a Fatalf there would mark the Once done
// with no address, and every later test would then dial go-redis's default
// localhost:6379, the real Redis. start is a field so the failure path can be
// tested.
type redisProvider struct {
	once  sync.Once
	addr  string
	err   error
	stop  func()
	start func(context.Context) (addr string, stop func(), err error)
}

func (p *redisProvider) get() (string, error) {
	p.once.Do(func() {
		addr, stop, err := p.start(context.Background())
		if err != nil {
			p.err = err
			return
		}
		p.addr, p.stop = addr, stop
	})
	return p.addr, p.err
}

func startRedisContainer(ctx context.Context) (string, func(), error) {
	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		if container != nil {
			_ = container.Terminate(ctx)
		}
		return "", nil, fmt.Errorf("starting test Redis container (or set %s to a scratch server): %w", envRedisAddr, err)
	}
	stop := func() { _ = container.Terminate(ctx) }
	conn, err := container.ConnectionString(ctx)
	if err != nil {
		stop()
		return "", nil, fmt.Errorf("getting test Redis connection string: %w", err)
	}
	return strings.TrimPrefix(conn, "redis://"), stop, nil
}

var testRedis = &redisProvider{start: startRedisContainer}

func TestMain(m *testing.M) {
	code := m.Run()
	if testRedis.stop != nil {
		testRedis.stop()
	}
	os.Exit(code)
}

// checkRedisAddr reports why addr must not be dialed by a test: empty (go-redis
// would dial its default localhost:6379), malformed, or port 6379 however it is
// spelled. net's dialer accepts a leading '+' and leading zeros in a port, so
// the number is parsed, and any spelling that is not its canonical decimal form
// is refused too. Port 6379 is where a real Redis lives.
func checkRedisAddr(addr string) error {
	if addr == "" {
		return fmt.Errorf("an empty address would dial go-redis's default localhost:6379, the real Redis")
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || strconv.FormatUint(n, 10) != port {
		return fmt.Errorf("the port must be a plain decimal number")
	}
	if n == 6379 {
		return fmt.Errorf("refusing port 6379, that is where a real Redis lives; start a scratch server on another port")
	}
	return nil
}

// portOf is the port of an address checkRedisAddr has accepted.
func portOf(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return port
}

// refuseRealRedis fails the test unless checkRedisAddr accepts addr.
func refuseRealRedis(t *testing.T, what, addr string) {
	t.Helper()
	if err := checkRedisAddr(addr); err != nil {
		t.Fatalf("%s=%q: %v", what, addr, err)
	}
}

// scratchAddr validates an env-provided address: loopback host, a plain port
// that is not 6379.
func scratchAddr(t *testing.T, env string) string {
	t.Helper()
	addr := os.Getenv(env)
	if addr == "" {
		return ""
	}
	refuseRealRedis(t, env, addr)
	host, _, _ := net.SplitHostPort(addr)
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		t.Fatalf("%s=%q: refusing a non-loopback host", env, addr)
	}
	return addr
}

// redisAddr is the address of the test Redis, started on first use.
func redisAddr(t *testing.T) string {
	t.Helper()
	if addr := scratchAddr(t, envRedisAddr); addr != "" {
		return addr
	}
	addr, err := testRedis.get()
	if err != nil {
		t.Fatalf("%v", err)
	}
	refuseRealRedis(t, "test Redis container", addr)
	return addr
}

// replicaAddr is a read-only replica: the scratch one from the environment,
// else a fresh container told to replicate from nowhere.
func replicaAddr(t *testing.T) string {
	t.Helper()
	if addr := scratchAddr(t, envReplicaAddr); addr != "" {
		// Both addresses are loopback with a canonical port, so equal ports name
		// one server whatever the host is spelled.
		if primary := os.Getenv(envRedisAddr); primary != "" && portOf(addr) == portOf(primary) {
			t.Fatalf("%s names the same server as %s: the replica test would turn the main scratch server into a read-only replica of nowhere", envReplicaAddr, envRedisAddr)
		}
		return addr
	}
	ctx := context.Background()
	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		if container != nil {
			_ = container.Terminate(ctx)
		}
		t.Fatalf("starting replica container (or set %s): %v", envReplicaAddr, err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })
	conn, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("getting replica connection string: %v", err)
	}
	addr := strings.TrimPrefix(conn, "redis://")
	refuseRealRedis(t, "replica container", addr)
	c := redis.NewClient(&redis.Options{Addr: addr})
	defer func() { _ = c.Close() }()
	if err := c.Do(ctx, "REPLICAOF", "127.0.0.1", "1").Err(); err != nil {
		t.Fatalf("REPLICAOF: %v", err)
	}
	return addr
}

// newClient is a client without retries, so a failure is one failure.
func newClient(t *testing.T, addr string, hooks ...redis.Hook) *redis.Client {
	t.Helper()
	refuseRealRedis(t, "client address", addr)
	c := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	for _, h := range hooks {
		c.AddHook(h)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

var (
	idCounter atomic.Uint64
	// runNonce keeps ids apart across runs too: a scratch server outlives the
	// test binary, a container does not.
	runNonce = strconv.FormatInt(time.Now().UnixNano(), 36)
)

// uniqueID is a fresh virtual-key id per call so tests never share state.
func uniqueID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("fz-%s-%s-%d", runNonce, strings.ReplaceAll(t.Name(), "/", "_"), idCounter.Add(1))
}

var ledgerOpts = spendledger.Options{Retention: 48 * time.Hour, MaxSessionsPerKey: 2, MaxRowsPerKey: 100}

// rig is a settler over one client plus the three standalone stores on the
// SAME client, so a differential test runs both paths against one server.
type rig struct {
	client  *redis.Client
	settler *Settler
	budget  *redisbudget.Backend
	limiter *redislimiter.Limiter
	ledger  *redisledger.Store
}

func newRig(t *testing.T, hooks ...redis.Hook) *rig {
	t.Helper()
	c := newClient(t, redisAddr(t), hooks...)
	led := redisledger.NewOn(c, ledgerOpts, func() error { return nil })
	return &rig{
		client:  c,
		settler: New(c, led, func() error { return nil }),
		budget:  redisbudget.NewOn(c, func() error { return nil }),
		limiter: redislimiter.NewOn(c, func() error { return nil }),
		ledger:  led,
	}
}

// countHook records the commands that cross it, by name, one entry per round
// trip (a pipeline counts as one entry named "pipeline").
type countHook struct {
	mu   sync.Mutex
	cmds []string
}

func (h *countHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *countHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.mu.Lock()
		h.cmds = append(h.cmds, cmd.Name())
		h.mu.Unlock()
		return next(ctx, cmd)
	}
}

func (h *countHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.mu.Lock()
		h.cmds = append(h.cmds, "pipeline")
		h.mu.Unlock()
		return next(ctx, cmds)
	}
}

func (h *countHook) reset() {
	h.mu.Lock()
	h.cmds = nil
	h.mu.Unlock()
}

func (h *countHook) names() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.cmds...)
}

// entryAt is a ledger entry at a fixed hour, so its hash key is stable.
func entryAt(id, session string, spend int64) spendledger.Entry {
	return spendledger.Entry{
		Key: id, Model: "claude-test", Tool: "tool-a", Session: session,
		At: time.Date(2026, 10, 30, 10, 20, 0, 0, time.UTC), SpendNanoUSD: spend, Requests: 1,
	}
}

func f64(v float64) *float64 { return &v }

// settle is a one-dimension token settlement for key.
func settle(key string, delta float64, real *float64) admission.Settlement {
	return admission.Settlement{Key: key, DeltaIncrement: delta, RealTokens: real}
}

// stateOf dumps every key a request for id can touch, by role, so two runs
// under different ids compare equal when they did the same thing. Strings are
// their value, hashes their sorted fields, sets their sorted members; a
// missing key is "-". ttl maps a role to its PTTL in ms (-2 missing, -1 none).
func stateOf(t *testing.T, ctx context.Context, c *redis.Client, id string, tokenKeys []string, entry spendledger.Entry, led *redisledger.Store) (state map[string]string, ttl map[string]int64) {
	t.Helper()
	state, ttl = map[string]string{}, map[string]int64{}
	dump := func(role, key string) {
		typ, err := c.Type(ctx, key).Result()
		if err != nil {
			t.Fatalf("TYPE %s: %v", key, err)
		}
		switch typ {
		case "none":
			state[role] = "-"
		case "string":
			state[role] = "s:" + c.Get(ctx, key).Val()
		case "hash":
			m := c.HGetAll(ctx, key).Val()
			parts := make([]string, 0, len(m))
			for k, v := range m {
				parts = append(parts, k+"="+v)
			}
			sort.Strings(parts)
			state[role] = "h:" + strings.Join(parts, ",")
		case "set":
			m := c.SMembers(ctx, key).Val()
			sort.Strings(m)
			state[role] = "set:" + strings.Join(m, ",")
		default:
			state[role] = "type:" + typ
		}
		ttl[role] = c.PTTL(ctx, key).Val().Milliseconds()
	}
	bk, _ := redisbudget.PlanAdjust(id, 0, 0, false)
	dump("budget", bk[0])
	for i, k := range tokenKeys {
		tk, _ := redislimiter.PlanSettleTokens([]admission.Settlement{{Key: k}})
		dump(fmt.Sprintf("tat%d", i), tk[0])
		dump(fmt.Sprintf("hist%d", i), tk[1])
	}
	e := entry
	e.Key = id
	lk, _, err := led.PlanRecord(e)
	if err != nil {
		t.Fatalf("PlanRecord: %v", err)
	}
	dump("ledger.hash", lk[0])
	dump("ledger.sessions", lk[1])
	dump("ledger.cells", lk[2])
	return state, ttl
}
