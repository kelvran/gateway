package cli

import (
	"fmt"
	"strings"
)

// yamlBuilder renders the subset gateway/internal/gateway/controlplane's
// parseYAMLMini reads — block mappings only, two-space indents, double-quoted
// strings, bare numbers and booleans, comments on their own lines — and
// refuses anything the parser would silently misread (RFC-3 fact 5):
// a value containing `"`, `#` or a newline (stripYAMLComment runs before
// quote handling and unquoteYAMLScalar has no escapes) or a key the parser
// could not split at its colon.
type yamlBuilder struct {
	b   strings.Builder
	err error
}

func indent(n int) string { return strings.Repeat("  ", n) }

// comment writes one `# text` line; multi-line text becomes one line each.
func (y *yamlBuilder) comment(level int, text string) {
	for _, line := range strings.Split(text, "\n") {
		y.b.WriteString(indent(level) + "# " + line + "\n")
	}
}

func (y *yamlBuilder) blank() { y.b.WriteString("\n") }

// section opens a mapping: `key:`.
func (y *yamlBuilder) section(level int, key string) {
	if err := checkYAMLKey(key); err != nil {
		y.fail(err)
		return
	}
	y.b.WriteString(indent(level) + key + ":\n")
}

// str writes `key: "value"`.
func (y *yamlBuilder) str(level int, key, value string) {
	if err := checkYAMLKey(key); err != nil {
		y.fail(err)
		return
	}
	if strings.ContainsAny(value, "\"#\n\r") {
		// The value is not echoed: it may be a URL or an id a user pasted a
		// credential into by mistake.
		y.fail(fmt.Errorf("the value for %s contains a quote, a #, or a line break, which the config parser cannot read inside a quoted string", key))
		return
	}
	y.b.WriteString(indent(level) + key + ": \"" + value + "\"\n")
}

// raw writes `key: value` for a bare number or boolean.
func (y *yamlBuilder) raw(level int, key, value string) {
	if err := checkYAMLKey(key); err != nil {
		y.fail(err)
		return
	}
	if strings.ContainsAny(value, " \"#:\n\r") || value == "" {
		y.fail(fmt.Errorf("the bare value for %s must be a number or a boolean", key))
		return
	}
	y.b.WriteString(indent(level) + key + ": " + value + "\n")
}

func (y *yamlBuilder) fail(err error) {
	if y.err == nil {
		y.err = err
	}
}

func (y *yamlBuilder) String() (string, error) { return y.b.String(), y.err }

// checkYAMLKey accepts the key shapes the parser splits unambiguously: no
// colon, hash, quote, whitespace or control character, non-empty.
func checkYAMLKey(key string) error {
	if key == "" || strings.ContainsAny(key, ":#\"' \t\n\r") {
		return fmt.Errorf("%q cannot be a config key in the gateway's YAML subset", key)
	}
	return nil
}

// deploymentName derives a config key from a provider and a model id:
// `<provider>-<model>` with every byte outside [A-Za-z0-9._-] replaced by
// `-`, so a model id such as `gpt-4o` or `anthropic.claude-sonnet-5-5`
// yields a key the parser reads back unchanged.
func deploymentName(provider, model string) string {
	var sb strings.Builder
	sb.WriteString(provider)
	sb.WriteByte('-')
	for _, r := range model {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteByte('-')
		}
	}
	return sb.String()
}
