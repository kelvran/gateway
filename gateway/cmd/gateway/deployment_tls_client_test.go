package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// writeTestPEMCert generates a real, self-signed x509 certificate (and,
// when isCA is false, treats it as a leaf usable as a client cert) plus
// its RSA private key, writes both as PEM files under dir, and returns
// their paths -- so newDeploymentTLSTransport parses genuinely valid PEM
// material, not a hand-typed fixture.
func writeTestPEMCert(t *testing.T, dir, prefix string) (certPath, keyPath string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: prefix},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}

	certPath = filepath.Join(dir, prefix+"-cert.pem")
	keyPath = filepath.Join(dir, prefix+"-key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), 0o600); err != nil {
		t.Fatalf("WriteFile cert: %v", err)
	}
	keyDER := x509.MarshalPKCS1PrivateKey(key)
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("WriteFile key: %v", err)
	}
	return certPath, keyPath
}

// TestNewDeploymentTLSTransportLoadsCACertIntoRootCAs proves a real PEM
// CA certificate on disk ends up in the built transport's RootCAs pool.
func TestNewDeploymentTLSTransportLoadsCACertIntoRootCAs(t *testing.T) {
	dir := t.TempDir()
	caCertPath, _ := writeTestPEMCert(t, dir, "ca")

	transport, err := newDeploymentTLSTransport(&controlplane.DeploymentTLSConfig{CACertPath: caCertPath})
	if err != nil {
		t.Fatalf("newDeploymentTLSTransport: %v", err)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("transport.TLSClientConfig.RootCAs = nil, want a populated pool")
	}
}

// TestNewDeploymentTLSTransportLoadsClientCertPair proves a real
// PEM client cert+key pair on disk ends up in the built transport's
// Certificates.
func TestNewDeploymentTLSTransportLoadsClientCertPair(t *testing.T) {
	dir := t.TempDir()
	clientCertPath, clientKeyPath := writeTestPEMCert(t, dir, "client")

	transport, err := newDeploymentTLSTransport(&controlplane.DeploymentTLSConfig{
		ClientCertPath: clientCertPath,
		ClientKeyPath:  clientKeyPath,
	})
	if err != nil {
		t.Fatalf("newDeploymentTLSTransport: %v", err)
	}
	if len(transport.TLSClientConfig.Certificates) != 1 {
		t.Fatalf("len(transport.TLSClientConfig.Certificates) = %d, want 1", len(transport.TLSClientConfig.Certificates))
	}
}

// TestNewDeploymentTLSTransportRejectsUnreadableCACertPath proves a
// missing/unreadable file is a real, loud error, never a silent no-op.
func TestNewDeploymentTLSTransportRejectsUnreadableCACertPath(t *testing.T) {
	_, err := newDeploymentTLSTransport(&controlplane.DeploymentTLSConfig{CACertPath: "/nonexistent/ca.pem"})
	if err == nil {
		t.Fatal("newDeploymentTLSTransport: want an error for an unreadable ca_cert_path, got nil")
	}
}

// TestBuildPerDeploymentTLSClientsOnlyCoversDeploymentsWithTLSConfig
// proves the additive shape: a deployment with no TLSConfig is simply
// absent from both returned maps, and a deployment WITH one gets a
// dedicated buffered client (upstreamHTTPTimeout-bounded) and a
// dedicated streaming client (zero timeout), sharing one transport.
func TestBuildPerDeploymentTLSClientsOnlyCoversDeploymentsWithTLSConfig(t *testing.T) {
	dir := t.TempDir()
	caCertPath, _ := writeTestPEMCert(t, dir, "ca")

	deployments := []controlplane.DeploymentConfig{
		{Name: "plain"},
		{Name: "tls-dep", TLSConfig: &controlplane.DeploymentTLSConfig{CACertPath: caCertPath}},
	}

	buffered, streaming, err := buildPerDeploymentTLSClients(deployments)
	if err != nil {
		t.Fatalf("buildPerDeploymentTLSClients: %v", err)
	}

	if _, ok := buffered["plain"]; ok {
		t.Error("buffered[\"plain\"] present, want absent (no TLSConfig)")
	}
	if _, ok := streaming["plain"]; ok {
		t.Error("streaming[\"plain\"] present, want absent (no TLSConfig)")
	}

	bufferedClient, ok := buffered["tls-dep"]
	if !ok {
		t.Fatal("buffered[\"tls-dep\"] absent, want present")
	}
	streamingClient, ok := streaming["tls-dep"]
	if !ok {
		t.Fatal("streaming[\"tls-dep\"] absent, want present")
	}
	if bufferedClient.Timeout != upstreamHTTPTimeout {
		t.Errorf("buffered[\"tls-dep\"].Timeout = %v, want %v", bufferedClient.Timeout, upstreamHTTPTimeout)
	}
	if streamingClient.Timeout != 0 {
		t.Errorf("streaming[\"tls-dep\"].Timeout = %v, want 0 (idle timeout is enforced elsewhere, not via http.Client.Timeout)", streamingClient.Timeout)
	}
	if bufferedClient.Transport != streamingClient.Transport {
		t.Error("buffered and streaming clients for the same deployment do not share one *http.Transport")
	}
	if _, isHTTPTransport := bufferedClient.Transport.(*http.Transport); !isHTTPTransport {
		t.Error("dedicated client's Transport is not a *http.Transport")
	}
}

// TestBuildPerDeploymentTLSClientsPropagatesLoadError proves a bad
// TLSConfig for one deployment fails the whole construction loudly,
// naming the deployment, rather than silently skipping it.
func TestBuildPerDeploymentTLSClientsPropagatesLoadError(t *testing.T) {
	deployments := []controlplane.DeploymentConfig{
		{Name: "broken", TLSConfig: &controlplane.DeploymentTLSConfig{CACertPath: "/nonexistent/ca.pem"}},
	}
	if _, _, err := buildPerDeploymentTLSClients(deployments); err == nil {
		t.Fatal("buildPerDeploymentTLSClients: want an error for an unreadable ca_cert_path, got nil")
	}
}
