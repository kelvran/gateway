package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/adminapi"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/router"
	"github.com/kelvran/gateway/gateway/internal/spendledger"
)

// newLedgerSpendTestPipeline is newTestPipeline with two virtual keys and a
// pre-filled in-memory spend ledger (item 13d slice 4): rows for test-key at
// now-30 min and now-30 h, and for other-key at now-30 min, plus a row 200 h
// old that only an unclamped window could reach.
func newLedgerSpendTestPipeline(t *testing.T, now time.Time) (*dataplane.Pipeline, *spendledger.Memory) {
	t.Helper()
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
		{ID: "other-key", KeyHash: testHashOf("other-key"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	ledger := spendledger.NewMemory(spendledger.Options{})
	ctx := context.Background()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ledger.Record(ctx, spendledger.Entry{Key: "test-key", Model: "gpt-4o", Tool: "curl", Session: "s1", At: now.Add(-30 * time.Minute), SpendNanoUSD: 1_500_000_000, Requests: 3}))
	must(ledger.Record(ctx, spendledger.Entry{Key: "test-key", Model: "claude", Tool: "claude_code", Session: "s2", At: now.Add(-30 * time.Hour), SpendNanoUSD: 250_000_000, Requests: 1}))
	must(ledger.Record(ctx, spendledger.Entry{Key: "other-key", Model: "gpt-4o", Tool: "curl", Session: "s1", At: now.Add(-30 * time.Minute), SpendNanoUSD: 500_000_000, Requests: 2}))
	must(ledger.Record(ctx, spendledger.Entry{Key: "test-key", Model: "gpt-4o", Tool: "curl", Session: "ancient", At: now.Add(-200 * time.Hour), SpendNanoUSD: 9_000_000_000, Requests: 9}))
	deployments := []dataplane.Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := dataplane.NewPipeline(dataplane.Config{
		Verifier: verifier,
		Limiter: ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{
			{ID: "test-key", Capacity: 100, RefillPerSecond: 100},
			{ID: "other-key", Capacity: 100, RefillPerSecond: 100},
		}),
		Budget:         budget.NewTracker(),
		Ledger:         ledger,
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         router.New([]router.Deployment{{Name: "d1", Model: "gpt-4o"}}, router.HealthConfig{}),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep dataplane.Deployment, req any) (any, error) {
			return &openai.Response{ID: "chatcmpl-fake", Model: dep.UpstreamModel}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p, ledger
}

func spendHandler(t *testing.T, p *dataplane.Pipeline) http.Handler {
	t.Helper()
	cfg := testConfig()
	cfg.Ledger.RetentionHours = 168
	return Handler(cfg, p, Credentials{Admin: fakeAdminCredential(), Viewer: fakeViewerCredential(), CostViewer: fakeCostViewerCredential(), Operator: fakeOperatorCredential()}, discardLogger(), nil)
}

func decodeSpend(t *testing.T, body []byte) adminapi.SpendResponse {
	t.Helper()
	var resp adminapi.SpendResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decoding spend response: %v\n%s", err, body)
	}
	return resp
}

func describeSpend(rows []adminapi.SpendRow) string {
	var parts []string
	for _, r := range rows {
		parts = append(parts, r.Key+"/"+r.Model+"/"+r.Tool+"/"+r.Session+"="+r.SpendUSD+":"+strconv.FormatInt(r.Requests, 10))
	}
	return strings.Join(parts, " ")
}

// TestGetSpendAggregatesByEveryDimensionWithinTheDefaultWindow: Admin and
// Viewer read any query; the default window is the last 24 h, so the 30 h
// old row is absent; rows come sorted by spend, highest first, `spend_usd`
// a decimal string, `requests` an integer; a key= narrows to that key.
func TestGetSpendAggregatesByEveryDimensionWithinTheDefaultWindow(t *testing.T) {
	now := time.Now()
	p, _ := newLedgerSpendTestPipeline(t, now)
	h := spendHandler(t, p)
	for _, cred := range []string{fakeAdminCredential(), fakeViewerCredential()} {
		rec := doRequest(t, h, http.MethodGet, "/admin/spend?group_by=key", cred, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /admin/spend?group_by=key: status = %d, body %s", rec.Code, rec.Body.String())
		}
		resp := decodeSpend(t, rec.Body.Bytes())
		if got := describeSpend(resp.Rows); got != "test-key///=1.5:3 other-key///=0.5:2" {
			t.Fatalf("rows by key = %q", got)
		}
		if resp.GroupBy != "key" || resp.Key != "" || resp.Until.Sub(resp.Since) != 24*time.Hour || now.Sub(resp.Until) > time.Minute {
			t.Fatalf("window = %+v, want group_by key, the last 24 h ending now", resp)
		}
	}
	cases := map[string]string{
		"model":   "/gpt-4o//=2:5",
		"tool":    "//curl/=2:5",
		"session": "test-key///s1=1.5:3 other-key///s1=0.5:2",
	}
	for groupBy, want := range cases {
		rec := doRequest(t, h, http.MethodGet, "/admin/spend?group_by="+groupBy, fakeAdminCredential(), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("group_by=%s: status = %d, body %s", groupBy, rec.Code, rec.Body.String())
		}
		if got := describeSpend(decodeSpend(t, rec.Body.Bytes()).Rows); got != want {
			t.Fatalf("rows by %s = %q, want %q", groupBy, got, want)
		}
	}
	rec := doRequest(t, h, http.MethodGet, "/admin/spend?group_by=model&key=other-key", fakeAdminCredential(), "")
	resp := decodeSpend(t, rec.Body.Bytes())
	if got := describeSpend(resp.Rows); got != "other-key/gpt-4o//=0.5:2" || resp.Key != "other-key" {
		t.Fatalf("key= narrowing: rows = %q, key = %q", got, resp.Key)
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/spend?group_by=key&key=nobody", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"rows":[]`) {
		t.Fatalf("an unknown key is an empty row list, not an error: %d %s", rec.Code, rec.Body.String())
	}
}

// TestGetSpendWindowParsesClampsAndRefuses: since/until are RFC 3339; a
// window reaching back past the retention is clamped to it (so the 200 h old
// row never appears while the 30 h old one does); until must follow since.
func TestGetSpendWindowParsesClampsAndRefuses(t *testing.T) {
	now := time.Now()
	p, _ := newLedgerSpendTestPipeline(t, now)
	h := spendHandler(t, p)
	since := now.Add(-400 * time.Hour).UTC().Format(time.RFC3339)
	until := now.UTC().Format(time.RFC3339)
	rec := doRequest(t, h, http.MethodGet, "/admin/spend?group_by=session&key=test-key&since="+since+"&until="+until, fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("wide window: status = %d, body %s", rec.Code, rec.Body.String())
	}
	resp := decodeSpend(t, rec.Body.Bytes())
	if got := describeSpend(resp.Rows); got != "test-key///s1=1.5:3 test-key///s2=0.25:1" {
		t.Fatalf("clamped window rows = %q (the 200 h old row must be outside the retention)", got)
	}
	if floor := now.Add(-168 * time.Hour); resp.Since.Sub(floor).Abs() > time.Minute {
		t.Fatalf("since was not clamped to now - retention: %v (floor %v)", resp.Since, floor)
	}
	// A window that lies entirely before the floor is a valid question with an
	// empty answer: served as [floor, floor), never inverted, never an error.
	past := "group_by=key&since=" + now.Add(-400*time.Hour).UTC().Format(time.RFC3339) + "&until=" + now.Add(-300*time.Hour).UTC().Format(time.RFC3339)
	rec = doRequest(t, h, http.MethodGet, "/admin/spend?"+past, fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a window before the floor: status = %d, body %s", rec.Code, rec.Body.String())
	}
	resp = decodeSpend(t, rec.Body.Bytes())
	if len(resp.Rows) != 0 || !resp.Since.Equal(resp.Until) || resp.Since.Sub(now.Add(-168*time.Hour)).Abs() > time.Minute {
		t.Fatalf("a window before the floor must collapse to [floor, floor) with no rows: %+v", resp)
	}
	for name, query := range map[string]string{
		"bad since":          "group_by=key&since=yesterday",
		"bad until":          "group_by=key&until=2026-13-40",
		"until before since": "group_by=key&since=2026-10-11T10:00:00Z&until=2026-10-11T09:00:00Z",
		"until equals since": "group_by=key&since=2026-10-11T10:00:00Z&until=2026-10-11T10:00:00Z",
		"missing group_by":   "key=test-key",
		"unknown group_by":   "group_by=hour",
		"a since without tz": "group_by=key&since=2026-10-11T10:00:00",
	} {
		rec := doRequest(t, h, http.MethodGet, "/admin/spend?"+query, fakeAdminCredential(), "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body %s", name, rec.Code, rec.Body.String())
		}
	}
}

