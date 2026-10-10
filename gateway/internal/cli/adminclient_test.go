package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveAdminTokenFollowsDecisionThreeOrder(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "tok")
	if err := os.WriteFile(tokenFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"KELVRAN_ADMIN_TOKEN_FILE": tokenFile, "MY_ADMIN": "from-config-var", "KELVRAN_ADMIN_TOKEN": "from-default-var"}
	getenv := func(k string) string { return env[k] }

	if tok, src, err := resolveAdminToken(tokenFile, "MY_ADMIN", getenv); err != nil || tok != "from-file" || !strings.Contains(src, "file") {
		t.Errorf("--admin-token-file first: %q %q %v", tok, src, err)
	}
	if tok, _, err := resolveAdminToken("", "MY_ADMIN", getenv); err != nil || tok != "from-file" {
		t.Errorf("KELVRAN_ADMIN_TOKEN_FILE second: %q %v", tok, err)
	}
	delete(env, "KELVRAN_ADMIN_TOKEN_FILE")
	if tok, src, err := resolveAdminToken("", "MY_ADMIN", getenv); err != nil || tok != "from-config-var" || !strings.Contains(src, "MY_ADMIN") {
		t.Errorf("admin.token_env third: %q %q %v", tok, src, err)
	}
	if tok, src, err := resolveAdminToken("", "", getenv); err != nil || tok != "from-default-var" || src != "KELVRAN_ADMIN_TOKEN" {
		t.Errorf("KELVRAN_ADMIN_TOKEN fourth: %q %q %v", tok, src, err)
	}
	if tok, src, err := resolveAdminToken("", "", func(string) string { return "" }); err != nil || tok != "" || src != "" {
		t.Errorf("none: %q %q %v", tok, src, err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveAdminToken(empty, "", getenv); err == nil {
		t.Error("an empty token file must be an error, not a silent fall-through")
	}
}

func TestResolveAdminURLGuardsCredentialCarryingURLs(t *testing.T) {
	none := func(string) string { return "" }
	for _, ok := range []string{"", "http://127.0.0.1:8081", "http://localhost:8081/", "http://[::1]:8081", "https://admin.example"} {
		if _, err := resolveAdminURL(ok, false, none); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
	if u, _ := resolveAdminURL("", false, none); u != "http://127.0.0.1:8081" {
		t.Errorf("default = %q", u)
	}
	if u, _ := resolveAdminURL("http://localhost:8081/", false, none); u != "http://localhost:8081" {
		t.Errorf("trailing slash must be trimmed: %q", u)
	}
	_, err := resolveAdminURL("http://203.0.113.1:8081", false, none)
	if err == nil || !strings.Contains(err.Error(), "--admin-url http://203.0.113.1:8081 is not https and not loopback; pass --allow-insecure-http") {
		t.Errorf("TEST-NET http must be refused with decision 3's wording: %v", err)
	}
	if _, err := resolveAdminURL("http://203.0.113.1:8081", true, none); err != nil {
		t.Errorf("--allow-insecure-http must lift the refusal: %v", err)
	}
	_, err = resolveAdminURL("", false, func(k string) string {
		if k == "KELVRAN_ADMIN_URL" {
			return "http://203.0.113.1:8081"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "KELVRAN_ADMIN_URL http://203.0.113.1:8081 is not https") {
		t.Errorf("the env-var URL must be guarded identically and named: %v", err)
	}
	if _, err := resolveAdminURL("ftp://x", true, none); err == nil {
		t.Error("a non-http scheme must be refused even with --allow-insecure-http")
	}
}

func TestAdminClientSendsTheBearerAndNotesTheHostOnce(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/admin/config":
			_, _ = w.Write([]byte(`{"ListenAddr":":8080"}`))
		default:
			http.Error(w, "nope", http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	var errb strings.Builder
	c := newAdminClient(srv.URL, "tok-one", &errb)
	var out struct{ ListenAddr string }
	if code, err := c.getJSON(context.Background(), "/admin/config", &out); err != nil || code != 200 || out.ListenAddr != ":8080" {
		t.Fatalf("getJSON: %d %v %+v", code, err, out)
	}
	if code, err := c.getJSON(context.Background(), "/admin/other", nil); code != 401 || err == nil {
		t.Errorf("non-200 must surface as an error with the status: %d %v", code, err)
	}
	if len(seen) != 2 || seen[0] != "Bearer tok-one" || seen[1] != "Bearer tok-one" {
		t.Errorf("bearer not sent: %v", seen)
	}
	if strings.Count(errb.String(), "sending the admin token to") != 1 || strings.Contains(errb.String(), "tok-one") {
		t.Errorf("the host note must appear once and never the token: %q", errb.String())
	}
}

func TestBodySummaryKeepsOnlyTheFirstLineAndRedactsBeforeTheCut(t *testing.T) {
	for _, body := range []string{"first\nsecond", "first\r\nsecond", "first\u2028second", "first\u0085second", "first\u2029second"} {
		if got := bodySummary(400, []byte(body), nil); got != ": first" {
			t.Errorf("bodySummary(%q) = %q, want %q", body, got, ": first")
		}
	}
	if got := bodySummary(401, []byte("anything"), nil); got != "" {
		t.Errorf("401 must be status-only, got %q", got)
	}
	// A secret that starts inside the 120-rune window and ends past it is replaced whole, never cut to a prefix.
	secret := strings.Repeat("s", 40)
	got := bodySummary(500, []byte(strings.Repeat("x", 110)+secret), tokenVariants(secret))
	if strings.Contains(got, "ss") || !strings.Contains(got, "***") {
		t.Errorf("the secret must be redacted before truncation: %q", got)
	}
	if v := tokenVariants("tok+en"); len(v) != 7 || v[0] != "tok+en" || v[1] != "tok%2Ben" || v[2] != "%74%6F%6B%2B%65%6E" || v[3] != "746f6b2b656e" || v[4] != "746F6B2B656E" || v[6] != "TOK+EN" {
		t.Errorf("tokenVariants = %q (the six-byte token needs no base64 padding, so the Raw and padded alphabets deduplicate to two; the upper-cased spelling is last, the lower-cased one deduplicates with the raw token)", v)
	}
	if v := tokenVariants(`to"ken`); !strings.Contains(strings.Join(v, "|"), `to\"ken`) {
		t.Errorf("the Go-quoted spelling %%q produces must be a variant: %q", v)
	}
	if tokenVariants("") != nil {
		t.Error("an empty token has no variants")
	}
}
