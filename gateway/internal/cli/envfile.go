package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// parseEnvFile reads a file in systemd's EnvironmentFile= grammar, the
// shape deploy/systemd/kelvran-gateway.service loads credentials from
// (/etc/kelvran-gateway/env) and the subset Docker's --env-file and
// Compose's env_file share when values are unquoted: blank lines and lines
// starting with # or ; are skipped, each entry is KEY=value, matching
// single or double quotes around the value are stripped, and a trailing
// backslash continues the value on the next line. Values are returned for
// presence checks only and are never printed by any caller (RFC-3
// decision 7).
func parseEnvFile(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var pending string
	continuing := false
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if continuing {
			pending += "\n" + line
		} else {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
				continue
			}
			pending = line
		}
		if strings.HasSuffix(pending, "\\") {
			pending = strings.TrimSuffix(pending, "\\")
			continuing = true
			continue
		}
		continuing = false
		key, value, ok := strings.Cut(pending, "=")
		key = strings.TrimSpace(key)
		if strings.HasPrefix(key, "export ") {
			// Neither systemd EnvironmentFile= nor Docker --env-file accepts a
			// shell `export`; the gateway would never see the variable.
			return nil, fmt.Errorf("line %d starts with `export`, which systemd EnvironmentFile= and Docker --env-file do not accept; write KEY=value", lineNo)
		}
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("line %d is not KEY=value", lineNo)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		out[key] = value
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if continuing {
		return nil, fmt.Errorf("line %d ends with a backslash but the file ends", lineNo)
	}
	return out, nil
}

// envSource answers "is this variable set and non-empty" for the gateway's
// environment as doctor can see it: the --env-file values (highest
// precedence) merged over the CLI's own process environment.
type envSource struct {
	files   map[string]string
	getenv  func(string) string
	hasFile bool // an --env-file (or the auto-detected one) was read
}

func (e envSource) isSet(name string) bool {
	if v, ok := e.files[name]; ok {
		return v != ""
	}
	return e.getenv(name) != ""
}

// value returns the variable's value for the two consumers that need one —
// the sk-ant-oat prefix test and the admin-token resolution; callers must
// never print it.
func (e envSource) value(name string) string {
	if v, ok := e.files[name]; ok {
		return v
	}
	return e.getenv(name)
}

// loadEnvFiles parses each path in order (later files win).
func loadEnvFiles(paths []string) (map[string]string, error) {
	merged := map[string]string{}
	for _, p := range paths {
		f, err := os.Open(p) //nolint:gosec // G304: the operator's own --env-file path
		if err != nil {
			return nil, fmt.Errorf("--env-file %s: %w", p, err)
		}
		vars, perr := parseEnvFile(f)
		_ = f.Close()
		if perr != nil {
			return nil, fmt.Errorf("--env-file %s: %w", p, perr)
		}
		for k, v := range vars {
			merged[k] = v
		}
	}
	return merged, nil
}
