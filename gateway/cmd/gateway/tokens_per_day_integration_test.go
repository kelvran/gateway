package main

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
)

// dayMessagesBody is messagesBody with a distinct user turn per call: an
// identical body would be an in-process cache hit, which is not billable
// and releases its reservation untouched -- the day bucket, like the TPM
// one, is only ever debited by real upstream usage.
func dayMessagesBody(i int) string {
	return fmt.Sprintf(`{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"day request %d"}]}`, i)
}

// Round-5 Tier S item 24 (2026-10-15): a virtual key's tokens_per_day on
// the wire. The mock upstream bills 11 tokens per call and the day-capped
// key allows 15: the first call reserves the cold bucket (15 -> 0, settled
// to 4), the second runs on the running mean into overdraft (4 -> -7), the
// third is a 429 rate_limit_exceeded whose Retry-After is the day bucket's
// own wait -- 7 tokens of debt at 15/86400 per second, 40 320 s -- not the
// 1-30 s per-key backoff, and never more than a day.
func TestIntegrationMessagesTokensPerDayRejectionCarriesTheDayWait(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	day := map[string]string{"x-api-key": "day-secret"}

	for i := 1; i <= 2; i++ {
		if status, _, raw := postMessages(t, gw.URL, day, dayMessagesBody(i)); status != http.StatusOK {
			t.Fatalf("call %d = %d; body %s", i, status, raw)
		}
	}
	status, header, raw := postMessages(t, gw.URL, day, dayMessagesBody(3))
	if status != http.StatusTooManyRequests {
		t.Fatalf("call 3 = %d, want 429; body %s", status, raw)
	}
	if e := decodeAnthropicError(t, raw); e.Error.Type != "rate_limit_error" || e.Error.Code != "rate_limit_exceeded" {
		t.Errorf("envelope = %+v, want rate_limit_error / rate_limit_exceeded (the day cap reuses the rate-limit code)", e.Error)
	}
	n, err := strconv.Atoi(header.Get("Retry-After"))
	if err != nil || n <= 3600 || n > 86400 {
		t.Errorf("Retry-After = %q, want the day wait in whole seconds: above an hour, at most a day", header.Get("Retry-After"))
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (the third request never left the gateway)", got)
	}
}
