package main

import (
	"bytes"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/backup"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/spendledger"
	"github.com/kelvran/gateway/gateway/internal/spendledger/redisledger"
)

// TestNewLedgerPicksTheBackendByConfig: no persist_path and no
// budget.redis_addr → the in-memory ledger; persist_path → the bbolt file
// (a backup candidate); budget.redis_addr → the Redis backend regardless of
// persist_path, with the same both-set warning the budget logs; the Redis
// client never dials at Open.
func TestNewLedgerPicksTheBackendByConfig(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	ledgerCfg := controlplane.LedgerConfig{RetentionHours: 168, MaxSessionsPerKey: 2000, MaxRowsPerKey: 50000}

	mem, err := newLedger(ledgerCfg, controlplane.BudgetConfig{}, "fail", logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.(*spendledger.Memory); !ok {
		t.Fatalf("no persist_path, no redis_addr: %T, want *spendledger.Memory", mem)
	}

	fileCfg := ledgerCfg
	fileCfg.PersistPath = filepath.Join(t.TempDir(), "ledger.db")
	file, err := newLedger(fileCfg, controlplane.BudgetConfig{}, "fail", logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, ok := file.(backup.DBBacked); !ok {
		t.Fatalf("persist_path: %T, want a bbolt-backed store (backup.DBBacked)", file)
	}

	logs.Reset()
	rds, err := newLedger(fileCfg, controlplane.BudgetConfig{RedisAddr: "127.0.0.1:1"}, "fail", logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rds.Close() }()
	if _, ok := rds.(*redisledger.Store); !ok {
		t.Fatalf("budget.redis_addr set: %T, want *redisledger.Store", rds)
	}
	if !strings.Contains(logs.String(), "ledger_redis_addr_and_persist_path_both_set") {
		t.Fatalf("both set: want the warning line, got %q", logs.String())
	}
}

// TestRestorablePersistPathKnowsTheLedger: the offline restore flag accepts
// `ledger` and resolves ledger.persist_path, like the other three stores.
func TestRestorablePersistPathKnowsTheLedger(t *testing.T) {
	cfg := &controlplane.Config{}
	cfg.Ledger.PersistPath = "/var/lib/kelvran-gateway/ledger.db"
	path, err := restorablePersistPath(cfg, "ledger")
	if err != nil || path != cfg.Ledger.PersistPath {
		t.Fatalf("restorablePersistPath(ledger) = %q, %v", path, err)
	}
	if _, err := restorablePersistPath(&controlplane.Config{}, "ledger"); err == nil || !strings.Contains(err.Error(), "no persist_path") {
		t.Fatalf("restorablePersistPath(ledger) without a path: %v, want the no-persist_path error", err)
	}
	if _, err := restorablePersistPath(cfg, "nope"); err == nil || !strings.Contains(err.Error(), "ledger") {
		t.Fatalf("the unknown-store error must list ledger: %v", err)
	}
}

// TestBuildPipelineWiresTheLedger: a config with ledger.persist_path gives
// the pipeline a bbolt-backed ledger — proven through the backup candidate
// list, the one observable the pipeline exposes before the admin routes land.
func TestBuildPipelineWiresTheLedger(t *testing.T) {
	t.Setenv("KELVRAN_TEST_UPSTREAM_KEY", "fake-upstream-key-not-a-real-secret")
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "k1", KeyHash: strings.Repeat("ab", 32), RateLimitBurst: 10, RateLimitRefill: 10},
		},
		Deployments: []controlplane.DeploymentConfig{
			{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://127.0.0.1:1", APIKeyEnv: "KELVRAN_TEST_UPSTREAM_KEY"},
		},
		Ledger: controlplane.LedgerConfig{PersistPath: filepath.Join(t.TempDir(), "ledger.db"), RetentionHours: 168, MaxSessionsPerKey: 2000, MaxRowsPerKey: 50000},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}
	defer func() { _ = pipeline.Close() }()
	files, err := pipeline.BackupStores(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var ledgerFiles []string
	for _, f := range files {
		if strings.HasPrefix(f, "ledger-") {
			ledgerFiles = append(ledgerFiles, f)
		}
	}
	if len(ledgerFiles) != 1 {
		t.Fatalf("BackupStores = %v, want one ledger-<stamp>.bbolt: the pipeline was built without the ledger", files)
	}
}
