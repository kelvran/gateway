package main

import (
	"sync"

	"github.com/redis/go-redis/v9"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/redishook"
)

// sharedStoreLabel is the kelvran.redis.store value of the one hook on the
// shared client: its commands belong to the rate limiter, the budget and the
// ledger at once, so no single store name is true. The three stores keep their
// own names when they run on clients of their own.
const sharedStoreLabel = "shared"

// sharedRedis is the one go-redis client the rate-limit, budget and ledger
// stores share when rate_limit.redis_addr and budget.redis_addr are the same
// Redis (the ledger always follows the budget) -- round-5 item 10 phase 2c-1,
// 2026-10-23. One pool, one hook, one close: the stores run over it through
// their NewOn constructors, and a single client is what lets a later phase fuse
// their scripts into one round trip before the upstream call and one after it.
type sharedRedis struct {
	client *redis.Client
	mu     sync.Mutex
	holds  int
}

// acquire registers one more holder of the client and returns its release. The
// last outstanding release closes the client; releasing the same handle twice
// counts once, so a store's Close stays idempotent.
func (s *sharedRedis) acquire() func() error {
	s.mu.Lock()
	s.holds++
	s.mu.Unlock()
	var once sync.Once
	return func() error {
		var err error
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.holds--
			if s.holds == 0 {
				err = s.client.Close()
			}
		})
		return err
	}
}

// sameRedisOptions reports whether two stores may share one client: the same
// non-empty address, username, resolved password and TLS setting -- the four
// things redisOptions sets. The comparison is deliberately conservative: two
// spellings of one host ("127.0.0.1:6379", "localhost:6379") do not match, and
// the stores then keep clients of their own.
func sameRedisOptions(a, b redis.Options) bool {
	return a.Addr != "" && a.Addr == b.Addr &&
		a.Username == b.Username && a.Password == b.Password &&
		(a.TLSConfig != nil) == (b.TLSConfig != nil)
}

// openSharedRedis opens the shared client when the rate limiter and the budget
// are both Redis-backed on the same Redis, and returns nil otherwise (the
// stores then open clients of their own, as before). The client carries
// exactly one hook: go-redis appends hooks, so one per store would count every
// command once per store. Opening never dials.
func openSharedRedis(cfg *controlplane.Config) *sharedRedis {
	if cfg.RateLimit.RedisAddr == "" || cfg.Budget.RedisAddr == "" {
		return nil
	}
	limiter := redisOptions(cfg.RateLimit.RedisAddr, cfg.RateLimit.Redis)
	budget := redisOptions(cfg.Budget.RedisAddr, cfg.Budget.Redis)
	if !sameRedisOptions(limiter, budget) {
		return nil
	}
	client := redis.NewClient(&budget)
	client.AddHook(redishook.New(sharedStoreLabel))
	return &sharedRedis{client: client}
}
