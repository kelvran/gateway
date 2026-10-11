package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMetricsMuxServesGetAndHeadOnly: the third listener's mux routes
// GET and HEAD /metrics to the handler, answers any other method with 405
// and `Allow: GET, HEAD`, and any other path with 404 — the surface named in
// docs/rfcs/2026-10-09-gateway-fallback-rescued-and-prometheus-pull.md §13c.
func TestMetricsMuxServesGetAndHeadOnly(t *testing.T) {
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, "kelvran_test 1\n")
	})
	srv := httptest.NewServer(newMetricsMux(stub))
	t.Cleanup(srv.Close)

	cases := []struct {
		method, path string
		wantStatus   int
		wantAllow    string
		wantBody     bool
	}{
		{http.MethodGet, "/metrics", http.StatusOK, "", true},
		{http.MethodHead, "/metrics", http.StatusOK, "", false},
		{http.MethodPost, "/metrics", http.StatusMethodNotAllowed, "GET, HEAD", false},
		{http.MethodGet, "/other", http.StatusNotFound, "", false},
		{http.MethodGet, "/", http.StatusNotFound, "", false},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(tc.method, srv.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.wantStatus {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.wantStatus)
		}
		if tc.wantAllow != "" && resp.Header.Get("Allow") != tc.wantAllow {
			t.Errorf("%s %s Allow = %q, want %q", tc.method, tc.path, resp.Header.Get("Allow"), tc.wantAllow)
		}
		if hasBody := len(body) > 0 && string(body) == "kelvran_test 1\n"; hasBody != tc.wantBody {
			t.Errorf("%s %s body %q, want served body = %v", tc.method, tc.path, body, tc.wantBody)
		}
	}
	if defaultMetricsListenAddr != "127.0.0.1:9464" {
		t.Errorf("defaultMetricsListenAddr = %q, want the loopback default the RFC names", defaultMetricsListenAddr)
	}
}

// TestIsLoopbackListenAddr: the startup warning for a non-loopback metrics
// bind fires for every-interface and remote binds and for garbage, never for
// the loopback forms.
func TestIsLoopbackListenAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:9464": true, "[::1]:9464": true, "localhost:9464": true, "127.0.0.2:9464": true,
		"0.0.0.0:9464": false, ":9464": false, "10.0.0.5:9464": false, "[::]:9464": false, "garbage": false, "": false,
	}
	for addr, want := range cases {
		if got := isLoopbackListenAddr(addr); got != want {
			t.Errorf("isLoopbackListenAddr(%q) = %v, want %v", addr, got, want)
		}
	}
	if !isLoopbackListenAddr(defaultMetricsListenAddr) {
		t.Error("the default bind must be loopback")
	}
}
