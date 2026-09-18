package alerting

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// alertingTestFakeSecret builds a fake shared secret via concatenation
// rather than a literal, mirroring gracefulShutdownTestSecret's/
// embeddingsTestFakeCredential's own established convention elsewhere
// in this codebase so it doesn't read as a real credential to
// secret-scanning tooling.
func alertingTestFakeSecret(suffix string) string {
	return "not-a-real-" + "alerting-test-" + suffix
}

// TestWebhookNotifierDeliversRealHTTPRequest proves a real end-to-end
// delivery: a real httptest.Server receives a real POST with the
// expected JSON body and webhook-id/webhook-timestamp headers.
func TestWebhookNotifierDeliversRealHTTPRequest(t *testing.T) {
	received := make(chan *http.Request, 1)
	var bodyBytes []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyBytes = b
		received <- r
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	n := NewWebhookNotifier(server.URL, "", discardLogger())
	n.Notify(t.Context(), Event{
		Type:      "budget_threshold_crossed",
		Timestamp: time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC),
		Fields:    map[string]any{"key_id": "team-alpha", "percent_bucket": 80},
	})

	select {
	case r := <-received:
		if r.Header.Get("webhook-id") == "" {
			t.Error("webhook-id header is empty")
		}
		if r.Header.Get("webhook-timestamp") == "" {
			t.Error("webhook-timestamp header is empty")
		}
		if r.Header.Get("webhook-signature") != "" {
			t.Error("webhook-signature header set with no signing secret configured, want empty")
		}
		if !strings.Contains(string(bodyBytes), "budget_threshold_crossed") ||
			!strings.Contains(string(bodyBytes), "team-alpha") {
			t.Errorf("body = %s, want it to contain the event type and key_id", bodyBytes)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the webhook delivery")
	}
}

// TestWebhookNotifierSignsPayloadWhenSecretConfigured proves the HMAC
// signature is genuinely verifiable against the Standard Webhooks
// scheme (id.timestamp.body, HMAC-SHA256, "v1,<base64>") -- recomputed
// independently on the "receiving" side, not just checking the header
// is non-empty.
func TestWebhookNotifierSignsPayloadWhenSecretConfigured(t *testing.T) {
	secret := alertingTestFakeSecret("plain-shared-secret")
	received := make(chan *http.Request, 1)
	var bodyBytes []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyBytes = b
		received <- r
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	n := NewWebhookNotifier(server.URL, secret, discardLogger())
	n.Notify(t.Context(), Event{Type: "budget_threshold_crossed", Timestamp: time.Now(), Fields: map[string]any{"key_id": "team-alpha"}})

	var r *http.Request
	select {
	case r = <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the webhook delivery")
	}

	sig := r.Header.Get("webhook-signature")
	if !strings.HasPrefix(sig, "v1,") {
		t.Fatalf("webhook-signature = %q, want a v1,<base64> prefix", sig)
	}

	id := r.Header.Get("webhook-id")
	timestamp := r.Header.Get("webhook-timestamp")
	toSign := id + "." + timestamp + "." + string(bodyBytes)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(toSign))
	want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if sig != want {
		t.Errorf("webhook-signature = %q, want %q (recomputed independently over id.timestamp.body)", sig, want)
	}
}

// TestNewWebhookNotifierDecodesWhsecPrefixedSecret proves a
// "whsec_"-prefixed base64 secret is decoded before use as the HMAC
// key, per the Standard Webhooks convention -- the signature must be
// computed with the DECODED bytes, not the literal "whsec_..." string.
func TestNewWebhookNotifierDecodesWhsecPrefixedSecret(t *testing.T) {
	rawSecret := []byte(alertingTestFakeSecret("sixteen-byte-key"))[:16]
	whsecSecret := "whsec_" + base64.StdEncoding.EncodeToString(rawSecret)

	received := make(chan *http.Request, 1)
	var bodyBytes []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyBytes = b
		received <- r
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	n := NewWebhookNotifier(server.URL, whsecSecret, discardLogger())
	n.Notify(t.Context(), Event{Type: "budget_threshold_crossed", Timestamp: time.Now(), Fields: map[string]any{}})

	var r *http.Request
	select {
	case r = <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the webhook delivery")
	}

	sig := r.Header.Get("webhook-signature")
	id := r.Header.Get("webhook-id")
	timestamp := r.Header.Get("webhook-timestamp")
	toSign := id + "." + timestamp + "." + string(bodyBytes)

	macWithDecoded := hmac.New(sha256.New, rawSecret)
	macWithDecoded.Write([]byte(toSign))
	wantWithDecoded := "v1," + base64.StdEncoding.EncodeToString(macWithDecoded.Sum(nil))

	macWithRawString := hmac.New(sha256.New, []byte(whsecSecret))
	macWithRawString.Write([]byte(toSign))
	wrongIfRawString := "v1," + base64.StdEncoding.EncodeToString(macWithRawString.Sum(nil))

	if sig != wantWithDecoded {
		t.Errorf("webhook-signature = %q, want %q (HMAC key must be the base64-decoded bytes)", sig, wantWithDecoded)
	}
	if sig == wrongIfRawString {
		t.Error("signature matches using the raw whsec_-prefixed string as the key -- the prefix was not stripped/decoded")
	}
}

