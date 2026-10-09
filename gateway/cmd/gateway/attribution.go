package main

// Zero-client-config attribution (plan item 13a), per
// docs/rfcs/2026-10-09-gateway-attribution-and-spend-ledger.md: one
// mux-level middleware reads the User-Agent and x-claude-code-* request
// headers, caps and validates every value, and attaches a
// telemetry.Attribution to the request context. Nothing here is
// authenticated or admitting — the headers are client-supplied, the same
// trust class as the agent_run_id Baggage member, and are consumed for
// attribution only ("the rest the gateway may consume for routing,
// attribution, and tracing, and need not forward" — Anthropic's Claude Code
// gateway-protocol page).

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

const (
	// maxUserAgentTokenBytes bounds the first whitespace-delimited token of
	// User-Agent before it is classified; a longer token is "other".
	maxUserAgentTokenBytes = 64
	// maxIdentifierBytes bounds every x-claude-code-* identifier (session,
	// agent, parent agent, prompt id). A longer or out-of-alphabet value is
	// dropped — never truncated into a different identifier.
	maxIdentifierBytes = 128
	// maxAgentTypeBytes bounds x-claude-code-agent-type, an open vocabulary
	// kept as a span attribute only.
	maxAgentTypeBytes = 64
	// maxPrevToolEntries and maxPrevToolBytes are the protocol page's own
	// caps for x-claude-code-prev-tool-durations (32 entries, 4 KB, first
	// entries kept).
	maxPrevToolEntries = 32
	maxPrevToolBytes   = 4096
	// maxPrevToolEntryMS bounds one entry's duration at one day. Durations
	// are client-supplied; without a per-entry cap two MaxInt64 entries
	// summed to a negative kelvran.claude_code.prev_tool_total_ms.
	maxPrevToolEntryMS = 24 * 60 * 60 * 1000

	headerSessionID         = "x-claude-code-session-id"
	headerAgentID           = "x-claude-code-agent-id"
	headerParentAgentID     = "x-claude-code-parent-agent-id"
	headerPromptID          = "x-claude-code-prompt-id"
	headerRequestClass      = "x-claude-code-request-class"
	headerAgentType         = "x-claude-code-agent-type"
	headerCompaction        = "x-claude-code-compaction"
	headerContextCompacted  = "x-claude-code-context-compacted"
	headerPrevToolDurations = "x-claude-code-prev-tool-durations"
)

// requestClasses and compactionValues are the protocol page's closed
// vocabularies; anything else normalises to "other".
var (
	requestClasses   = map[string]bool{"main": true, "subagent": true, "workflow": true, "compaction": true, "auxiliary": true}
	compactionValues = map[string]bool{"auto": true, "manual": true, "reactive": true}
)

