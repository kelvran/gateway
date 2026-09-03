package guardrail

import (
	"context"
	"net"
	"regexp"
)

// ipCandidatePattern is a coarse pre-filter (IPv4 dotted-quad shape, or
// an IPv6-shaped run of hex groups/colons) — net.ParseIP does the real
// validation, so this pattern only needs to avoid scanning every
// character of the input with net.ParseIP; it does not need to be
// byte-precise about valid octet ranges itself.
var ipCandidatePattern = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b|\b[0-9A-Fa-f:]{2,39}:[0-9A-Fa-f:]{2,39}\b`)

// IPAddressDetector detects real, valid IPv4/IPv6 addresses —
// CategoryNetworkID (Warn tier): a real but lower-stakes identifier.
// Uses net.ParseIP (stdlib) for real validation rather than a
// hand-rolled octet-range regex, which would either be wrong at the
// edges or need to reimplement IPv6's own compressed-notation rules.
type IPAddressDetector struct{}

func (IPAddressDetector) Name() string       { return "ipaddress" }
func (IPAddressDetector) Category() Category { return CategoryNetworkID }
func (IPAddressDetector) Detect(_ context.Context, text string) ([]Finding, error) {
	var findings []Finding
	for _, loc := range ipCandidatePattern.FindAllStringIndex(text, -1) {
		candidate := text[loc[0]:loc[1]]
		if net.ParseIP(candidate) != nil {
			findings = append(findings, Finding{Category: CategoryNetworkID, Detector: "ipaddress", Start: loc[0], End: loc[1]})
		}
	}
	return findings, nil
}
