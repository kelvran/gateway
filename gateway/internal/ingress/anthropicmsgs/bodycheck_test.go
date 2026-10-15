package anthropicmsgs

import (
	"errors"
	"strings"
	"testing"
)

// TestCheckBodyIsExportedWithTheDetail: the OpenAI decode paths call CheckBody
// and format its detail behind their own prefix, so the error must carry the
// detail apart from the sentinel text while errors.Is(ErrInvalidBody) and the
// full message the Anthropic handler relays stay as they were.
func TestCheckBodyIsExportedWithTheDetail(t *testing.T) {
	cases := map[string]struct{ body, detail string }{
		"duplicate at the top level": {`{"a":1,"a":2}`, `the body has a duplicate member "a"`},
		"duplicate nested":           {`{"a":[{"b":1,"b":2}]}`, `/a/0 has a duplicate member "b"`},
		"invalid UTF-8":              {"{\"a\":\"\xff\"}", "not valid UTF-8"},
		"too deep":                   {"{\"a\":" + strings.Repeat("[", 64) + strings.Repeat("]", 64) + "}", "nests deeper than 64 levels"},
		"truncated inside a string":  {`{"a":"abc`, "unexpected EOF"},
	}
	// A mid-stream syntax error is the decoder's own text, bounded by describe;
	// only its prefix is pinned because the wording is the toolchain's (Go 1.27
	// moved encoding/json onto the v2 implementation, which may reword errors).
	if err := CheckBody([]byte(`{"a":1,}`)); err == nil {
		t.Errorf("syntax error: CheckBody accepted the body")
	} else {
		var be *BodyError
		if !errors.As(err, &be) || !strings.HasPrefix(be.Detail, "invalid character") {
			t.Errorf("syntax error: err = %v, want a *BodyError whose Detail starts with the decoder's `invalid character`", err)
		}
	}
	for name, c := range cases {
		err := CheckBody([]byte(c.body))
		if err == nil {
			t.Errorf("%s: CheckBody accepted the body", name)
			continue
		}
		if !errors.Is(err, ErrInvalidBody) {
			t.Errorf("%s: errors.Is(err, ErrInvalidBody) is false for %v", name, err)
		}
		var be *BodyError
		if !errors.As(err, &be) {
			t.Errorf("%s: %T is not a *BodyError", name, err)
			continue
		}
		if be.Detail != c.detail {
			t.Errorf("%s: Detail %q, want %q", name, be.Detail, c.detail)
		}
		if want := ErrInvalidBody.Error() + ": " + c.detail; err.Error() != want {
			t.Errorf("%s: Error() %q, want %q (the message the Anthropic handler relays)", name, err.Error(), want)
		}
	}
	// A non-object body is object()'s business on /v1/messages and encoding/json's
	// on the OpenAI routes; a body truncated between tokens ends in EOF, which the
	// token walker takes as the end, and encoding/json names it a step later
	// (one cut inside a string, literal or number is refused above as
	// `unexpected EOF`); an out-of-range
	// number literal is not parsed by the walk (UseNumber) and is the typed
	// decoder's business -- none is the check's to refuse.
	for name, body := range map[string]string{"object": `{"a":[1,2],"b":{"c":"d"}}`, "bare array": `[1,2]`, "scalar": `1`, "truncated between tokens": `{"a":`, "out-of-range number": `{"a":1e999}`} {
		if err := CheckBody([]byte(body)); err != nil {
			t.Errorf("%s: CheckBody refused %q: %v", name, body, err)
		}
	}
}
