package bench

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The gateway's default guardrail policy runs PII detectors over every
// prompt: a finding is a WARN log write per request for the warn-tier
// categories and a blocked request for the block tier (SSN). A benchmark
// prompt that trips one measures that, not the gateway — which is what
// happened when the run nonce was a 19-digit timestamp (the phone detector
// reads any ten digits as a number). internal/bench is a leaf and cannot
// import internal/guardrail, so the three default detectors whose patterns
// can match a bare digit run are copied here verbatim; TestPIIPatternsAre
// TheGatewaysOwn reads the guardrail sources so the copies cannot drift.
var piiPatterns = map[string]*regexp.Regexp{
	"phone.go":      regexp.MustCompile(`(?:\+?1[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}\b|\+[1-9]\d{7,14}\b`),
	"creditcard.go": regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`),
	"ssn.go":        regexp.MustCompile(`\b(?:[0-7][0-9]{2}|8[0-8][0-9])[-\s]?(?:[1-9][0-9])[-\s]?(?:[1-9][0-9]{3})\b`),
}

var mustCompileLiteral = regexp.MustCompile("regexp\\.MustCompile\\(`([^`]*)`\\)")

func TestPIIPatternsAreTheGatewaysOwn(t *testing.T) {
	for file, want := range piiPatterns {
		src, err := os.ReadFile(filepath.Join("..", "guardrail", file))
		if err != nil {
			t.Fatalf("reading the guardrail source: %v", err)
		}
		m := mustCompileLiteral.FindStringSubmatch(string(src))
		if m == nil {
			t.Fatalf("%s: no regexp.MustCompile(`…`) literal found", file)
		}
		if got := m[1]; got != want.String() {
			t.Errorf("%s: the gateway's pattern is\n  %s\nbut this test carries\n  %s\n— update the copy", file, got, want.String())
		}
	}
}

func TestPromptsDoNotTripTheDefaultPIIDetectors(t *testing.T) {
	rec := newPromptRecorder(false)
	srv := httptest.NewServer(rec)
	defer srv.Close()
	for _, pool := range []int{0, 5} {
		if _, err := Run(context.Background(), Config{Scenario: "pii", Target: srv.URL, Bearer: "x", Model: "bench", RPS: 40, Duration: 300 * time.Millisecond, Seed: 9, PromptPool: pool}); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.prompts) == 0 {
		t.Fatal("no prompts recorded")
	}
	for prompt := range rec.prompts {
		for file, re := range piiPatterns {
			if re.MatchString(prompt) {
				t.Errorf("prompt %q matches the gateway's %s detector", prompt, strings.TrimSuffix(file, ".go"))
			}
		}
	}
}

func TestRunNonceIsLettersOnly(t *testing.T) {
	for _, n := range []int64{0, 1, 35, 36, 1759929426833702000, 1<<62 + 12345} {
		got := lettersNonce(n)
		if got == "" || !regexp.MustCompile(`^[a-z]+$`).MatchString(got) {
			t.Errorf("lettersNonce(%d) = %q, want lower-case letters only", n, got)
		}
	}
	if lettersNonce(1) == lettersNonce(2) {
		t.Error("distinct inputs encode to the same nonce")
	}
}
