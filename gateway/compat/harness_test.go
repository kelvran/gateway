package compat

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Shared by every test in the package. TestMain fills them in before the
// first test runs and tears the gateway down after the last one; the tests
// run sequentially (none calls t.Parallel) because the mocks record the last
// request each test inspects.
var (
	gatewayURL        string
	gatewayCredential string
	gatewayBin        string
	anthropicMock     *anthropicUpstream
	bedrockMock       *bedrockUpstream
)

// Canonical model names (what a client sends) and the upstream ids the
// deployments rewrite them to (what a mock must see).
const (
	modelAnthropic  = "claude-sys"
	modelBedrock45  = "claude-bedrock-45"
	modelBedrock55  = "claude-bedrock-55"
	upstreamModelID = "claude-upstream-id"
	// haiku-4-5 is a Claude generation the Bedrock adapter knows accepts
	// thinking.type "enabled", so the field is forwarded and the mock's 400 is
	// what the client sees -- RFC-1 line 191's case. sonnet-5-5 is a generation
	// known to reject it, so the adapter drops the field and the turn succeeds.
	bedrock45UpstreamID = "anthropic.claude-haiku-4-5-20251001-v1:0"
	bedrock55UpstreamID = "global.anthropic.claude-sonnet-5-5"
	// Deployment names are no substring of any model name, so a redaction
	// assertion can tell a leaked deployment name from a legitimate mention
	// of the model the client asked for.
	deploymentBedrock45 = "dep-haiku45"
	deploymentBedrock55 = "dep-sonnet55"
	// The deployment-side value the gateway must present to the Anthropic mock
	// as x-api-key. The config names the variable; only the child process's
	// environment carries the value.
	upstreamCredential    = "compat-mock-upstream-credential-not-a-secret"
	upstreamCredentialEnv = "KELVRAN_COMPAT_UPSTREAM_CREDENTIAL"
	awsAccessKeyIDEnv     = "KELVRAN_COMPAT_AWS_ACCESS_KEY_ID"
	awsSecretEnv          = "KELVRAN_COMPAT_AWS_SECRET_ACCESS_KEY"
	// A prebuilt gateway binary to run the matrix against instead of building
	// ./cmd/gateway from the tree -- the red proof against gateway/v0.18.0
	// used it, and it lets an operator point the matrix at a release artifact.
	gatewayBinaryEnv = "KELVRAN_COMPAT_GATEWAY_BIN"

	readyTimeout = 60 * time.Second
	stopTimeout  = 10 * time.Second
	testTimeout  = 30 * time.Second
	// startAttempts bounds the retry when another process takes the reserved
	// port between freeAddr's release and the gateway's bind.
	startAttempts = 3
	// maxLogBytes caps the captured gateway log so a logging loop in the
	// binary under test cannot exhaust the runner's memory.
	maxLogBytes = 8 << 20
)

func TestMain(m *testing.M) {
	code, err := runMatrix(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "compat harness:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// runMatrix builds (or locates) the gateway, starts the two upstream mocks,
// writes a config that routes the three canonical models to them, starts the
// gateway and runs every test against it. The gateway's own log is printed
// only when a test failed -- it is the one place the redaction tests' hidden
// detail is allowed to appear.
func runMatrix(m *testing.M) (int, error) {
	workdir, err := os.MkdirTemp("", "kelvran-compat-")
	if err != nil {
		return 1, err
	}
	defer func() { _ = os.RemoveAll(workdir) }()

	bin, err := gatewayBinary(workdir)
	if err != nil {
		return 1, err
	}
	gatewayBin = bin
	anthropicMock, err = newAnthropicUpstream()
	if err != nil {
		return 1, err
	}
	defer anthropicMock.Close()
	bedrockMock, err = newBedrockUpstream()
	if err != nil {
		return 1, err
	}
	defer bedrockMock.Close()

	gatewayCredential, err = newCredential()
	if err != nil {
		return 1, err
	}
	gw, addr, err := startGatewayOnFreePort(bin, workdir, gatewayCredential, anthropicMock.URL(), bedrockMock.URL())
	if err != nil {
		return 1, err
	}
	defer gw.stop()
	gatewayURL = "http://" + addr

	code := m.Run()
	if code != 0 {
		fmt.Fprintf(os.Stderr, "--- gateway log (%s) ---\n%s\n", bin, gw.output())
	}
	return code, nil
}

// gatewayBinary returns the binary under test: the operator's override when
// set, otherwise a fresh build of ./cmd/gateway from the gateway module this
// module nests in (the test's working directory is this package).
func gatewayBinary(workdir string) (string, error) {
	if bin := os.Getenv(gatewayBinaryEnv); bin != "" {
		if _, err := os.Stat(bin); err != nil { //nolint:gosec // the operator names the binary under test on purpose

			return "", fmt.Errorf("%s: %w", gatewayBinaryEnv, err)
		}
		return bin, nil
	}
	moduleRoot, err := filepath.Abs("..")
	if err != nil {
		return "", err
	}
	out := filepath.Join(workdir, "gateway")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/gateway") //nolint:gosec // out is inside this harness's own temp dir
	cmd.Dir = moduleRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building the gateway from %s: %w\n%s", moduleRoot, err, output)
	}
	return out, nil
}

// newCredential is the virtual key's raw secret for this run: 32 random
// bytes, hex-encoded. The config carries only its SHA-256.
func newCredential() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// freeAddr reserves and releases a loopback port. The gateway logs the
// configured listen address, not the bound one, so ":0" would leave the
// harness no way to learn the port. The window between the release and the
// gateway's bind is real on a shared runner; startGatewayOnFreePort retries
// when the gateway loses that race.
func freeAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	return addr, ln.Close()
}

