// See admin_integration_test.go's own doc comment for why this lives in
// package main, not an external _test package: it needs buildPipeline,
// newAdminMTLSConfig, and writeTestPEMCert (from
// deployment_tls_client_test.go), all unexported on purpose.
package main

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/admin"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// fakeAdminMTLSCredential returns a fake, non-secret admin bearer
// credential, assembled from parts rather than one literal, mirroring
// fakeAdminCredentialForIntegrationTest's own established pattern for
// avoiding false-positive secret-scanning matches on obviously-fake test
// values.
func fakeAdminMTLSCredential() string {
	parts := []string{"not", "a", "real", "admin", "credential", "mtls", "test"}
	return strings.Join(parts, "-")
}

// newAdminMTLSTestServer builds ONE real pipeline (via buildPipeline, the
// exact function main.run() calls) and serves admin.Handler through a
// REAL httptest.Server with mTLS enabled via newAdminMTLSConfig -- the
// exact function main.run() calls when admin.mtls is configured --
// mirroring newAdminIntegrationServers' real-pipeline convention in
// admin_integration_test.go. Uses httptest.NewUnstartedServer +
// srv.TLS + StartTLS() (its own documented "use s.TLS as the base
// config, and don't touch Certificates if already set" behavior) rather
// than a hand-rolled net.Listener, since that fits this package's
// existing httptest-based admin-server test convention. Returns the
// started *httptest.Server plus the admin bearer credential.
func newAdminMTLSTestServer(t *testing.T, caCertPath, serverCertPath, serverKeyPath string) (srv *httptest.Server, adminToken string) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY_ADMIN_MTLS_TEST", "fake-upstream-key-not-a-real-secret")

	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "gpt4o-primary",
				Model:         "gpt-4o",
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       "https://example.invalid",
				APIKeyEnv:     "OPENAI_API_KEY_ADMIN_MTLS_TEST",
			},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"gpt-4o": {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}

	adminToken = fakeAdminMTLSCredential()
	handler := admin.Handler(cfg, pipeline, admin.Credentials{Admin: adminToken}, logger, nil)

	adminTLSConfig, err := newAdminMTLSConfig(&controlplane.AdminMTLSConfig{
		CACertPath:     caCertPath,
		ServerCertPath: serverCertPath,
		ServerKeyPath:  serverKeyPath,
	})
	if err != nil {
		t.Fatalf("newAdminMTLSConfig: %v", err)
	}

	srv = httptest.NewUnstartedServer(handler)
	srv.TLS = adminTLSConfig
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return srv, adminToken
}

// clientWithCert builds an *http.Client presenting the given client
// certificate (or none at all, when certPath/keyPath are both empty) and
// skipping server-certificate verification. This test's focus is the
// admin server's own client-certificate ENFORCEMENT (ClientAuth), not
// whether a generic HTTP client trusts this self-signed test server's own
// certificate -- an orthogonal concern already covered by ordinary TLS
// library behavior and irrelevant to what this feature adds.
//
// Deliberately uses tls.Config.GetClientCertificate rather than the
// static tlsConfig.Certificates list: crypto/tls's own client, when given
// a static list, silently filters it down to certificates whose issuer
// matches one of the server's advertised acceptable CAs (see
// (*tls.Config).getClientCertificate / CertificateRequestInfo.
// SupportsCertificate) and sends an EMPTY certificate message instead of
// a non-matching one. That auto-filtering would make a "wrong CA"
// client cert collapse into the exact same "client didn't provide a
// certificate" handshake failure as no certificate at all, silently
// testing nothing beyond the no-cert case. GetClientCertificate bypasses
// that filtering and unconditionally sends whatever certificate was
// loaded, which is what every caller of clientWithCert actually wants to
// exercise.
func clientWithCert(t *testing.T, certPath, keyPath string) *http.Client {
	t.Helper()
	tlsConfig := &tls.Config{InsecureSkipVerify: true}
	if certPath != "" {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			t.Fatalf("tls.LoadX509KeyPair: %v", err)
		}
		tlsConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &cert, nil
		}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}
}

