// Package alerting delivers a direct-from-Go webhook push for
// operational signals the gateway already computes but never delivers
// anywhere, per docs/upgrade-research/operator-alerting-integrations-
// 2026-09-15.md Finding 4 (the same pattern LiteLLM's own budget-alerts
// webhook and Helicone's Slack/email alerts both ship). A pure leaf
// package: no project-internal imports, so any future signal source
// (a rate-limiter fail-open, a health-probe transition) can depend on
// it without this package ever needing to know about any of them.
//
// The sender is built with docs/upgrade-research/operator-alerting-
// integrations-2026-09-15.md Finding 5's own baseline properties from
// the start, not as a naive version to "harden later": a stable
// per-event ID for receiver-side dedup, optional HMAC-SHA256 signing
// per the Standard Webhooks specification (standardwebhooks.com,
// verified directly against that spec's own current text, not
// assumed), a bounded exponential-backoff-with-jitter retry that gives
// up rather than looping forever, and an async send so a slow or dead
// receiver never blocks the caller.
package alerting

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	mathrand "math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Event is one alertable signal. Type names the kind of event
// ("budget_threshold_crossed" today; a future signal adds its own Type
// value, never overloads this one). Fields carries event-specific data
// as a plain map, deliberately loose (not a Go struct per event type)
// since this package has no way to know every future event shape a
// caller might add.
type Event struct {
	Type      string
	Timestamp time.Time
	Fields    map[string]any
}

// Notifier delivers an Event to wherever it's configured to go.
// Deliberately an interface, not a concrete *WebhookNotifier
// dependency, so a caller can no-op in tests without a real HTTP
// server — mirrors configpropagation.Publisher's own established shape
// for an optional, injectable side-effect.
type Notifier interface {
	Notify(ctx context.Context, event Event)
}

const (
	maxDeliveryAttempts = 3
	baseBackoff         = 500 * time.Millisecond
)

// WebhookNotifier POSTs a JSON payload to a configured URL.
type WebhookNotifier struct {
	url           string
	signingSecret string
	client        *http.Client
	logger        *slog.Logger
}

// NewWebhookNotifier constructs a WebhookNotifier. signingSecret may be
// empty — an unsigned payload is a real, accepted choice (e.g. a plain
// Slack Incoming Webhook has no signature-verification concept at
// all), not a silent gap. When non-empty and prefixed "whsec_" (the
// Standard Webhooks convention for a base64-encoded secret), the
// prefix is stripped and the remainder base64-decoded before use as
// the HMAC key, for direct interoperability with any Standard-
// Webhooks-compliant receiver library. A secret in neither form is
// still accepted — used as raw bytes directly — logged once as
// non-spec-compliant rather than rejected outright, since an operator
// sharing an arbitrary secret with their own receiver has no need to
// follow that exact convention.
func NewWebhookNotifier(url, signingSecret string, logger *slog.Logger) *WebhookNotifier {
	key := []byte(signingSecret)
	if secret, ok := strings.CutPrefix(signingSecret, "whsec_"); ok {
		if decoded, err := base64.StdEncoding.DecodeString(secret); err == nil {
			key = decoded
		} else {
			logger.Warn("alerting_webhook_signing_secret_not_base64", "error", err)
		}
	} else if signingSecret != "" {
		logger.Warn("alerting_webhook_signing_secret_not_whsec_prefixed", "detail", "using raw bytes as the HMAC key -- not Standard-Webhooks-compliant, but still a real, usable shared secret")
	}
	return &WebhookNotifier{
		url:           url,
		signingSecret: string(key),
		client:        &http.Client{Timeout: 10 * time.Second},
		logger:        logger,
	}
}

// Notify sends event in its own goroutine — per Finding 5's own "doing
// the send from a background/async path so a slow or dead receiver
// never blocks the caller" property, Notify itself returns
// immediately; the caller never learns the outcome, matching
// telemetry.RecordBudgetThresholdCrossed's own identical fire-and-
// forget posture for the sibling signal this event usually
// accompanies. context.WithoutCancel: the delivery goroutine must
// outlive the request context that triggered it (the whole point of
// not blocking the caller), but still carries any trace/baggage values
// already attached. Never tracked against any shutdown WaitGroup — a
// disclosed, accepted scope limit: an in-flight retry loop can be
// abandoned mid-backoff on process exit, exactly like every other
// best-effort telemetry side-channel in this codebase.
func (w *WebhookNotifier) Notify(ctx context.Context, event Event) {
	go w.deliver(context.WithoutCancel(ctx), event)
}

func (w *WebhookNotifier) deliver(ctx context.Context, event Event) {
	id, err := newEventID()
	if err != nil {
		w.logger.Warn("alerting_webhook_id_generation_failed", "error", err)
		return
	}

	body, err := json.Marshal(map[string]any{
		"id":        id,
		"type":      event.Type,
		"timestamp": event.Timestamp.UTC().Format(time.RFC3339),
		"data":      event.Fields,
	})
	if err != nil {
		w.logger.Warn("alerting_webhook_marshal_failed", "event_type", event.Type, "error", err)
		return
	}

	var lastErr error
	for attempt := 0; attempt < maxDeliveryAttempts; attempt++ {
		if attempt > 0 {
			// Exponential backoff (500ms, 1s) with up to 50% jitter —
			// bounded, per Finding 5's own "ending in a dead-letter/
			// give-up state rather than an unbounded loop" property; a
			// full dead-letter queue is real, larger, named future
			// work, not built here.
			backoff := baseBackoff * time.Duration(1<<uint(attempt-1))
			jitter := time.Duration(mathrand.Int64N(int64(backoff) / 2))
			select {
			case <-time.After(backoff + jitter):
			case <-ctx.Done():
				return
			}
		}
		if lastErr = w.send(ctx, id, body); lastErr == nil {
			return
		}
	}
	w.logger.Warn("alerting_webhook_delivery_failed", "event_type", event.Type, "event_id", id, "attempts", maxDeliveryAttempts, "error", lastErr)
}

func (w *WebhookNotifier) send(ctx context.Context, id string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", id)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("webhook-timestamp", timestamp)
	if w.signingSecret != "" {
		req.Header.Set("webhook-signature", signPayload(w.signingSecret, id, timestamp, body))
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook endpoint returned status %d", resp.StatusCode)
	}
	return nil
}

// signPayload computes an HMAC-SHA256 signature over "id.timestamp.body",
// per the Standard Webhooks specification (standardwebhooks.com/
// github.com/standard-webhooks/standard-webhooks, verified directly
// against that spec's own current text before implementing this, not
// assumed) — the identical scheme Svix's own webhook platform uses.
// Returned as "v1,<base64>", that spec's own documented symmetric-
// signature header value shape (a real receiver may also accept
// multiple space-delimited "v1,..." values for zero-downtime secret
// rotation; this sender only ever emits one).
func signPayload(secret, id, timestamp string, body []byte) string {
	toSign := id + "." + timestamp + "." + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(toSign))
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// newEventID generates a stable, unique identifier for one logical
// event — reused across every retry attempt for that SAME event (never
// regenerated per attempt), so a receiver can dedupe an at-least-once
// delivery per Finding 5's own "a stable per-event ID" property. 16
// random bytes, hex-encoded — no UUID library dependency needed for a
// value that only needs to be unique, never parsed or structurally
// validated by this codebase itself.
func newEventID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating event id: %w", err)
	}
	return "evt_" + hex.EncodeToString(b), nil
}
