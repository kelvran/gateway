package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCapPprofSecondsRefusesOnlyValuesAboveTheCap(t *testing.T) {
	cases := []struct {
		raw     string
		refused bool
	}{
		{"61", true}, {"60.5", true}, {"1e3", true}, {"100000000", true},
		{"60", false}, {"30", false}, {"0.5", false},
		// the library's own business: defaults for these, or its own 400
		{"0", false}, {"-5", false}, {"abc", false}, {"", false},
	}
	for _, c := range cases {
		called := false
		h := capPprofSeconds(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusTeapot)
		}), 60)
		target := "/admin/debug/pprof/profile"
		if c.raw != "" {
			target += "?seconds=" + url.QueryEscape(c.raw)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		switch {
		case c.refused && (rec.Code != http.StatusBadRequest || rec.Body.String() != "seconds must be at most 60\n" || called):
			t.Errorf("seconds=%q: %d %q called=%v, want 400 %q and the handler not run", c.raw, rec.Code, rec.Body.String(), called, "seconds must be at most 60")
		case !c.refused && (!called || rec.Code != http.StatusTeapot):
			t.Errorf("seconds=%q: %d called=%v, want the handler to run untouched", c.raw, rec.Code, called)
		}
	}
}

func TestDecodeJSONBodySplitsTooLargeFromInvalid(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	run := func(body string, limit int64) (*httptest.ResponseRecorder, payload, bool) {
		rec := httptest.NewRecorder()
		var p payload
		ok := decodeJSONBody(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)), limit, &p)
		return rec, p, ok
	}
	if rec, _, ok := run(`{"name":"`+strings.Repeat("x", 100)+`"}`, 64); ok || rec.Code != http.StatusRequestEntityTooLarge || rec.Body.String() != "request body too large\n" {
		t.Errorf("over the limit: ok=%v %d %q, want false 413 %q", ok, rec.Code, rec.Body.String(), "request body too large")
	}
	if rec, _, ok := run(`{"name":`, 64); ok || rec.Code != http.StatusBadRequest || !strings.HasPrefix(rec.Body.String(), "invalid request body: ") {
		t.Errorf("invalid JSON under the limit: ok=%v %d %q, want false 400 invalid request body", ok, rec.Code, rec.Body.String())
	}
	if rec, p, ok := run(`{"name":"ok"}`, 64); !ok || rec.Code != http.StatusOK || p.Name != "ok" {
		t.Errorf("valid under the limit: ok=%v %d name=%q, want true and the value decoded (recorder untouched = 200)", ok, rec.Code, p.Name)
	}
	exact := `{"name":"` + strings.Repeat("y", 64-11) + `"}`
	if int64(len(exact)) != 64 {
		t.Fatalf("fixture is %d bytes, want 64", len(exact))
	}
	if rec, p, ok := run(exact, 64); !ok || rec.Code != http.StatusOK || len(p.Name) != 53 {
		t.Errorf("exactly at the limit: ok=%v %d name len %d, want true and the value decoded", ok, rec.Code, len(p.Name))
	}
}
