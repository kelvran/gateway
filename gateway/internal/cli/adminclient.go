package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Admin-API access shared by every verb that talks to a running gateway
// (RFC-3 decision 3): doctor --admin-url, keys, status and spend; connect
// later. The token is never a flag value; the URL is guarded before the
// first request leaves.

const (
	defaultAdminURL      = "http://127.0.0.1:8081"
	adminTokenFileEnv    = "KELVRAN_ADMIN_TOKEN_FILE" //nolint:gosec // G101: the NAME of an environment variable
	adminURLEnv          = "KELVRAN_ADMIN_URL"
	adminRequestTimeout  = 5 * time.Second
	allowInsecureHTTPFlg = "--allow-insecure-http"
)

// resolveAdminToken follows decision 3's order: --admin-token-file, the
// KELVRAN_ADMIN_TOKEN_FILE variable, the variable admin.token_env names
// (when a config loaded), then KELVRAN_ADMIN_TOKEN. It returns the token and
// a short description of where it came from (never the value), or "" when
// none resolved.
func resolveAdminToken(tokenFile string, configTokenEnv string, getenv func(string) string) (token, source string, err error) {
	if tokenFile == "" {
		tokenFile = getenv(adminTokenFileEnv)
	}
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile) //nolint:gosec // G304: the operator's own token file
		if err != nil {
			return "", "", fmt.Errorf("reading the admin token file: %w", err)
		}
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, "the admin token file", nil
		}
		return "", "", errors.New("the admin token file is empty")
	}
	if configTokenEnv != "" {
		if v := getenv(configTokenEnv); v != "" {
			return v, "the variable admin.token_env names (" + configTokenEnv + ")", nil
		}
	}
	if v := getenv(adminTokenEnvName); v != "" {
		return v, adminTokenEnvName, nil
	}
	return "", "", nil
}

// resolveAdminURL is --admin-url, else KELVRAN_ADMIN_URL, else the default,
// then the credential-carrying URL guard: https, or http with a loopback
// host, or --allow-insecure-http. The refusal wording is decision 3's.
func resolveAdminURL(flagValue string, allowInsecure bool, getenv func(string) string) (string, error) {
	raw, flag := flagValue, "--admin-url"
	if raw == "" {
		raw, flag = getenv(adminURLEnv), adminURLEnv
	}
	if raw == "" {
		raw = defaultAdminURL
	}
	if err := guardCredentialURL(flag, raw, allowInsecure); err != nil {
		return "", err
	}
	return strings.TrimRight(raw, "/"), nil
}

// guardCredentialURL refuses to send a credential to a URL that is neither
// https nor loopback unless --allow-insecure-http was passed.
func guardCredentialURL(flag, raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// Not echoed: an unparsable value may still carry a pasted secret.
		return fmt.Errorf("%s is not an http(s) URL", flag)
	}
	// Only scheme and host are ever echoed: Redacted() masks a password but
	// would still print a bare username, a query or a fragment, each a place
	// a token gets pasted by mistake.
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || strings.ContainsAny(raw, "?#") {
		return fmt.Errorf("%s %s://%s must not carry credentials, a query or a fragment", flag, u.Scheme, u.Host)
	}
	if u.Scheme == "https" || allowInsecure || isLoopbackHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("%s %s://%s is not https and not loopback; pass %s if this is a deliberate LAN/dev endpoint", flag, u.Scheme, u.Host, allowInsecureHTTPFlg)
}

// adminClient issues bearer-authenticated GETs against one admin base URL.
type adminClient struct {
	base    string
	secrets []string // tokenVariants(token), redacted from every body summary
	token   string
	client  *http.Client
	stderr  io.Writer
	noted   bool
}

// noRedirects: the admin API never redirects, and following one would
// re-send the bearer to wherever the Location points (Go keeps
// Authorization for the same host and its subdomains, scheme included).
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// adminTransport lets tests inject a RoundTripper — one that fails the test
// when the URL guard should have prevented any request; nil is the default.
var adminTransport http.RoundTripper

func newAdminClient(base, token string, stderr io.Writer) *adminClient {
	return &adminClient{base: base, token: token, secrets: tokenVariants(token), client: &http.Client{Timeout: adminRequestTimeout, CheckRedirect: noRedirects, Transport: adminTransport}, stderr: stderr}
}

// adminHTTPError is a non-2xx answer: the status for the caller's mapping
// (404, 409) and the sanitised body summary for the user.
type adminHTTPError struct {
	method, path string
	status       int
	summary      string
}