// clientToolFromUserAgent normalises a User-Agent header into the closed
// kelvran.client.tool vocabulary. The first whitespace-delimited token is
// split at its first "/" into a product and a trailer. The product is
// matched case-insensitively after stripping a leading "async" and then a
// leading "azure" (the Stainless-generated Python SDKs send
// "{ClassName}/Python {version}": OpenAI, AsyncOpenAI, AzureOpenAI,
// AsyncAzureOpenAI; Anthropic, AsyncAnthropic, AnthropicBedrock,
// AnthropicVertex, AnthropicAWS, AnthropicGoogleCloud,
// AnthropicBedrockMantle and their Async forms — verified against openai
// 3.14.1 and anthropic 1.6.0). For those SDKs the trailer's first word
// selects the language. Single-token tools match by product alone. Anything
// unknown, over the cap, non-ASCII or non-printable is "other"; the raw
// token is never stored (the gateway.http server span already carries
// user_agent.original).
func clientToolFromUserAgent(ua string) string {
	ua = strings.TrimSpace(ua)
	token := ua
	if i := strings.IndexAny(ua, " \t"); i >= 0 {
		token = ua[:i]
	}
	if token == "" || len(token) > maxUserAgentTokenBytes || !isPrintableASCII(token) {
		return telemetry.ClientToolOther
	}
	product, trailer := token, ""
	if i := strings.IndexByte(token, '/'); i >= 0 {
		product, trailer = token[:i], token[i+1:]
	}
	product = strings.ToLower(product)
	product = strings.TrimPrefix(product, "async")
	product = strings.TrimPrefix(product, "azure")
	lang := strings.ToLower(trailer)
	if i := strings.IndexAny(lang, " \t/("); i >= 0 {
		lang = lang[:i]
	}
	// The trailer of the Stainless SDKs is "<Language> <version>"; the
	// first whitespace-delimited word after the slash is the language. For
	// a User-Agent such as "OpenAI/Python 3.14.1" the token ends at the
	// space, so the language is the whole trailer.
	switch {
	case strings.HasPrefix(product, "openai"):
		return stainlessTool(lang, telemetry.ClientToolOpenAIPython, telemetry.ClientToolOpenAINode, telemetry.ClientToolOpenAIGo)
	case strings.HasPrefix(product, "anthropic"):
		return stainlessTool(lang, telemetry.ClientToolAnthropicPython, telemetry.ClientToolAnthropicNode, telemetry.ClientToolAnthropicGo)
	case product == "claude-cli" || product == "claude-code":
		return telemetry.ClientToolClaudeCode
	case product == "codex":
		return telemetry.ClientToolCodex
	case product == "aider":
		return telemetry.ClientToolAider
	case product == "continue":
		return telemetry.ClientToolContinue
	case product == "litellm":
		return telemetry.ClientToolLiteLLM
	case product == "curl":
		return telemetry.ClientToolCurl
	}
	return telemetry.ClientToolOther
}

