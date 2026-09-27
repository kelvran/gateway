package bedrockguard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestEffectiveCredentialsReflectsInitialConfig proves a Detector with no
// *File fields configured just reflects its own construction-time Config
// values -- the exact behavior before this feature existed.
func TestEffectiveCredentialsReflectsInitialConfig(t *testing.T) {
	d := New(testConfig(""), nil)
	got := d.effectiveCredentials()
	if got.AccessKeyID != "test-access-key" || got.SecretAccessKey != "test-secret-key" {
		t.Errorf("effectiveCredentials() = %+v, want AccessKeyID=test-access-key SecretAccessKey=test-secret-key", got)
	}
}

// TestRunCredentialReloadLoopPicksUpFileRotationWithinOneInterval is the
// bedrockguard-package mirror of dataplane's own
// TestRunCredentialReloadLoopPicksUpFileRotationWithinOneInterval --
// proves the atomic-swap mechanism works for a real Detector, not just
// the shared credentialstate primitive in isolation.
func TestRunCredentialReloadLoopPicksUpFileRotationWithinOneInterval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access-key-id")
	if err := os.WriteFile(path, []byte("initial-key"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := testConfig("")
	cfg.AccessKeyIDFile = path
	d := New(cfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const reloadInterval = 10 * time.Millisecond
	go d.RunCredentialReloadLoop(ctx, reloadInterval, discardLogger())

	if err := os.WriteFile(path, []byte("rotated-key"), 0o600); err != nil {
		t.Fatalf("WriteFile (rotation): %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := d.effectiveCredentials().AccessKeyID; got == "rotated-key" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("effectiveCredentials().AccessKeyID = %q after 2s, want %q (rotation was never picked up)", d.effectiveCredentials().AccessKeyID, "rotated-key")
		}
		time.Sleep(reloadInterval)
	}
}

// TestRunCredentialReloadLoopNoOpForEnvOnlyDetector proves the feature's
// opt-in guarantee: a Detector configured entirely via plain Config
// values (no *File fields) returns immediately rather than starting a
// real ticker loop -- mirrors dataplane's own
// TestRunCredentialReloadLoopNoOpForEnvOnlyDeployment. Called directly
// (not via `go`) with a live, un-canceled context specifically so that
// a regression which accidentally started a real loop would hang this
// test until its own timeout, rather than silently passing.
func TestRunCredentialReloadLoopNoOpForEnvOnlyDetector(t *testing.T) {
	d := New(testConfig(""), nil)
	d.RunCredentialReloadLoop(context.Background(), 10*time.Millisecond, discardLogger())
}

// TestRunCredentialReloadLoopZeroIntervalIsNoOp mirrors dataplane's own
// TestRunCredentialReloadLoopZeroIntervalIsNoOp.
func TestRunCredentialReloadLoopZeroIntervalIsNoOp(t *testing.T) {
	cfg := testConfig("")
	cfg.AccessKeyIDFile = filepath.Join(t.TempDir(), "access-key-id")
	d := New(cfg, nil)
	d.RunCredentialReloadLoop(context.Background(), 0, discardLogger())
}

// TestReloadCredentialsKeepsLastKnownGoodOnReadError mirrors dataplane's
// own TestReloadDeploymentCredentialsKeepsLastKnownGoodOnReadError: a
// read failure (file missing) must never swap in an empty/partial value.
func TestReloadCredentialsKeepsLastKnownGoodOnReadError(t *testing.T) {
	cfg := testConfig("")
	cfg.AccessKeyID = "good-key"
	cfg.AccessKeyIDFile = filepath.Join(t.TempDir(), "does-not-exist")
	d := New(cfg, nil)

	d.reloadCredentials(discardLogger())

	if got := d.effectiveCredentials().AccessKeyID; got != "good-key" {
		t.Errorf("effectiveCredentials().AccessKeyID after a failed reload = %q, want %q (last-known-good)", got, "good-key")
	}
}

// TestDetectConcurrentWithCredentialSwapNoRace is the -race proof: many
// concurrent Detect calls against a real httptest.Server while a
// separate goroutine repeatedly rotates the credential file.
func TestDetectConcurrentWithCredentialSwapNoRace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access-key-id")
	if err := os.WriteFile(path, []byte("key-0"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(applyGuardrailResponse{Action: "NONE"})
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.AccessKeyIDFile = path
	d := New(cfg, srv.Client())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.RunCredentialReloadLoop(ctx, time.Millisecond, discardLogger())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			_ = os.WriteFile(path, []byte("key-"+string(rune('0'+i%10))), 0o600)
			time.Sleep(time.Millisecond)
		}
	}()

	for i := 0; i < 20; i++ {
		if _, err := d.Detect(context.Background(), "hello"); err != nil {
			t.Fatalf("Detect: %v", err)
		}
	}
	<-done
}