func (e *adminHTTPError) Error() string {
	if e.method == http.MethodGet {
		return fmt.Sprintf("%s: HTTP %d%s", e.path, e.status, e.summary)
	}
	return fmt.Sprintf("%s %s: HTTP %d%s", e.method, e.path, e.status, e.summary)
}

// getJSON fetches path and decodes a 200 body into out (out may be nil to
// check the status only).
func (c *adminClient) getJSON(ctx context.Context, path string, out any) (int, error) {
	return c.doJSON(ctx, http.MethodGet, path, nil, out)
}

// doJSON sends one authenticated request — a JSON body when body is non-nil —
// and decodes a 2xx answer into out when out is non-nil. Before the first
// authenticated request it prints scheme://host to stderr, as decision 3
// requires, never the token. A non-2xx answer is an *adminHTTPError whose
// summary never carries the token (bodySummary).
func (c *adminClient) doJSON(ctx context.Context, method, path string, body, out any) (int, error) {
	if !c.noted {
		if u, err := url.Parse(c.base); err == nil {
			_, _ = fmt.Fprintf(c.stderr, "kelvran: sending the admin token to %s\n", sanitizeCell(u.Scheme+"://"+u.Host))
		}
		c.noted = true
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, &adminHTTPError{method: method, path: path, status: resp.StatusCode, summary: bodySummary(resp.StatusCode, b, c.secrets)}
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s: decoding the response: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

// dataPlaneProbe answers doctor's --url checks without a credential: the
// status of GET /readyz and of a bearer-less GET /v1/models (expected 401).
func dataPlaneProbe(ctx context.Context, base, path string) (int, error) {
	status, _, err := dataPlaneGet(ctx, base, path)
	return status, err
}

// dataPlaneGet is one bearer-less GET against the data plane: the status and
// up to 1 MiB of body (status reads the /readyz summary), redirects not
// followed.
func dataPlaneGet(ctx context.Context, base, path string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := (&http.Client{Timeout: adminRequestTimeout, CheckRedirect: noRedirects, Transport: adminTransport}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// bodySummary is what of a non-200 admin body may reach a finding: nothing
// for 401/403 (the admin API's texts there are fixed strings, and a body is
// the one place a misbehaving server could echo the bearer), a redirect
// hint for 3xx, otherwise the first line (any Unicode line break ends it)
// with control characters removed and every secret variant replaced BEFORE
// the 120-rune cut, so a truncation cannot leave a prefix of the token
// behind. The caller also redacts the token from every detail.
func bodySummary(status int, body []byte, secrets []string) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ""
	case status >= 300 && status < 400:
		return " (a redirect, not followed — is this the admin listener?)"
	}
	line := strings.TrimSpace(string(body))
	if i := strings.IndexFunc(line, isLineBreak); i >= 0 {
		line = line[:i]
	}
	line = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, line)
	for _, s := range secrets {
		line = strings.ReplaceAll(line, s, "***")
	}
	if rs := []rune(line); len(rs) > 120 {
		line = string(rs[:120]) + "…"
	}
	if line == "" {
		return ""
	}
	return ": " + line
}

func isLineBreak(r rune) bool {
	return r == '\r' || r == '\n' || r == '\u0085' || r == '\u2028' || r == '\u2029'
}

// tokenVariants is the token as a server might echo it back: raw,
// URL-encoded (reserved characters only, and every byte), hex in both cases,
// and base64 in the standard and URL alphabets with and without padding;
// deduplicated, never empty strings.
func tokenVariants(token string) []string {
	if token == "" {
		return nil
	}
	var pct strings.Builder
	for _, b := range []byte(token) {
		_, _ = fmt.Fprintf(&pct, "%%%02X", b)
	}
	quoted := strconv.Quote(token)
	candidates := []string{
		token,
		quoted[1 : len(quoted)-1], // the spelling %q gives a token with a quote or backslash in it
		url.QueryEscape(token),
		pct.String(),
		hex.EncodeToString([]byte(token)),
		strings.ToUpper(hex.EncodeToString([]byte(token))),
		base64.StdEncoding.EncodeToString([]byte(token)),
		base64.RawStdEncoding.EncodeToString([]byte(token)),
		base64.URLEncoding.EncodeToString([]byte(token)),
		base64.RawURLEncoding.EncodeToString([]byte(token)),
		strings.ToUpper(token), // a server that case-folds what it echoes
		strings.ToLower(token),
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range candidates {
		if c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// redactedURL renders a configured URL for a finding: password replaced,
// query withheld, an unparsable value withheld whole (redactOneURL).
func redactedURL(raw string) string { return redactOneURL(raw) }