// TestCostViewerReadsSpendOnlyWithAKey: G27 — the cost-viewer tier may call
// GET /admin/spend only when key= names one key, across every group_by;
// without key= it is 401 like any insufficient tier; Operator never reads.
func TestCostViewerReadsSpendOnlyWithAKey(t *testing.T) {
	p, _ := newLedgerSpendTestPipeline(t, time.Now())
	h := spendHandler(t, p)
	rec := doRequest(t, h, http.MethodGet, "/admin/spend?group_by=key", fakeCostViewerCredential(), "")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "key=") {
		t.Fatalf("cost viewer without key=: status = %d, body %q; want 401 naming key=", rec.Code, rec.Body.String())
	}
	for _, groupBy := range []string{"key", "model", "tool", "session"} {
		rec := doRequest(t, h, http.MethodGet, "/admin/spend?group_by="+groupBy+"&key=test-key", fakeCostViewerCredential(), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("cost viewer with key=, group_by=%s: status = %d, body %s", groupBy, rec.Code, rec.Body.String())
		}
		for _, row := range decodeSpend(t, rec.Body.Bytes()).Rows {
			if row.Key != "test-key" && row.Key != "" {
				t.Fatalf("cost viewer saw another key's row: %+v", row)
			}
		}
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/spend?group_by=key&key=test-key", fakeOperatorCredential(), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("operator on GET /admin/spend: status = %d, want 401", rec.Code)
	}
}

