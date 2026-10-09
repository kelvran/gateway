package cli

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ClientURLs derives the two base URLs a client pastes from the gateway's
// listen_addr: the Anthropic form (no path — Claude Code appends
// /v1/messages) and the OpenAI form (…/v1 — the OpenAI SDKs append
// /chat/completions). An empty or wildcard host (":8080", "0.0.0.0:8080",
// "[::]:8080") becomes 127.0.0.1, because "http://:8080" is not a URL a
// client accepts. init and connect share this one helper so their output
// can never drift (RFC-3 decision 6).
func ClientURLs(listenAddr string) (anthropicBase, openaiBase string, err error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", "", fmt.Errorf("listen_addr %q is not host:port: %w", listenAddr, err)
	}
	// net.SplitHostPort accepts any string as the port; the URLs built here
	// are printed into shell lines, so only a plain decimal port passes.
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strings.TrimLeft(port, "0123456789") != "" {
		return "", "", fmt.Errorf("listen_addr %q needs a numeric port between 1 and 65535", listenAddr)
	}
	switch strings.Trim(host, "[]") {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") { // a literal IPv6 address keeps its brackets
		host = "[" + strings.Trim(host, "[]") + "]"
	}
	base := "http://" + host + ":" + port
	return base, base + "/v1", nil
}
