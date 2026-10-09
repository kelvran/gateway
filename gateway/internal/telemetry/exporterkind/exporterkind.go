// Package exporterkind is the telemetry exporter-name vocabulary — the
// three values config.yaml's telemetry.exporter accepts — as a stdlib-only
// leaf. telemetry.Init switches on these constants and reports unknown
// names with WantList, and the kelvran CLI's doctor validates a config's
// exporter through Valid without importing package telemetry, which would
// pull OpenTelemetry and its init-time work into a diagnostic tool (RFC-3,
// docs/rfcs/2026-10-09-gateway-kelvran-cli-and-single-user-mode.md,
// decision 7). Adding an exporter means adding it here first: a name absent
// from Names is unreachable by configuration.
package exporterkind

import "strings"

const (
	// Stdout prints spans and a periodic metrics dump as JSON on the
	// gateway's own stdout, interleaved with its logs — the default when
	// telemetry.exporter is empty or the section is absent.
	Stdout = "stdout"
	// OTLP ships spans and metrics over OTLP/HTTP to telemetry.otlp_endpoint.
	OTLP = "otlp"
	// None installs no exporter at all.
	None = "none"
)

// Names lists every accepted exporter, in the order the documentation and
// the startup error quote them.
var Names = []string{Stdout, OTLP, None}

// Valid reports whether name is an accepted exporter. The empty string is
// valid because it means "default to stdout" (telemetry.Init). Matching is
// exact: no case folding, no trimming, the same comparison Init makes.
func Valid(name string) bool {
	if name == "" {
		return true
	}
	for _, n := range Names {
		if name == n {
			return true
		}
	}
	return false
}

// WantList renders Names as the quoted, comma-separated phrase the startup
// error and docs/how-to/troubleshooting.md use: `"stdout", "otlp", or "none"`.
func WantList() string {
	quoted := make([]string, len(Names))
	for i, n := range Names {
		quoted[i] = `"` + n + `"`
	}
	if len(quoted) == 1 {
		return quoted[0]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + ", or " + quoted[len(quoted)-1]
}
