package main

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/admin/auditstore"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

func TestResolveAuditSigningKeyReturnsTheKeyWhenTheVariableIsSet(t *testing.T) {
	var buf strings.Builder
	key, err := resolveAuditSigningKey(controlplane.AdminConfig{AuditLogPath: "/tmp/x.jsonl", AuditSigningSecretEnv: "AUDIT_TEST_KEY"},
		func(k string) string {
			if k == "AUDIT_TEST_KEY" {
				return "the-key-material"
			}
			return ""
		}, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil || string(key) != "the-key-material" {
		t.Fatalf("key = %q, err = %v; want the variable's value", key, err)
	}
	if strings.Contains(buf.String(), "admin_audit_unsigned") {
		t.Errorf("a configured key must not warn about an unsigned trail: %s", buf.String())
	}
}

func TestResolveAuditSigningKeyRefusesASetButEmptyVariable(t *testing.T) {
	_, err := resolveAuditSigningKey(controlplane.AdminConfig{AuditLogPath: "/tmp/x.jsonl", AuditSigningSecretEnv: "AUDIT_TEST_KEY"},
		func(string) string { return "" }, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
	if err == nil || !strings.Contains(err.Error(), "AUDIT_TEST_KEY") || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v; want a refusal naming the variable, like every other *_env secret that resolves empty", err)
	}
}

func TestResolveAuditSigningKeyWarnsWhenNoKeyIsConfigured(t *testing.T) {
	var buf strings.Builder
	key, err := resolveAuditSigningKey(controlplane.AdminConfig{AuditLogPath: "/tmp/x.jsonl"}, func(string) string { return "" }, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil || key != nil {
		t.Fatalf("key = %q, err = %v; want nil key and no error (chain without MAC)", key, err)
	}
	if !strings.Contains(buf.String(), "admin_audit_unsigned") || !strings.Contains(buf.String(), "audit_signing_secret_env") {
		t.Errorf("want an admin_audit_unsigned warning naming the key to set: %s", buf.String())
	}
}

func TestWarnOnUnsignedAuditHistory(t *testing.T) {
	cases := []struct {
		name    string
		resumed auditstore.Resumed
		keyed   bool
		warn    bool
	}{
		{"keyed, history without a mac", auditstore.Resumed{Lines: 4, LastSeq: 4}, true, true},
		{"keyed, legacy-only history", auditstore.Resumed{Lines: 4}, true, true},
		{"keyed, signed history", auditstore.Resumed{Lines: 4, LastSeq: 4, SawMAC: true}, true, false},
		{"keyed, new file", auditstore.Resumed{}, true, false},
		{"no key, history without a mac", auditstore.Resumed{Lines: 4, LastSeq: 4}, false, false},
	}
	for _, c := range cases {
		var buf strings.Builder
		warned := warnOnUnsignedAuditHistory(c.resumed, c.keyed, "/var/lib/kelvran/admin-audit.jsonl", slog.New(slog.NewTextHandler(&buf, nil)))
		if warned != c.warn || strings.Contains(buf.String(), "admin_audit_resumed_unsigned_history") != c.warn {
			t.Errorf("%s: warned = %v, log %q; want %v", c.name, warned, buf.String(), c.warn)
		}
		if c.warn && !strings.Contains(buf.String(), "lines=4") {
			t.Errorf("%s: the warning must carry the line count: %q", c.name, buf.String())
		}
	}
}
