package admin

import (
	"net/http"
	"strings"
	"testing"
)

// TestPprofGoroutineleakProfileIsServed asks for Go 1.27's goroutine-leak
// profile itself. The named profiles are mounted by name through
// pprof.Handler, which answers 404 "Unknown profile" for a name the running
// toolchain does not register -- so on a toolchain older than 1.27 every
// route-table test would stay green while the route served a 404. debug=1
// selects the text form, whose first line names the profile. The collection
// forces a leak-detecting garbage-collection cycle, the cost the admin
// reference and THREAT_MODEL record for this route.
func TestPprofGoroutineleakProfileIsServed(t *testing.T) {
	h := Handler(pprofEnabledConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)
	rec := limitsRequest(t, h, http.MethodGet, "/admin/debug/pprof/goroutineleak?debug=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/debug/pprof/goroutineleak?debug=1: %d %q, want 200 (a toolchain without the profile answers 404 Unknown profile)", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	first := strings.SplitN(rec.Body.String(), "\n", 2)[0]
	if !strings.HasPrefix(first, "goroutineleak profile: total ") {
		t.Errorf("first line %q, want the text profile header `goroutineleak profile: total N`", first)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type %q under debug=1, want text/plain", ct)
	}
}
