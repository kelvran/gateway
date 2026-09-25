package dataplane

// Tests for credential_reload.go's file-based credential hot-reload
// mechanism -- see that file's own doc comment for the full design
// rationale. Three things this file must prove, per the feature's own
// acceptance criteria:
//
//  1. A deployment configured with a file-based credential source picks
//     up a REAL file content change within one reload interval (a short
//     test interval, never the production default).
//  2. A deployment using only the pre-existing *Env convention is
//     completely unaffected by this feature existing at all.
//  3. Concurrent reads (the real upstream-auth hot path,
//     setUpstreamAuthHeaders) and a credential swap never race --
//     verified by running this file under `go test -race`.
//
// Every credential-shaped value below goes through testCred
// (bedrock_auth_test.go) rather than a literal string, for the same
// reason that file's own doc comment gives: computed, not a literal, so
// nothing resembling a real secret ever appears as source text.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// unusedUpstream is an UpstreamCaller that's never actually expected to
// be called -- every test in this file that needs a *Pipeline is only
// exercising RunCredentialReloadLoop/effectiveCredentials, never a real
// request through HandleChatCompletion, so NewPipeline's own "Config.
// Upstream is required" validation just needs a non-nil placeholder.
func unusedUpstream(_ context.Context, _ Deployment, _ any) (any, error) {
	return nil, errors.New("unusedUpstream: not expected to be called by this test")
}

