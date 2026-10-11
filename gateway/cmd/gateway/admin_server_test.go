package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"testing"
	"time"
)

// TestNewAdminServerCarriesEveryTimeout pins the admin listener's four server
// timeouts to the values docs/reference/admin-api.md ("Timeouts") states, and
// that the constructor adds nothing else (TLS stays the caller's).
func TestNewAdminServerCarriesEveryTimeout(t *testing.T) {
	handler := http.NotFoundHandler()
	srv := newAdminServer("127.0.0.1:0", handler)
	if srv.Addr != "127.0.0.1:0" || srv.Handler == nil {
		t.Fatalf("newAdminServer kept neither the address nor the handler: %+v", srv)
	}
	if srv.TLSConfig != nil {
		t.Fatalf("newAdminServer set a TLSConfig; admin.mtls is the caller's to add")
	}
	for _, tc := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout, 10 * time.Second},
		{"ReadTimeout", srv.ReadTimeout, 60 * time.Second},
		{"WriteTimeout", srv.WriteTimeout, 60 * time.Second},
		{"IdleTimeout", srv.IdleTimeout, 60 * time.Second},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// TestAdminServerWriteTimeoutIsExtendedByPprof proves, in this toolchain, the
// property the admin write timeout relies on: a pprof request that runs for
// `seconds` completes under a WriteTimeout shorter than that, because
// net/http/pprof extends the connection's write deadline by
// WriteTimeout + seconds itself (so heap?seconds=1 has two seconds under a
// one-second bound). The control is a plain handler that sleeps twice the
// timeout on the same server and is cut off by it, which proves
// the timeout was live; without the control the pprof leg would pass
// vacuously on a server with no timeout at all.
func TestAdminServerWriteTimeoutIsExtendedByPprof(t *testing.T) {
	const writeTimeout = time.Second
	mux := http.NewServeMux()
	mux.Handle("GET /debug/pprof/heap", pprof.Handler("heap"))
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * writeTimeout)
		_, _ = io.WriteString(w, "late")
	})
	srv := newAdminServer("127.0.0.1:0", mux)
	// The shipped bound is 60 s; the test shrinks it so one run takes seconds.
	srv.WriteTimeout = writeTimeout

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 10 * time.Second}
	get := func(path string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return client.Do(req)
	}

	// The control: a handler that writes only after the write deadline passed
	// never reaches the client, so the client sees a transport error.
	if resp, err := get("/slow"); err == nil {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("the control handler answered %d %q past a %v write timeout; the timeout is not live, so the pprof leg below would prove nothing", resp.StatusCode, body, writeTimeout)
	}

	// The pprof leg: a one-second delta profile under the same one-second
	// write timeout completes, because net/http/pprof extended the deadline
	// to WriteTimeout + seconds.
	resp, err := get("/debug/pprof/heap?seconds=1")
	if err != nil {
		t.Fatalf("heap?seconds=1 under a %v write timeout: %v (net/http/pprof no longer extends the write deadline; the admin bound would cut profiles short)", writeTimeout, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("reading the delta profile: %v", err)
	}
	if resp.StatusCode != http.StatusOK || len(body) == 0 {
		t.Fatalf("heap?seconds=1 answered %d with %d body bytes, want 200 with a profile", resp.StatusCode, len(body))
	}
}
