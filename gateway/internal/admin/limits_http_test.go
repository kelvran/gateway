package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// limitsRequest sends one authenticated request to the admin handler and
// returns the recorder; it does not go through doRequest so the body can be
// any size and the method any verb.
func limitsRequest(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+fakeAdminCredential())
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// pprofSecondsRoutes are the eight pprof routes that read `seconds`.
var pprofSecondsRoutes = []string{
	"/admin/debug/pprof/profile", "/admin/debug/pprof/trace",
	"/admin/debug/pprof/goroutine", "/admin/debug/pprof/heap", "/admin/debug/pprof/threadcreate",
	"/admin/debug/pprof/block", "/admin/debug/pprof/mutex", "/admin/debug/pprof/allocs",
}

// TestPprofRoutesRefuseSecondsAboveTheCap: under the pinned toolchain
// net/http/pprof never bounds `seconds` -- it extends the connection's write
// deadline by that much -- so before the cap an Admin-tier caller could hold
// the exclusive CPU profiler and a connection for as long as it asked. The
// gateway answers 400 above 60 before the library runs; a value the library
// itself rejects keeps the library's own answer.
func TestPprofRoutesRefuseSecondsAboveTheCap(t *testing.T) {
	h := Handler(pprofEnabledConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)
	for _, path := range pprofSecondsRoutes {
		start := time.Now()
		rec := limitsRequest(t, h, http.MethodGet, path+"?seconds=61", "")
		if rec.Code != http.StatusBadRequest || rec.Body.String() != "seconds must be at most 60\n" {
			t.Errorf("GET %s?seconds=61: %d %q, want 400 %q", path, rec.Code, rec.Body.String(), "seconds must be at most 60")
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("GET %s?seconds=61 took %v: the profile ran instead of being refused", path, took)
		}
	}
	rec := limitsRequest(t, h, http.MethodGet, "/admin/debug/pprof/heap?seconds=0", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "positive integer") {
		t.Errorf("heap?seconds=0: %d %q, want net/http/pprof's own 400 about a positive integer", rec.Code, rec.Body.String())
	}
}

// adminWriteRoutes are the six write routes whose body was unbounded before
// 2026-10-12 (POST /admin/spend/erase has its own 4 KiB bound and is tested
// below).
var adminWriteRoutes = []struct{ method, path string }{
	{http.MethodPost, "/admin/virtual_keys/k1"},
	{http.MethodPost, "/admin/virtual_keys/k1/rotate"},
	{http.MethodPost, "/admin/deployments/d1/weight"},
	{http.MethodPost, "/admin/cache/erase"},
	{http.MethodPost, "/admin/prompts/p1"},
	{http.MethodPut, "/admin/prompts/p1/labels/l1"},
}

// paddedJSON returns a JSON object of exactly total bytes: one string member
// named `pad`, which every write route's decoder ignores (no decoder in the
// gateway rejects unknown members).
func paddedJSON(prefix string, total int) string {
	const suffix = `"}`
	return prefix + strings.Repeat("x", total-len(prefix)-len(suffix)) + suffix
}

// TestAdminWriteRoutesRefuseBodiesOverTheLimit pins the documented 1 MiB cap
// (4 KiB on the erase route): one byte over is 413 `request body too large`
// before any handler logic runs; exactly at the limit the body decodes and
// whatever the handler answers next is not a size refusal.
func TestAdminWriteRoutesRefuseBodiesOverTheLimit(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)
	const limit = 1 << 20
	for _, rt := range adminWriteRoutes {
		over := paddedJSON(`{"pad":"`, limit+1)
		rec := limitsRequest(t, h, rt.method, rt.path, over)
		if rec.Code != http.StatusRequestEntityTooLarge || rec.Body.String() != "request body too large\n" {
			t.Errorf("%s %s with %d bytes: %d %q, want 413 %q", rt.method, rt.path, len(over), rec.Code, strings.TrimSpace(rec.Body.String()), "request body too large")
		}
		exact := paddedJSON(`{"pad":"`, limit)
		if rec := limitsRequest(t, h, rt.method, rt.path, exact); rec.Code == http.StatusRequestEntityTooLarge {
			t.Errorf("%s %s with exactly %d bytes: 413, want the body to decode", rt.method, rt.path, len(exact))
		}
	}
	const eraseLimit = 4096
	over := paddedJSON(`{"session":"s1","pad":"`, eraseLimit+1)
	rec := limitsRequest(t, h, http.MethodPost, "/admin/spend/erase", over)
	if rec.Code != http.StatusRequestEntityTooLarge || rec.Body.String() != "request body too large\n" {
		t.Errorf("POST /admin/spend/erase with %d bytes: %d %q, want 413 %q", len(over), rec.Code, strings.TrimSpace(rec.Body.String()), "request body too large")
	}
	exact := paddedJSON(`{"session":"s1","pad":"`, eraseLimit)
	if rec := limitsRequest(t, h, http.MethodPost, "/admin/spend/erase", exact); rec.Code == http.StatusRequestEntityTooLarge {
		t.Errorf("POST /admin/spend/erase with exactly %d bytes: 413, want the body to decode", len(exact))
	}
}
