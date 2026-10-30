package main

import "testing"

// The guard that keeps the integration tests off a real Redis: every spelling
// of port 6379 is refused (net's dialer accepts a leading '+' and leading
// zeros), so is a non-loopback host, and a plain scratch port is accepted.
func TestCheckScratchAddrRefusesTheRealRedisInEverySpelling(t *testing.T) {
	for _, bad := range []string{
		"", "127.0.0.1", "127.0.0.1:6379", "localhost:6379", "[::1]:6379",
		"127.0.0.1:06379", "127.0.0.1:006379", "127.0.0.1:+6379", "127.0.0.1:6379 ",
		"127.0.0.1:redis", "127.0.0.1:65536", "127.0.0.1:-1",
		"10.0.0.5:16379", "redis.internal:16379", "0.0.0.0:16379",
	} {
		if err := checkScratchAddr(bad); err == nil {
			t.Errorf("checkScratchAddr(%q) accepted an address that can reach a real Redis", bad)
		}
	}
	for _, good := range []string{"127.0.0.1:16379", "localhost:16380", "[::1]:49153"} {
		if err := checkScratchAddr(good); err != nil {
			t.Errorf("checkScratchAddr(%q) = %v, want accepted", good, err)
		}
	}
}
