package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/identity"
	identityboltstore "github.com/kelvran/gateway/gateway/internal/identity/boltstore"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// TestMergePersistedVirtualKeysLogsOneLinePerOverriddenID proves the real
// disclosure gap this phase closes: before this change,
// mergePersistedVirtualKeys silently overwrote a config-declared entry
// with the persisted one — no logger parameter existed at all, so an
// operator debugging "why isn't my config.yaml edit taking effect" had no
// way to learn a persisted store was the real, authoritative source.
func TestMergePersistedVirtualKeysLogsOneLinePerOverriddenID(t *testing.T) {
	store, err := identityboltstore.Open(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatalf("identityboltstore.Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := store.Save(context.Background(), identity.VirtualKey{ID: "overridden-key", KeyHash: testKeyHash("persisted-value")}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	configDeclared := []identity.VirtualKey{{ID: "overridden-key", KeyHash: testKeyHash("stale-config-value")}}
	keyConfigs := []ratelimit.KeyConfig{{ID: "overridden-key"}}
	concurrencyConfigs := []ratelimit.ConcurrencyConfig{{ID: "overridden-key"}}

	if _, _, _, err := mergePersistedVirtualKeys(configDeclared, keyConfigs, concurrencyConfigs, store, logger); err != nil {
		t.Fatalf("mergePersistedVirtualKeys: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "startup_virtual_key_overridden_by_persisted_store") || !strings.Contains(got, "overridden-key") {
		t.Errorf("log output = %q, want a line naming startup_virtual_key_overridden_by_persisted_store and key_id=overridden-key", got)
	}
}

// TestMergePersistedVirtualKeysLogsNothingWhenNoOverrideOccurs is the
// regression guard: a persisted key with no config-declared counterpart
// at all (the net-new-entry branch) is not an override of anything, and
// must not log as if it were one.
func TestMergePersistedVirtualKeysLogsNothingWhenNoOverrideOccurs(t *testing.T) {
	store, err := identityboltstore.Open(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatalf("identityboltstore.Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := store.Save(context.Background(), identity.VirtualKey{ID: "net-new-key", KeyHash: testKeyHash("net-new-key")}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	if _, _, _, err := mergePersistedVirtualKeys(nil, nil, nil, store, logger); err != nil {
		t.Fatalf("mergePersistedVirtualKeys: %v", err)
	}

	if got := buf.String(); got != "" {
		t.Errorf("log output = %q, want empty — a net-new persisted key overrides nothing", got)
	}
}