// startGatewayOnFreePort reserves a port, writes the config for it, starts
// the gateway and waits for /readyz; when the gateway exits because another
// process took the port first, it tries again with a new port.
func startGatewayOnFreePort(bin, workdir, credential, anthropicURL, bedrockURL string) (*gatewayProcess, string, error) {
	var lastErr error
	for attempt := 1; attempt <= startAttempts; attempt++ {
		addr, err := freeAddr()
		if err != nil {
			return nil, "", err
		}
		cfgPath := filepath.Join(workdir, fmt.Sprintf("config-%d.yaml", attempt))
		cfg := gatewayConfig(addr, credential, anthropicURL, bedrockURL)
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
			return nil, "", err
		}
		gw, err := startGateway(bin, cfgPath)
		if err != nil {
			return nil, "", err
		}
		err = waitReady("http://"+addr, gw)
		if err == nil {
			return gw, addr, nil
		}
		if !gw.lostPortRace() {
			gw.stop()
			return nil, "", err
		}
		lastErr = err
	}
	return nil, "", fmt.Errorf("no free loopback port after %d attempts: %w", startAttempts, lastErr)
}

// gatewayConfig is the minimal operator config the matrix runs against: one
// virtual key with a generous request budget, one anthropic deployment (the
// passthrough path) and two bedrock deployments (the translate path, one per
// thinking-capability generation), no admin server, no telemetry exporter.
func gatewayConfig(addr, credential, anthropicURL, bedrockURL string) string {
	hash := sha256.Sum256([]byte(credential))
	bedrockDeployment := func(name, model, upstreamID string) string {
		return fmt.Sprintf(`  %s:
    model: %q
    provider: "bedrock"
    upstream_model: %q
    base_url: %q
    allow_insecure_http: true
    access_key_id_env: %q
    secret_access_key_env: %q
    region: "us-east-1"
`, name, model, upstreamID, bedrockURL+"/model/"+upstreamID+"/converse", awsAccessKeyIDEnv, awsSecretEnv)
	}
	return fmt.Sprintf(`listen_addr: %q
virtual_keys:
  compat:
    key_hash: %q
    rate_limit:
      burst: 1000
      refill_per_second: 1000
deployments:
  claude-primary:
    model: %q
    provider: "anthropic"
    upstream_model: %q
    base_url: %q
    allow_insecure_http: true
    api_key_env: %q
%s%stelemetry:
  exporter: "none"
`, addr, hex.EncodeToString(hash[:]), modelAnthropic, upstreamModelID, anthropicURL+"/v1/messages", upstreamCredentialEnv,
		bedrockDeployment(deploymentBedrock45, modelBedrock45, bedrock45UpstreamID),
		bedrockDeployment(deploymentBedrock55, modelBedrock55, bedrock55UpstreamID))
}

type gatewayProcess struct {
	cmd  *exec.Cmd
	log  *syncBuffer
	done chan struct{}
	err  error
}