// TestDeploymentCredentialFilesHasAny covers every field independently,
// plus the all-empty zero value -- HasAny is the single gate both
// cmd/gateway's buildPipeline and RunCredentialReloadLoop use to decide
// whether a deployment opted into this feature at all.
func TestDeploymentCredentialFilesHasAny(t *testing.T) {
	placeholderPath := filepath.Join(t.TempDir(), "placeholder")
	cases := []struct {
		name  string
		files DeploymentCredentialFiles
		want  bool
	}{
		{"zero value", DeploymentCredentialFiles{}, false},
		{"api key only", DeploymentCredentialFiles{APIKey: placeholderPath}, true},
		{"access key id only", DeploymentCredentialFiles{AccessKeyID: placeholderPath}, true},
		{"secret access key only", DeploymentCredentialFiles{SecretAccessKey: placeholderPath}, true},
		{"session token only", DeploymentCredentialFiles{SessionToken: placeholderPath}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.files.HasAny(); got != tc.want {
				t.Errorf("HasAny() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReadCredentialFileTrimsWhitespaceAndErrorsOnMissingFile proves
// both halves of ReadCredentialFile's contract: a Kubernetes projected
// Secret volume's typical trailing-newline file reads back as the bare
// credential value, and a missing/unreadable path surfaces a real
// error rather than silently returning "".
func TestReadCredentialFileTrimsWhitespaceAndErrorsOnMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api-key")
	missingPath := filepath.Join(dir, "does-not-exist")
	fileValue := testCred("file-value")
	if err := os.WriteFile(path, []byte("  "+fileValue+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ReadCredentialFile(path)
	if err != nil {
		t.Fatalf("ReadCredentialFile: %v", err)
	}
	if got != fileValue {
		t.Errorf("ReadCredentialFile = %q, want the trimmed value %q with no surrounding whitespace", got, fileValue)
	}

	if _, err := ReadCredentialFile(missingPath); err == nil {
		t.Error("ReadCredentialFile on a missing path returned nil error, want a real error")
	}
}

// TestEffectiveCredentialsFallsBackToPlainFieldsWhenNoCredentialState
// is the decisive backward-compatibility proof: a Deployment built
// exactly as every existing call site (main.go, bedrock_auth_test.go)
// already constructs one -- plain APIKey/AccessKeyID/SecretAccessKey/
// SessionToken fields, CredentialState left at its zero value (nil) --
// keeps working through effectiveCredentials with zero behavior
// change.
func TestEffectiveCredentialsFallsBackToPlainFieldsWhenNoCredentialState(t *testing.T) {
	dep := Deployment{
		Name:            "d",
		Provider:        "bedrock",
		AccessKeyID:     testCred("access-key"),
		SecretAccessKey: testCred("access-value"),
		SessionToken:    testCred("session"),
	}
	if dep.CredentialState != nil {
		t.Fatal("a Deployment built via a plain struct literal has a non-nil CredentialState, want nil")
	}
	got := dep.effectiveCredentials()
	want := DeploymentCredentials{
		AccessKeyID:     dep.AccessKeyID,
		SecretAccessKey: dep.SecretAccessKey,
		SessionToken:    dep.SessionToken,
	}
	if got != want {
		t.Errorf("effectiveCredentials() = %+v, want %+v (dep's own plain fields, CredentialState is nil)", got, want)
	}
}

// TestEffectiveCredentialsPrefersCredentialStateOverPlainFields proves
// CredentialState, once populated, is authoritative -- the plain fields
// are only ever the seed value, never re-consulted once a hot-reloadable
// snapshot exists (they'd otherwise go stale the moment a real rotation
// happened).
func TestEffectiveCredentialsPrefersCredentialStateOverPlainFields(t *testing.T) {
	stale := testCred("stale-plain-field")
	current := testCred("current-hot-reloaded")
	dep := Deployment{
		Name:            "d",
		Provider:        "openai",
		APIKey:          stale,
		CredentialState: NewDeploymentCredentialState(DeploymentCredentials{APIKey: current}),
	}
	got := dep.effectiveCredentials()
	if got.APIKey != current {
		t.Errorf("effectiveCredentials().APIKey = %q, want the CredentialState value %q, not the stale plain field", got.APIKey, current)
	}
}

// TestCredentialStateSharedAcrossDeploymentValueCopies is the load-
// bearing proof for this whole feature's thread-safety design:
// Deployment is copied BY VALUE throughout this package (nextDeployment,
// ProbeDeployments, RunCredentialReloadLoop's own reloadable slice,
// etc.) -- a rotation published on ONE copy's CredentialState must be
// visible through every OTHER copy too, since they all share the same
// underlying *atomic.Pointer.
func TestCredentialStateSharedAcrossDeploymentValueCopies(t *testing.T) {
	before := testCred("before-rotation")
	after := testCred("after-rotation")
	original := Deployment{
		Name:            "d",
		Provider:        "openai",
		CredentialState: NewDeploymentCredentialState(DeploymentCredentials{APIKey: before}),
	}
	// copy1/copy2 are independent Go value copies -- exactly what
	// nextDeployment/ProbeDeployments/etc already produce on every call.
	copy1 := original
	copy2 := original

	original.CredentialState.Store(&DeploymentCredentials{APIKey: after})

	if got := copy1.effectiveCredentials().APIKey; got != after {
		t.Errorf("copy1.effectiveCredentials().APIKey = %q, want the rotated value %q visible through the shared pointer", got, after)
	}
	if got := copy2.effectiveCredentials().APIKey; got != after {
		t.Errorf("copy2.effectiveCredentials().APIKey = %q, want the rotated value %q visible through the shared pointer", got, after)
	}
}

// TestSetUpstreamAuthHeadersReflectsRotatedBedrockCredential drives the
// REAL hot-path call site (setUpstreamAuthHeaders, via the SigV4
// signer) against a CredentialState that gets swapped between two
// calls -- proving the rotation actually reaches the Authorization
// header a real upstream call would send, not just effectiveCredentials
// in isolation.
func TestSetUpstreamAuthHeadersReflectsRotatedBedrockCredential(t *testing.T) {
	beforeAccess := testCred("access-key-before")
	afterAccess := testCred("access-key-after")

	state := NewDeploymentCredentialState(DeploymentCredentials{
		AccessKeyID:     beforeAccess,
		SecretAccessKey: testCred("secret-before"),
	})
	dep := Deployment{
		Name:            "bedrock-primary",
		Provider:        "bedrock",
		Region:          "us-east-1",
		CredentialState: state,
	}
	body := []byte(`{}`)

	req1, err := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := setUpstreamAuthHeaders(context.Background(), req1, dep, nil, body); err != nil {
		t.Fatalf("setUpstreamAuthHeaders (before rotation): %v", err)
	}
	if auth := req1.Header.Get("Authorization"); !strings.Contains(auth, beforeAccess) {
		t.Fatalf("Authorization (before rotation) = %q, want it to reference %q", auth, beforeAccess)
	}

	// Simulate RunCredentialReloadLoop picking up a rotation.
	state.Store(&DeploymentCredentials{
		AccessKeyID:     afterAccess,
		SecretAccessKey: testCred("secret-after"),
	})

	req2, err := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := setUpstreamAuthHeaders(context.Background(), req2, dep, nil, body); err != nil {
		t.Fatalf("setUpstreamAuthHeaders (after rotation): %v", err)
	}
	auth := req2.Header.Get("Authorization")
	if !strings.Contains(auth, afterAccess) {
		t.Errorf("Authorization (after rotation) = %q, want it to reference the NEW access key %q", auth, afterAccess)
	}
	if strings.Contains(auth, beforeAccess) {
		t.Errorf("Authorization (after rotation) = %q, still references the OLD access key %q", auth, beforeAccess)
	}
}

// TestRunCredentialReloadLoopPicksUpFileRotationWithinOneInterval is
// the end-to-end proof this feature exists to satisfy: a deployment
// configured with a file-based credential source picks up a REAL file
// content change within one reload interval. Uses a short interval
// (milliseconds), never DefaultCredentialReloadInterval.
func TestRunCredentialReloadLoopPicksUpFileRotationWithinOneInterval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api-key")
	initial := testCred("initial")
	rotated := testCred("rotated")
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	state := NewDeploymentCredentialState(DeploymentCredentials{APIKey: initial})
	dep := Deployment{
		Name:            "file-backed",
		Model:           "m",
		Provider:        "openai",
		UpstreamModel:   "m",
		BaseURL:         "http://unused",
		CredentialFiles: DeploymentCredentialFiles{APIKey: path},
		CredentialState: state,
	}

	p := newTestPipeline(t, unusedUpstream, []Deployment{dep})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const reloadInterval = 10 * time.Millisecond
	go p.RunCredentialReloadLoop(ctx, reloadInterval)

	if err := os.WriteFile(path, []byte(rotated), 0o600); err != nil {
		t.Fatalf("WriteFile (rotation): %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := state.Load().APIKey; got == rotated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("CredentialState.Load().APIKey = %q after 2s, want %q (rotation was never picked up)", state.Load().APIKey, rotated)
		}
		time.Sleep(reloadInterval)
	}
}

// TestRunCredentialReloadLoopNoOpForEnvOnlyDeployment proves the
// feature's opt-in guarantee: a deployment configured entirely via the
// pre-existing *Env convention (no CredentialFiles, CredentialState
// nil) is completely unaffected by RunCredentialReloadLoop -- both
// because there is nothing for it to reload, and because calling it at
// all must never panic on a nil CredentialState.
func TestRunCredentialReloadLoopNoOpForEnvOnlyDeployment(t *testing.T) {
	envResolved := testCred("env-resolved")
	dep := Deployment{
		Name:          "env-only",
		Model:         "m",
		Provider:      "openai",
		UpstreamModel: "m",
		BaseURL:       "http://unused",
		APIKey:        envResolved,
	}
	p := newTestPipeline(t, unusedUpstream, []Deployment{dep})

	ctx, cancel := context.WithCancel(context.Background())
	// RunCredentialReloadLoop must return almost immediately here --
	// zero deployments opted in, so it never even starts a ticker. If
	// it incorrectly blocked on ctx instead, this test would hang until
	// the surrounding go test timeout, which is exactly what this
	// proves does NOT happen.
	done := make(chan struct{})
	go func() {
		p.RunCredentialReloadLoop(ctx, time.Hour)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunCredentialReloadLoop did not return promptly for an all-*Env deployment set, want an immediate no-op")
	}
	cancel()

	if dep.CredentialState != nil {
		t.Error("env-only Deployment has a non-nil CredentialState, want nil -- this feature must never touch it")
	}
	if got := dep.effectiveCredentials().APIKey; got != envResolved {
		t.Errorf("effectiveCredentials().APIKey = %q, want the original env-resolved value untouched", got)
	}
}

// TestRunCredentialReloadLoopZeroIntervalIsNoOp proves the same
// contract RunHealthProbeLoop already established: interval <= 0
// returns immediately, never starts a ticker.
func TestRunCredentialReloadLoopZeroIntervalIsNoOp(t *testing.T) {
	p := newTestPipeline(t, unusedUpstream, []Deployment{{Name: "d", Model: "m", Provider: "openai", UpstreamModel: "m", BaseURL: "http://unused"}})

	done := make(chan struct{})
	go func() {
		p.RunCredentialReloadLoop(context.Background(), 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunCredentialReloadLoop(ctx, 0) did not return promptly, want an immediate no-op")
	}
}

// TestReloadDeploymentCredentialsKeepsLastKnownGoodOnReadError proves
// the degrade-safely contract: a read failure on one field never wipes
// out that field's last-known-good value, and never touches the other,
// still-healthy fields either.
func TestReloadDeploymentCredentialsKeepsLastKnownGoodOnReadError(t *testing.T) {
	goodValue := testCred("last-known-good")
	untouchedValue := testCred("unrelated-field")
	missingPath := filepath.Join(t.TempDir(), "does-not-exist")
	state := NewDeploymentCredentialState(DeploymentCredentials{
		APIKey:      goodValue,
		AccessKeyID: untouchedValue,
	})
	dep := Deployment{
		Name:            "d",
		CredentialFiles: DeploymentCredentialFiles{APIKey: missingPath},
		CredentialState: state,
	}

	reloadDeploymentCredentials(dep, discardLogger())

	got := state.Load()
	if got.APIKey != goodValue {
		t.Errorf("APIKey = %q after a read error, want the last-known-good value %q preserved", got.APIKey, goodValue)
	}
	if got.AccessKeyID != untouchedValue {
		t.Errorf("AccessKeyID = %q, want it untouched (%q) by an APIKey-only read failure", got.AccessKeyID, untouchedValue)
	}
}

// TestSetUpstreamAuthHeadersConcurrentReadsDuringCredentialSwapNoRace
// is this feature's required `-race` proof: many goroutines calling the
// real hot-path function (setUpstreamAuthHeaders) concurrently with a
// separate goroutine repeatedly swapping CredentialState, exactly the
// concurrency shape RunCredentialReloadLoop (background goroutine) vs.
// real request-serving goroutines produces in production. Each
// goroutine builds its OWN *http.Request per call -- sharing one
// http.Request/http.Header across goroutines would race on the Header
// map itself, which is unrelated to (and would falsely implicate) the
// credential-swap mechanism this test actually targets.
func TestSetUpstreamAuthHeadersConcurrentReadsDuringCredentialSwapNoRace(t *testing.T) {
	state := NewDeploymentCredentialState(DeploymentCredentials{
		AccessKeyID:     testCred("access-key-0"),
		SecretAccessKey: testCred("secret-0"),
	})
	dep := Deployment{
		Name:            "bedrock-primary",
		Provider:        "bedrock",
		Region:          "us-east-1",
		CredentialState: state,
	}
	body := []byte(`{}`)

	const readers = 20
	const iterationsPerReader = 100

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			state.Store(&DeploymentCredentials{
				AccessKeyID:     testCred("access-key-rotated"),
				SecretAccessKey: testCred("secret-rotated"),
			})
		}
	}()

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterationsPerReader; i++ {
				req, err := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse", nil)
				if err != nil {
					t.Errorf("NewRequest: %v", err)
					return
				}
				if err := setUpstreamAuthHeaders(context.Background(), req, dep, nil, body); err != nil {
					t.Errorf("setUpstreamAuthHeaders: %v", err)
					return
				}
				if req.Header.Get("Authorization") == "" {
					t.Error("Authorization header is empty after a concurrent setUpstreamAuthHeaders call")
					return
				}
			}
		}()
	}

	// Let the readers run their full course, then stop the writer.
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}
