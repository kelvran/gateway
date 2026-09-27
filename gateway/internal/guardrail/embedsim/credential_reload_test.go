package embedsim

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEffectiveCredentialsReflectsInitialConfig proves a BedrockEmbedder
// with no *File fields configured just reflects its own
// construction-time Config values -- the exact behavior before this
// feature existed. Mirrors bedrockguard's own identically-named test.
func TestEffectiveCredentialsReflectsInitialConfig(t *testing.T) {
	e := NewBedrockEmbedder(testEmbedderConfig(""), nil)
	got := e.effectiveCredentials()
	if got.AccessKeyID != "test-access-key" || got.SecretAccessKey != "test-secret-key" {
		t.Errorf("effectiveCredentials() = %+v, want AccessKeyID=test-access-key SecretAccessKey=test-secret-key", got)
	}
}

// TestRunCredentialReloadLoopPicksUpFileRotationWithinOneInterval is the
// embedsim-package mirror of bedrockguard's own identically-named test.
func TestRunCredentialReloadLoopPicksUpFileRotationWithinOneInterval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access-key-id")
	if err := os.WriteFile(path, []byte("initial-key"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := testEmbedderConfig("")
	cfg.AccessKeyIDFile = path
	e := NewBedrockEmbedder(cfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const reloadInterval = 10 * time.Millisecond
	go e.RunCredentialReloadLoop(ctx, reloadInterval, discardLogger())

	if err := os.WriteFile(path, []byte("rotated-key"), 0o600); err != nil {
		t.Fatalf("WriteFile (rotation): %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := e.effectiveCredentials().AccessKeyID; got == "rotated-key" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("effectiveCredentials().AccessKeyID = %q after 2s, want %q (rotation was never picked up)", e.effectiveCredentials().AccessKeyID, "rotated-key")
		}
		time.Sleep(reloadInterval)
	}
}

// TestRunCredentialReloadLoopNoOpForEnvOnlyEmbedder mirrors
// bedrockguard's own TestRunCredentialReloadLoopNoOpForEnvOnlyDetector.
func TestRunCredentialReloadLoopNoOpForEnvOnlyEmbedder(t *testing.T) {
	e := NewBedrockEmbedder(testEmbedderConfig(""), nil)
	e.RunCredentialReloadLoop(context.Background(), 10*time.Millisecond, discardLogger())
}

// TestRunCredentialReloadLoopZeroIntervalIsNoOp mirrors bedrockguard's
// own identically-named test.
func TestRunCredentialReloadLoopZeroIntervalIsNoOp(t *testing.T) {
	cfg := testEmbedderConfig("")
	cfg.AccessKeyIDFile = filepath.Join(t.TempDir(), "access-key-id")
	e := NewBedrockEmbedder(cfg, nil)
	e.RunCredentialReloadLoop(context.Background(), 0, discardLogger())
}

// TestReloadCredentialsKeepsLastKnownGoodOnReadError mirrors
// bedrockguard's own identically-named test.
func TestReloadCredentialsKeepsLastKnownGoodOnReadError(t *testing.T) {
	cfg := testEmbedderConfig("")
	cfg.AccessKeyID = "good-key"
	cfg.AccessKeyIDFile = filepath.Join(t.TempDir(), "does-not-exist")
	e := NewBedrockEmbedder(cfg, nil)

	e.reloadCredentials(discardLogger())

	if got := e.effectiveCredentials().AccessKeyID; got != "good-key" {
		t.Errorf("effectiveCredentials().AccessKeyID after a failed reload = %q, want %q (last-known-good)", got, "good-key")
	}
}

// TestEmbedConcurrentWithCredentialSwapNoRace is the -race proof, mirroring
// bedrockguard's own TestDetectConcurrentWithCredentialSwapNoRace.
func TestEmbedConcurrentWithCredentialSwapNoRace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access-key-id")
	if err := os.WriteFile(path, []byte("key-0"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(invokeModelResponse{Embedding: []float32{0.1, 0.2}})
	}))
	defer srv.Close()

	cfg := testEmbedderConfig(srv.URL)
	cfg.AccessKeyIDFile = path
	e := NewBedrockEmbedder(cfg, srv.Client())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.RunCredentialReloadLoop(ctx, time.Millisecond, discardLogger())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			_ = os.WriteFile(path, []byte("key-"+string(rune('0'+i%10))), 0o600)
			time.Sleep(time.Millisecond)
		}
	}()

	for i := 0; i < 20; i++ {
		if _, err := e.Embed(context.Background(), "hello"); err != nil {
			t.Fatalf("Embed: %v", err)
		}
	}
	<-done
}

// TestDetectorRunCredentialReloadLoopNoOpWhenEmbedderDoesNotSupportReload
// proves Detector's own delegating RunCredentialReloadLoop is a safe
// no-op against wordHashEmbedder (this package's own test fake), which
// never implements the method -- the exact shape a real, non-Bedrock
// Embedder implementation would have.
func TestDetectorRunCredentialReloadLoopNoOpWhenEmbedderDoesNotSupportReload(t *testing.T) {
	d := testDetector(t, 0.8)
	d.RunCredentialReloadLoop(context.Background(), 10*time.Millisecond, discardLogger())
}

// TestDetectorRunCredentialReloadLoopDelegatesToBedrockEmbedder proves
// Detector's delegation reaches a real, file-backed *BedrockEmbedder --
// the actual path cmd/gateway's run() drives via the shared
// credentialReloader type assertion over guardrail.Engine.Detectors().
func TestDetectorRunCredentialReloadLoopDelegatesToBedrockEmbedder(t *testing.T) {
	// New eagerly embeds the whole corpus at construction time (one real
	// Embed call per entry, per New's own doc comment) -- point the
	// embedder at a fake server instead of real AWS so construction
	// succeeds without a live credential or network access.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(invokeModelResponse{Embedding: []float32{0.1, 0.2}})
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "access-key-id")
	if err := os.WriteFile(path, []byte("initial-key"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := testEmbedderConfig(srv.URL)
	cfg.AccessKeyIDFile = path
	embedder := NewBedrockEmbedder(cfg, srv.Client())

	d, err := New(Config{SimilarityThreshold: 0.8}, embedder, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const reloadInterval = 10 * time.Millisecond
	go d.RunCredentialReloadLoop(ctx, reloadInterval, discardLogger())

	if err := os.WriteFile(path, []byte("rotated-key"), 0o600); err != nil {
		t.Fatalf("WriteFile (rotation): %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := embedder.effectiveCredentials().AccessKeyID; got == "rotated-key" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("embedder.effectiveCredentials().AccessKeyID = %q after 2s, want %q (Detector's delegation never reached the embedder)", embedder.effectiveCredentials().AccessKeyID, "rotated-key")
		}
		time.Sleep(reloadInterval)
	}
}
