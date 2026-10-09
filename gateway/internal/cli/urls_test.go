package cli

import "testing"

func TestClientURLsReplaceEmptyAndWildcardHostsWithLoopback(t *testing.T) {
	cases := []struct{ in, anthropic, openai string }{
		{"127.0.0.1:8080", "http://127.0.0.1:8080", "http://127.0.0.1:8080/v1"},
		{":8080", "http://127.0.0.1:8080", "http://127.0.0.1:8080/v1"},
		{":9000", "http://127.0.0.1:9000", "http://127.0.0.1:9000/v1"},
		{"0.0.0.0:9000", "http://127.0.0.1:9000", "http://127.0.0.1:9000/v1"},
		{"[::]:8080", "http://127.0.0.1:8080", "http://127.0.0.1:8080/v1"},
		{"gateway.internal:8080", "http://gateway.internal:8080", "http://gateway.internal:8080/v1"},
		{"[fd00::1]:8080", "http://[fd00::1]:8080", "http://[fd00::1]:8080/v1"},
	}
	for _, c := range cases {
		a, o, err := ClientURLs(c.in)
		if err != nil {
			t.Errorf("ClientURLs(%q): %v", c.in, err)
			continue
		}
		if a != c.anthropic || o != c.openai {
			t.Errorf("ClientURLs(%q) = (%q, %q), want (%q, %q)", c.in, a, o, c.anthropic, c.openai)
		}
	}
}

func TestClientURLsRejectAnAddressWithoutAPort(t *testing.T) {
	for _, in := range []string{"", "127.0.0.1", "localhost", "127.0.0.1:", "127.0.0.1:$(id)", "127.0.0.1:http", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:+8080", "127.0.0.1:80 80"} {
		if _, _, err := ClientURLs(in); err == nil {
			t.Errorf("ClientURLs(%q) = nil error, want one (the port must be a plain number)", in)
		}
	}
}