func startGateway(bin, cfgPath string) (*gatewayProcess, error) {
	cmd := exec.Command(bin, "-config", cfgPath) //nolint:gosec // bin is this harness's own build output or the operator's explicit override
	buf := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = buf, buf
	// A minimal environment, not the runner's: the gateway needs nothing but
	// its credential variables, and a CI runner's environment is not the
	// child's business (nor the log's, which is printed on failure).
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + os.Getenv("TMPDIR"),
		upstreamCredentialEnv + "=" + upstreamCredential,
		awsAccessKeyIDEnv + "=compat-fake-access-key-id",
		awsSecretEnv + "=compat-fake-secret-access-value",
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", bin, err)
	}
	p := &gatewayProcess{cmd: cmd, log: buf, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

func (p *gatewayProcess) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *gatewayProcess) output() string { return p.log.String() }

// lostPortRace reports that the gateway exited because its reserved port
// was taken between freeAddr's release and the bind -- Go's net package
// words it the same way on Linux and macOS.
func (p *gatewayProcess) lostPortRace() bool {
	return p.exited() && strings.Contains(p.output(), "address already in use")
}

// stop asks for a graceful shutdown first; the gateway handles SIGTERM.
func (p *gatewayProcess) stop() {
	if p.exited() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(stopTimeout):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func waitReady(baseURL string, gw *gatewayProcess) error {
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		if gw.exited() {
			return fmt.Errorf("gateway exited before it was ready (%w):\n%s", gw.err, gw.output())
		}
		if readyOnce(client, baseURL) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("gateway not ready within %s:\n%s", readyTimeout, gw.output())
}

func readyOnce(client *http.Client, baseURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/readyz", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

// syncBuffer collects the child's stdout and stderr from two writers at once,
// keeping the first maxLogBytes and noting the cut.
type syncBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

// Write always reports the full count: os/exec drains the child's pipe with
// io.Copy, which treats a short count as io.ErrShortWrite, stops copying and
// lets the child die on its next write -- the cap must truncate the record,
// never the child.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if room := maxLogBytes - b.buf.Len(); n > room {
		b.truncated = true
		p = p[:max(room, 0)]
	}
	b.buf.Write(p)
	return n, nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return b.buf.String() + "\n[gateway log truncated at 8 MiB]"
	}
	return b.buf.String()
}

// uniqueQuestion makes each test's request body distinct. The gateway's
// response cache is on, as it is for every operator, so two tests sending the
// same body would see the second answer re-encoded from the cache instead of
// relayed from the mock; the one test that wants the cache hit asks for it.
func uniqueQuestion(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s [%s]", question, t.Name())
}

// The retry in startGatewayOnFreePort rests on recognising a lost port race
// from the gateway's own exit; this holds the port and proves the wording the
// detection relies on is what this platform's gateway prints.
func TestHarnessRecognisesALostPortRace(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := gatewayConfig(ln.Addr().String(), gatewayCredential, anthropicMock.URL(), bedrockMock.URL())
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	gw, err := startGateway(gatewayBin, cfgPath)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer gw.stop()
	select {
	case <-gw.done:
	case <-time.After(readyTimeout):
		t.Fatalf("the gateway kept running on a port another listener holds:\n%s", gw.output())
	}
	if !gw.lostPortRace() {
		t.Errorf("exit on an occupied port was not recognised as a lost race:\n%s", gw.output())
	}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

// assertRedacted fails when a client-visible string carries any operator
// detail: the deployment name, the upstream's address or path, or the
// upstream's own wording.
func assertRedacted(t *testing.T, what, got string, forbidden ...string) {
	t.Helper()
	for _, needle := range forbidden {
		if strings.Contains(got, needle) {
			t.Errorf("%s %q leaks %q", what, got, needle)
		}
	}
}

// assertHeadersRedacted runs assertRedacted over every response header value
// and fails on any header the upstream's cloud stamped (x-amzn-*).
func assertHeadersRedacted(t *testing.T, h http.Header, forbidden ...string) {
	t.Helper()
	for name, values := range h {
		if strings.HasPrefix(strings.ToLower(name), "x-amzn-") {
			t.Errorf("response header %s reached the client", name)
		}
		for _, v := range values {
			assertRedacted(t, "response header "+name, v, forbidden...)
		}
	}
}

// assertClientCredentialNotForwarded fails if the virtual key's secret, in
// any header form, reached an upstream.
func assertClientCredentialNotForwarded(t *testing.T, fwd recordedRequest) {
	t.Helper()
	for name, values := range fwd.Header {
		for _, v := range values {
			if strings.Contains(v, gatewayCredential) {
				t.Errorf("the client's credential reached the upstream in header %s", name)
			}
		}
	}
}

// hostPort is a mock's address without the scheme, the shape a leaked dial
// target takes ("127.0.0.1" alone would miss a port-only leak).
func hostPort(rawURL string) string { return strings.TrimPrefix(rawURL, "http://") }