// TestWebhookNotifierRetriesOnFailureThenSucceeds proves the retry
// loop actually retries (not just "fails once and gives up"): a server
// that fails the first 2 requests and succeeds the 3rd eventually
// receives a successful delivery, reusing the SAME webhook-id across
// every attempt.
func TestWebhookNotifierRetriesOnFailureThenSucceeds(t *testing.T) {
	var attempts int32
	var mu sync.Mutex
	var seenIDs []string
	done := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenIDs = append(seenIDs, r.Header.Get("webhook-id"))
		mu.Unlock()
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		close(done)
	}))
	defer server.Close()

	notifier := NewWebhookNotifier(server.URL, "", discardLogger())
	notifier.Notify(t.Context(), Event{Type: "budget_threshold_crossed", Timestamp: time.Now(), Fields: map[string]any{}})

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the 3rd (successful) attempt")
	}

	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want exactly 3 (2 failures + 1 success)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, id := range seenIDs {
		if id == "" || (i > 0 && id != seenIDs[0]) {
			t.Errorf("seenIDs = %v, want every attempt to reuse the SAME non-empty webhook-id", seenIDs)
			break
		}
	}
}

// TestWebhookNotifierDoesNotRetryOnPermanentClientErrorStatus is the
// regression proof for a real gap an audit found: a 4xx response (a
// client-error class that will fail identically on retry — bad
// signature, wrong path, auth failure) previously burned all
// maxDeliveryAttempts identically to a transient 5xx. Calls deliver
// directly (synchronous, same package) rather than via the async
// Notify, so the exact request count is observable without any
// backoff-timing guesswork.
func TestWebhookNotifierDoesNotRetryOnPermanentClientErrorStatus(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	notifier := NewWebhookNotifier(server.URL, "", discardLogger())
	notifier.deliver(t.Context(), Event{Type: "budget_threshold_crossed", Timestamp: time.Now(), Fields: map[string]any{}})

	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want exactly 1 -- a 4xx must never be retried", got)
	}
}

// TestWebhookNotifierDoesNotRetryOnMalformedURL proves a webhook URL
// that fails at request-construction time (rejected by
// http.NewRequestWithContext before any network call) is also treated
// as permanent -- never retried, never even reaching the network.
func TestWebhookNotifierDoesNotRetryOnMalformedURL(t *testing.T) {
	// A control character in the URL is rejected by net/url's own
	// parsing inside http.NewRequestWithContext, deterministically,
	// before any network call is attempted.
	notifier := NewWebhookNotifier("http://example.invalid/\x7f", "", discardLogger())

	start := time.Now()
	notifier.deliver(t.Context(), Event{Type: "budget_threshold_crossed", Timestamp: time.Now(), Fields: map[string]any{}})
	elapsed := time.Since(start)

	// A retried delivery would wait at least baseBackoff (500ms) before
	// its second attempt; a permanent, single-attempt failure returns
	// near-instantly. A generous threshold well under one backoff
	// interval rules out any retry having occurred.
	if elapsed >= baseBackoff {
		t.Errorf("deliver took %v, want near-instant (<%v) -- a malformed URL must never be retried", elapsed, baseBackoff)
	}
}

// TestWebhookNotifierStillRetriesOnTransportError closes a real test-
// coverage gap an audit found: every prior failure test drove an
// httptest.Server returning an HTTP status, never a transport-level
// failure (connection refused). Dials a real, immediately-closed
// listener so the connection is refused deterministically, and proves
// the retry loop still treats it as transient (all maxDeliveryAttempts
// attempted), unlike a 4xx.
func TestWebhookNotifierStillRetriesOnTransportError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	deadURL := "http://" + ln.Addr().String()
	if closeErr := ln.Close(); closeErr != nil {
		t.Fatalf("closing listener: %v", closeErr)
	}

	notifier := NewWebhookNotifier(deadURL, "", discardLogger())

	start := time.Now()
	notifier.deliver(t.Context(), Event{Type: "budget_threshold_crossed", Timestamp: time.Now(), Fields: map[string]any{}})
	elapsed := time.Since(start)

	// A permanent, single-attempt failure returns near-instantly; a
	// retried one waits through the full backoff schedule
	// (baseBackoff + 2*baseBackoff, plus jitter) before giving up --
	// proving all 3 attempts were genuinely made, not just 1.
	minRetriedElapsed := baseBackoff + 2*baseBackoff
	if elapsed < minRetriedElapsed {
		t.Errorf("deliver took %v, want at least %v -- a connection-refused error must still be retried like any transient failure", elapsed, minRetriedElapsed)
	}
}

// TestWebhookNotifierGivesUpAfterMaxAttempts proves the retry loop is
// genuinely bounded: a server that ALWAYS fails receives exactly
// maxDeliveryAttempts requests, never more.
func TestWebhookNotifierGivesUpAfterMaxAttempts(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	notifier := NewWebhookNotifier(server.URL, "", discardLogger())
	notifier.Notify(t.Context(), Event{Type: "budget_threshold_crossed", Timestamp: time.Now(), Fields: map[string]any{}})

	// Bounded wait comfortably longer than maxDeliveryAttempts's own
	// worst-case backoff total (500ms + 1s + jitter), then confirm the
	// count settled and never grew further.
	time.Sleep(3 * time.Second)
	if got := atomic.LoadInt32(&attempts); got != int32(maxDeliveryAttempts) {
		t.Errorf("attempts = %d, want exactly %d (bounded, not unbounded)", got, maxDeliveryAttempts)
	}
}
