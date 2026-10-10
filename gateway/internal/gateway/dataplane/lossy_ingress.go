package dataplane

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// Item 11 slice S9b, the dataplane half of the Anthropic Messages ingress
// (docs/rfcs/2026-10-09-gateway-anthropic-messages-ingress.md §3, §6, §8;
// docs/rfcs/2026-10-10-gateway-owner-gate-decisions.md Q5). Everything here
// reads adapter.ChatRequest.Passthrough, which only the /v1/messages handler
// (slice S10a) sets, so none of it changes a request from the OpenAI route.

// ErrLossyIngressRejected is returned before any upstream call when an
// Anthropic Messages request carries members the canonical shadow cannot
// express (Passthrough.UnknownFields) and the deployment finally picked for
// it is neither an anthropic deployment (which can carry the body as
// received) nor one the operator marked accept_lossy_anthropic_ingress.
// Decided from the request and the routing table alone, so cmd/gateway
// answers 400 with code lossy_ingress_rejected and the pointers in param.
//
// The message is a fixed sentence (gate-decisions Q5): it names the config
// key and the client-side remedy, names an unknown member only when the
// WHOLE top-level value is unknown (a one-segment pointer), and only counts
// members nested under a known field -- so /system/0/foo or
// /thinking/display never put "system" or "thinking" into the text, which
// Claude Code matches on for its own recovery paths. A top-level member is
// named only when its name is a plain identifier free of those strings
// (nameableMember); any other is counted and left to param, so a body
// cannot smuggle a recovery string into the message through a member
// called cache_control or system_prompt. It never names the deployment.
type ErrLossyIngressRejected struct {
	// TopLevelFields counts the one-segment pointers (the message quotes the
	// nameable ones and counts the rest, see Error); NestedCount the deeper
	// ones (counted only).
	TopLevelFields int
	NestedCount    int
	// Pointers lists every unknown member's RFC 6901 pointer, sorted; the
	// envelope carries them comma-joined in param, through Param.
	Pointers []string
}

func (e *ErrLossyIngressRejected) Error() string {
	var named []string
	unnamed, nested := 0, 0
	for _, ptr := range e.Pointers {
		switch name := topLevelName(ptr); {
		case !isTopLevelPointer(ptr):
			nested++
		case nameableMember(name):
			named = append(named, name)
		default:
			unnamed++
		}
	}
	var clauses []string
	if len(named) > 0 {
		clauses = append(clauses, plural(len(named), "member")+" this gateway cannot translate for the deployment that would serve it ("+boundedJoin(named)+")")
	}
	if unnamed > 0 {
		clauses = append(clauses, plural(unnamed, "member")+" named only in param")
	}
	if nested > 0 {
		clauses = append(clauses, plural(nested, "nested member")+" (listed in param)")
	}
	if len(clauses) == 0 {
		clauses = append(clauses, "members this gateway cannot translate for the deployment that would serve it")
	}
	return "dataplane: request carries " + joinClauses(clauses) +
		"; set accept_lossy_anthropic_ingress: true on that deployment to drop them, or start Claude Code with CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1"
}

// claudeCodeRecoveryStrings are the substrings gate-decisions Q5 forbids in
// the message: Claude Code matches on them to decide its own recovery
// (disable thinking, strip cache_control, reshape system content, ...), so
// a 400 that contained one by accident would make the client degrade its
// request and retry into the same 400. Compared case-insensitively.
var claudeCodeRecoveryStrings = []string{
	"thinking", "cache_control", "system", "output_config.effort",
	"extra inputs are not permitted", "input tag", "bound to a different conversation", "capability_rejected:",
}

// nameableMemberRe is the shape of a member name the message may quote: a
// short plain identifier (letters, digits, "_", ".", "-"), so spaces,
// quotes, control characters and anything else a body could carry never
// reach the sentence.
var nameableMemberRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// nameableMember reports whether a top-level member name may appear in the
// message: it matches nameableMemberRe and contains none of
// claudeCodeRecoveryStrings.
func nameableMember(name string) bool {
	if !nameableMemberRe.MatchString(name) {
		return false
	}
	lower := strings.ToLower(name)
	for _, s := range claudeCodeRecoveryStrings {
		if strings.Contains(lower, s) {
			return false
		}
	}
	return true
}