// TestGetSpendAuditsTheQueryNeverTheFigures: one admin_spend_read line with
// group_by, key, the window and the row count; no spend figure.
func TestGetSpendAuditsTheQueryNeverTheFigures(t *testing.T) {
	p, _ := newLedgerSpendTestPipeline(t, time.Now())
	logger, buf := capturingLogger()
	cfg := testConfig()
	cfg.Ledger.RetentionHours = 168
	h := Handler(cfg, p, Credentials{Admin: fakeAdminCredential()}, logger, nil)
	rec := doRequest(t, h, http.MethodGet, "/admin/spend?group_by=session&key=test-key", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	out := buf.String()
	for _, want := range []string{"admin_spend_read", "group_by=session", "key=test-key", "rows=1", "authorized_by=admin", "since=", "until="} {
		if !strings.Contains(out, want) {
			t.Errorf("audit line lacks %q: %s", want, out)
		}
	}
	if strings.Contains(out, "1.5") || strings.Contains(out, "spend_usd") {
		t.Errorf("audit line must not carry spend figures: %s", out)
	}
}

// TestEraseSpendSessionIsAdminOnlyBoundedAndAudited: POST /admin/spend/erase
// is Admin-only (Viewer, CostViewer and Operator are 401); the body is
// bounded at 4 KiB; an empty session, the fold target `other` and a value
// outside the grammar are 400; a success returns rows_erased, removes the
// session's rows across keys and audits a 16-hex SHA-256 prefix of the
// session, never the id.
func TestEraseSpendSessionIsAdminOnlyBoundedAndAudited(t *testing.T) {
	p, ledger := newLedgerSpendTestPipeline(t, time.Now())
	logger, buf := capturingLogger()
	cfg := testConfig()
	cfg.Ledger.RetentionHours = 168
	h := Handler(cfg, p, Credentials{Admin: fakeAdminCredential(), Viewer: fakeViewerCredential(), CostViewer: fakeCostViewerCredential(), Operator: fakeOperatorCredential()}, logger, nil)
	for name, cred := range map[string]string{"viewer": fakeViewerCredential(), "cost viewer": fakeCostViewerCredential(), "operator": fakeOperatorCredential()} {
		rec := doRequest(t, h, http.MethodPost, "/admin/spend/erase", cred, `{"session":"s1"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s on POST /admin/spend/erase: status = %d, want 401", name, rec.Code)
		}
	}
	for name, body := range map[string]string{
		"not json":        `{`,
		"empty session":   `{"session":""}`,
		"missing session": `{}`,
		"the fold target": `{"session":"other"}`,
		"outside grammar": `{"session":"bad|id"}`,
		"too long":        `{"session":"` + strings.Repeat("a", 129) + `"}`,
	} {
		rec := doRequest(t, h, http.MethodPost, "/admin/spend/erase", fakeAdminCredential(), body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body %s", name, rec.Code, rec.Body.String())
		}
	}
	// Over the 4 KiB bound is 413 since 2026-10-12 (decodeJSONBody); before, the
	// bounded reader's error fell into the 400 `invalid request body` text.
	over := doRequest(t, h, http.MethodPost, "/admin/spend/erase", fakeAdminCredential(), `{"session":"s1","pad":"`+strings.Repeat("x", 5000)+`"}`)
	if over.Code != http.StatusRequestEntityTooLarge || over.Body.String() != "request body too large\n" {
		t.Errorf("over the body cap: status = %d, want 413; body %s", over.Code, over.Body.String())
	}
	if strings.Contains(buf.String(), "admin_spend_erased") {
		t.Fatalf("a refused erase must not be audited as an erasure: %s", buf.String())
	}
	rec := doRequest(t, h, http.MethodPost, "/admin/spend/erase", fakeAdminCredential(), `{"session":"s1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("erase s1: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp adminapi.SpendEraseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.RowsErased != 2 {
		t.Fatalf("erase response = %s (%v), want rows_erased 2 (one cell per key)", rec.Body.String(), err)
	}
	rows, err := ledger.Query(context.Background(), spendledger.Query{GroupBy: spendledger.GroupBySession, Since: time.Now().Add(-400 * time.Hour), Until: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Session == "s1" {
			t.Fatalf("s1 still present after erase: %+v", rows)
		}
	}
	sum := sha256.Sum256([]byte("s1"))
	prefix := hex.EncodeToString(sum[:])[:16]
	out := buf.String()
	// The prefix is exactly 16 hex characters: the field ends right after it (slog's text handler separates attributes with a space).
	if !strings.Contains(out, "admin_spend_erased") || !strings.Contains(out, "session_sha256_prefix="+prefix+" rows_erased=2 authorized_by=admin") {
		t.Fatalf("audit line lacks the event, the exact 16-hex prefix, the count or the tier: %s", out)
	}
	if strings.Contains(out, "session=s1") {
		t.Fatalf("audit line carries the raw session id: %s", out)
	}
}