// stainlessTool picks the language-specific tool for a Stainless SDK
// product from the trailer's first word; an unknown or missing language is
// "other" (a bare product without a trailer is not a verified wire value).
func stainlessTool(lang, python, node, golang string) string {
	switch lang {
	case "python":
		return python
	case "js", "javascript", "node", "typescript":
		return node
	case "go", "golang":
		return golang
	}
	return telemetry.ClientToolOther
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// isIdentifier reports whether v satisfies the identifier grammar: 1 to
// maxIdentifierBytes bytes of [A-Za-z0-9._:-]. The protocol page documents
// only -prompt-id's shape (a UUID), so the alphabet is deliberately
// narrow; a value outside it is dropped and counted, never stored.
func isIdentifier(v string) bool {
	if v == "" || len(v) > maxIdentifierBytes {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// parseAttributionHeaders builds the Attribution for one request and
// returns the lower-case names of the headers it dropped for violating a
// cap or grammar (vocabulary violations are normalised to "other", not
// dropped). Absent headers leave their fields empty.
func parseAttributionHeaders(h http.Header) (telemetry.Attribution, []string) {
	var a telemetry.Attribution
	var dropped []string
	a.ClientTool = clientToolFromUserAgent(h.Get("User-Agent"))

	ident := func(name string) string {
		v := h.Get(name)
		if v == "" {
			return ""
		}
		if !isIdentifier(v) {
			dropped = append(dropped, name)
			return ""
		}
		return v
	}
	a.SessionID = ident(headerSessionID)
	a.AgentID = ident(headerAgentID)
	a.ParentAgentID = ident(headerParentAgentID)
	a.ClaudeCodePromptID = ident(headerPromptID)

	vocab := func(name string, allowed map[string]bool, other string) string {
		v := h.Get(name)
		if v == "" {
			return ""
		}
		if allowed[v] {
			return v
		}
		return other
	}
	a.RequestClass = vocab(headerRequestClass, requestClasses, telemetry.RequestClassOther)
	a.Compaction = vocab(headerCompaction, compactionValues, "other")
	a.ContextCompacted = vocab(headerContextCompacted, compactionValues, "other")

	if v := h.Get(headerAgentType); v != "" {
		if len(v) <= maxAgentTypeBytes && isPrintableASCII(v) {
			a.AgentType = v
		} else {
			dropped = append(dropped, headerAgentType)
		}
	}

	if v := h.Get(headerPrevToolDurations); v != "" {
		raw, count, total := parsePrevToolDurations(v)
		if count == 0 {
			dropped = append(dropped, headerPrevToolDurations)
		} else {
			a.PrevToolDurations, a.PrevToolCount, a.PrevToolTotalMS = raw, count, total
		}
	}
	return a, dropped
}

// parsePrevToolDurations parses "<name>=<ms>;<name>=<ms>" per the protocol
// page: walk the value entry by entry with strings.Cut — never an eager
// Split, so no slice proportional to the raw header is allocated (the value
// is client-supplied, read before authentication on every route, and may be
// as large as http.Server's 1 MiB header cap) — and stop at the caps: at
// most maxPrevToolEntries entries and maxPrevToolBytes of the raw value, cut
// at an entry boundary. Malformed entries are skipped (prevToolEntryMS).
// Returns the kept raw value (the wire form of the kept entries), the entry
// count and the total milliseconds; the total cannot wrap because each
// entry is bounded by maxPrevToolEntryMS.
func parsePrevToolDurations(v string) (raw string, count, totalMS int) {
	var kept []string
	keptBytes := 0
	rest := v
	for count < maxPrevToolEntries {
		entry, after, found := strings.Cut(rest, ";")
		rest = after
		if n, ok := prevToolEntryMS(entry); ok {
			next := keptBytes + len(entry)
			if len(kept) > 0 {
				next++ // the ";" separator
			}
			if next > maxPrevToolBytes {
				break
			}
			kept = append(kept, entry)
			keptBytes = next
			count++
			totalMS += n
		}
		if !found {
			break
		}
	}
	return strings.Join(kept, ";"), count, totalMS
}

// prevToolEntryMS validates one "<name>=<ms>" entry and returns its
// duration. Well-formed means: printable ASCII throughout, a non-empty
// percent-decodable name, and a whole-millisecond duration in
// [0, maxPrevToolEntryMS]. The printable-ASCII check is the one that keeps
// the span exportable: a conforming entry is always printable ASCII (names
// are percent-encoded), Go's server passes 0x80-0xFF header bytes through
// to handlers, and the kept entries become the
// kelvran.claude_code.prev_tool_durations span attribute — a string with
// invalid UTF-8 there makes the OTLP exporter's proto.Marshal reject the
// whole batch, so one unauthenticated request could have dropped every
// span exported alongside it.
func prevToolEntryMS(entry string) (int, bool) {
	if !isPrintableASCII(entry) {
		return 0, false
	}
	name, ms, ok := strings.Cut(entry, "=")
	if !ok || name == "" {
		return 0, false
	}
	if _, err := url.PathUnescape(name); err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(ms)
	if err != nil || n < 0 || n > maxPrevToolEntryMS {
		return 0, false
	}
	return n, true
}

// captureAttribution is the mux-level middleware: it parses the
// attribution headers once per request, counts every dropped header on
// kelvran.attribution.dropped, blanks the identifier fields when
// captureIDs is false (the global attribution.capture_ids switch; the
// per-key switch is applied in the dataplane where the virtual key is
// known), and hands the Attribution to the handlers through the request
// context. Never a Baggage member: attribution must not leave the gateway.
func captureAttribution(captureIDs bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, dropped := parseAttributionHeaders(r.Header)
		for _, name := range dropped {
			telemetry.RecordAttributionDropped(r.Context(), name)
		}
		if !captureIDs {
			a = a.WithoutIdentifiers()
		}
		next.ServeHTTP(w, r.WithContext(telemetry.WithAttribution(r.Context(), a)))
	})
}

// dataPlaneHandler is the data server's full handler chain — in-flight
// tracking, attribution capture, the otelhttp server span, then the mux —
// built here so run() and the tests that exercise the chain use one
// definition. Attribution sits inside trackInFlight and outside the span
// wrapper so every route, present or future, is attributed exactly once.
func dataPlaneHandler(mux http.Handler, inFlight *sync.WaitGroup, captureIDs bool) http.Handler {
	return trackInFlight(inFlight, captureAttribution(captureIDs, wrapHTTPServerSpan(mux)))
}