// joinClauses joins with ", " and a final " and ".
func joinClauses(clauses []string) string {
	if len(clauses) <= 1 {
		return strings.Join(clauses, "")
	}
	return strings.Join(clauses[:len(clauses)-1], ", ") + " and " + clauses[len(clauses)-1]
}

// Param is the envelope's param member: every pointer, sorted and
// comma-joined, cut like dropped_fields when a body carries more than
// maxDroppedFieldsBytes of them -- the pointers are client input echoed
// back, so they are bounded the same way everywhere they leave the process.
func (e *ErrLossyIngressRejected) Param() string {
	return boundedJoin(e.Pointers)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// isTopLevelPointer reports whether ptr addresses a whole top-level member:
// one segment, so "/foo" but not "/system/0/foo".
func isTopLevelPointer(ptr string) bool {
	return strings.HasPrefix(ptr, "/") && !strings.Contains(ptr[1:], "/")
}

// topLevelName is the member name of a one-segment pointer, unescaped per
// RFC 6901 (~1 is "/", ~0 is "~"; ~1 is decoded first so the escaped
// "~01" becomes the literal "~1" rather than "/").
func topLevelName(ptr string) string {
	name := strings.ReplaceAll(strings.TrimPrefix(ptr, "/"), "~1", "/")
	return strings.ReplaceAll(name, "~0", "~")
}

// lossyIngressIneligible reports whether dep would have to DROP members of
// req to serve it: the request came through the Anthropic Messages ingress
// with members the shadow cannot express, dep is not an anthropic
// deployment, and the operator did not accept the loss for dep. RFC-1 §6
// makes lossiness a routing property: capabilityOKForRequest folds this in,
// so rerouteToCapableDeploymentIfNeeded and attemptFallbackChain both skip
// such a deployment, and checkLossyIngressEligible turns "no eligible
// deployment anywhere in the pool" into the 400.
func lossyIngressIneligible(dep Deployment, req adapter.ChatRequest) bool {
	return req.Passthrough != nil && len(req.Passthrough.UnknownFields) > 0 &&
		dep.Provider != "anthropic" && !dep.AcceptLossyAnthropicIngress
}

// checkLossyIngressEligible is checkResponseFormatEnforceable's twin for
// lossiness: ErrLossyIngressRejected when the deployment
// rerouteToCapableDeploymentIfNeeded settled on would still drop members --
// i.e. no deployment in the pool can serve the request whole -- decided
// before any upstream call. Called beside its twins at both first-pick
// sites (runMissPath; streaming.go's handleChatCompletionStream).
func checkLossyIngressEligible(dep Deployment, req adapter.ChatRequest) error {
	if !lossyIngressIneligible(dep, req) {
		return nil
	}
	e := &ErrLossyIngressRejected{Pointers: sortedPointers(req.Passthrough.UnknownFields)}
	for _, ptr := range e.Pointers {
		if isTopLevelPointer(ptr) {
			e.TopLevelFields++
		} else {
			e.NestedCount++
		}
	}
	return e
}

func sortedPointers(unknown map[string]json.RawMessage) []string {
	pointers := make([]string, 0, len(unknown))
	for ptr := range unknown {
		pointers = append(pointers, ptr)
	}
	sort.Strings(pointers)
	return pointers
}

// passthroughFingerprint is the cache-key / L3 hard-gate / singleflight input
// for the ingress-only request state the canonical shadow does not carry
// (RFC-1 §3): the sha256, hex-encoded, of the canonical JSON of
// Passthrough.UnknownFields -- every member the parser could not consume,
// keyed by pointer, so the hash covers each one's raw value -- together
// with the indexes of the role:"tool" messages flagged is_error
// (adapter.Message.ToolResultIsError is json:"-", so serializeMessages and
// normalizeMessages never see it, and a failed tool result is a different
// prompt from a successful one). "" when neither is present, so a request
// from the OpenAI route and an Anthropic request with nothing unknown
// fingerprint alike and cross-format hits still happen -- the same
// empty-string convention as samplingFingerprint. A hash rather than the
// JSON itself because unknown members are unbounded client input and the L3
// candidate stores this string per entry; encoding/json sorts map keys and
// compacts each RawMessage, so equal inputs give equal hashes.
func passthroughFingerprint(req adapter.ChatRequest) string {
	var unknown map[string]json.RawMessage
	if req.Passthrough != nil {
		unknown = req.Passthrough.UnknownFields
	}
	var erroredToolResults []int
	for i, m := range req.Messages {
		if m.ToolResultIsError {
			erroredToolResults = append(erroredToolResults, i)
		}
	}
	if len(unknown) == 0 && len(erroredToolResults) == 0 {
		return ""
	}
	b, err := json.Marshal(struct {
		Unknown            map[string]json.RawMessage `json:"unknown,omitempty"`
		ErroredToolResults []int                      `json:"tool_result_errors,omitempty"`
	}{Unknown: unknown, ErroredToolResults: erroredToolResults})
	if err != nil {
		panic(fmt.Sprintf("dataplane: marshaling passthrough state for cache key: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// maxDroppedFieldsBytes bounds the dropped_fields telemetry summary (RFC-1
// §8): a span attribute and a log field, never unbounded client input.
const maxDroppedFieldsBytes = 512

// droppedFieldsSummary is the sorted, comma-joined pointer list for the
// telemetry carrier, bounded by boundedJoin.
func droppedFieldsSummary(pointers []string) string {
	sorted := append([]string(nil), pointers...)
	sort.Strings(sorted)
	return boundedJoin(sorted)
}

// boundedJoin comma-joins items in the order given, cut on an item
// boundary so the result never exceeds maxDroppedFieldsBytes; a cut ends
// in "…+N", N being how many items were left out. The one bound for every
// client-supplied member list that leaves the process -- the telemetry
// summary, the 400's message and its param.
func boundedJoin(items []string) string {
	if joined := strings.Join(items, ","); len(joined) <= maxDroppedFieldsBytes {
		return joined
	}
	for kept := len(items) - 1; kept > 0; kept-- {
		cut := strings.Join(items[:kept], ",") + ",…+" + strconv.Itoa(len(items)-kept)
		if len(cut) <= maxDroppedFieldsBytes {
			return cut
		}
	}
	return "…+" + strconv.Itoa(len(items))
}

// ingressFormat is Passthrough.Format for a request from the Anthropic
// Messages ingress and "" for the OpenAI route -- the telemetry carrier's
// discriminator (telemetry.ChatCompletionResult.IngressFormat).
func ingressFormat(req adapter.ChatRequest) string {
	if req.Passthrough == nil {
		return ""
	}
	return req.Passthrough.Format
}

// droppedFields is the dropped_fields summary for a SERVED ingress request
// whose shadow had unknown members: every hop translates today (slice S11
// adds the raw-body relay on an anthropic hop, and then reports nothing
// dropped there), so it is the pointers of req.Passthrough.UnknownFields.
// "" for the OpenAI route, for a request with nothing unknown, and for a
// rejected request (err != nil), whose error already carries the pointers.
func droppedFields(req adapter.ChatRequest, err error) string {
	if err != nil || req.Passthrough == nil || len(req.Passthrough.UnknownFields) == 0 {
		return ""
	}
	return droppedFieldsSummary(sortedPointers(req.Passthrough.UnknownFields))
}

// ingressLogFields are the chat_completion log line's ingress keys: nothing
// for the OpenAI route; ingress_format and passthrough (false on every hop
// until S11 relays) for an ingress request, plus dropped_fields when the
// served translate dropped members -- the same values finalize puts on
// the span, derived through the same two helpers.
func ingressLogFields(req adapter.ChatRequest, err error) []any {
	format := ingressFormat(req)
	if format == "" {
		return nil
	}
	fields := []any{"ingress_format", format, "passthrough", false}
	if dropped := droppedFields(req, err); dropped != "" {
		fields = append(fields, "dropped_fields", dropped)
	}
	return fields
}