// TestAdminMTLSRejectsClientWithNoCertificate proves ClientAuth is
// RequireAndVerifyClientCert, not VerifyClientCertIfGiven: a client that
// presents no certificate at all must fail the TLS handshake itself --
// never merely reach the handler and get a 401.
func TestAdminMTLSRejectsClientWithNoCertificate(t *testing.T) {
	dir := t.TempDir()
	caCertPath, _ := writeTestPEMCert(t, dir, "ca")
	serverCertPath, serverKeyPath := writeTestPEMCert(t, dir, "server")

	srv, _ := newAdminMTLSTestServer(t, caCertPath, serverCertPath, serverKeyPath)

	client := clientWithCert(t, "", "")
	resp, err := client.Get(srv.URL + "/admin/config")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("client.Get with no client certificate: want a TLS handshake failure, got a successful response")
	}
	// Assert the SPECIFIC handshake-level rejection reason, not merely
	// "some error happened" -- a generic err != nil check can't tell this
	// failure apart from an unrelated one (a typo'd port, the test server
	// never starting, a panic before the handshake even begins), and
	// specifically can't tell it apart from
	// TestAdminMTLSRejectsClientWithWrongCACertificate's own distinct
	// failure mode below.
	if !strings.Contains(err.Error(), "certificate required") {
		t.Fatalf("client.Get with no client certificate: err = %q, want it to mention \"certificate required\"", err)
	}
}

// TestAdminMTLSRejectsClientWithWrongCACertificate proves a certificate
// signed by a real, but DIFFERENT and unrelated, CA is rejected at the
// handshake level exactly like no certificate at all -- the server
// verifies against its OWN configured CA specifically, not "any CA".
func TestAdminMTLSRejectsClientWithWrongCACertificate(t *testing.T) {
	dir := t.TempDir()
	caCertPath, _ := writeTestPEMCert(t, dir, "ca")
	serverCertPath, serverKeyPath := writeTestPEMCert(t, dir, "server")
	wrongCertPath, wrongKeyPath := writeTestPEMCert(t, dir, "wrongca")

	srv, _ := newAdminMTLSTestServer(t, caCertPath, serverCertPath, serverKeyPath)

	client := clientWithCert(t, wrongCertPath, wrongKeyPath)
	resp, err := client.Get(srv.URL + "/admin/config")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("client.Get with a certificate signed by an unrelated CA: want a TLS handshake failure, got a successful response")
	}
	// Assert the SPECIFIC x509-verification rejection reason -- distinct
	// from TestAdminMTLSRejectsClientWithNoCertificate's "certificate
	// required" above -- to prove the server actually walked the
	// presented certificate's chain against its OWN configured CA and
	// rejected it for THAT reason, not merely that some unrelated error
	// occurred (or that no certificate was sent at all, which is exactly
	// the failure this test collapsed into before clientWithCert started
	// using GetClientCertificate).
	if !strings.Contains(err.Error(), "unknown certificate authority") {
		t.Fatalf("client.Get with a certificate signed by an unrelated CA: err = %q, want it to mention \"unknown certificate authority\"", err)
	}
}

// TestAdminMTLSAcceptsCorrectClientCertWithValidBearerToken proves the
// additive, defense-in-depth shape end to end: a client presenting a
// certificate signed by the server's own configured CA, AND a valid
// admin bearer token, gets a normal 200 from a real existing read route.
func TestAdminMTLSAcceptsCorrectClientCertWithValidBearerToken(t *testing.T) {
	dir := t.TempDir()
	caCertPath, caKeyPath := writeTestPEMCert(t, dir, "ca")
	serverCertPath, serverKeyPath := writeTestPEMCert(t, dir, "server")

	srv, adminToken := newAdminMTLSTestServer(t, caCertPath, serverCertPath, serverKeyPath)

	client := clientWithCert(t, caCertPath, caKeyPath)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/admin/config", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do with a correctly-signed client cert and a valid bearer token: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 -- mTLS must be additive, never blocking an otherwise-authorized admin request", resp.StatusCode)
	}
}

// TestAdminMTLSStillRequiresBearerTokenWithCorrectClientCert proves mTLS
// is additive, not a replacement for the existing bearer-token check: a
// client with a correctly-signed cert but no bearer token still gets
// rejected at the APPLICATION layer, not waved through on cert alone.
func TestAdminMTLSStillRequiresBearerTokenWithCorrectClientCert(t *testing.T) {
	dir := t.TempDir()
	caCertPath, caKeyPath := writeTestPEMCert(t, dir, "ca")
	serverCertPath, serverKeyPath := writeTestPEMCert(t, dir, "server")

	srv, _ := newAdminMTLSTestServer(t, caCertPath, serverCertPath, serverKeyPath)

	client := clientWithCert(t, caCertPath, caKeyPath)
	resp, err := client.Get(srv.URL + "/admin/config")
	if err != nil {
		t.Fatalf("client.Get with a correct client cert but no bearer token: unexpected transport-level error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 -- a correct mTLS client cert must not bypass the existing bearer-token check", resp.StatusCode)
	}
}
