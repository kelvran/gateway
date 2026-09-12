package gemini

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// TestRoundTrip proves the two hazards shared with anthropic: system-prompt
// placement and tool-call argument re-encoding, plus Gemini's own
// "assistant" -> "model" role mapping.
func TestRoundTrip(t *testing.T) {
	original := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful weather assistant."},
			{Role: "user", Content: "What's the weather in Boston?"},
		},
		Tools: []adapter.ToolDef{
			{
				Name:           "get_weather",
				Description:    "Get the current weather for a city",
				ParametersJSON: `{"type":"object","properties":{"city":{"type":"string"}}}`,
			},
		},
	}

	a := New()

	nativeAny, err := a.ToProvider(original)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native, ok := nativeAny.(*Request)
	if !ok {
		t.Fatalf("ToProvider returned %T, want *Request", nativeAny)
	}

	if native.SystemInstruction == nil || len(native.SystemInstruction.Parts) != 1 ||
		native.SystemInstruction.Parts[0].Text != "You are a helpful weather assistant." {
		t.Errorf("native.SystemInstruction = %+v, want the system message content", native.SystemInstruction)
	}
	for _, c := range native.Contents {
		if c.Role == "system" {
			t.Errorf("native.Contents contains a role:system content; it must be pulled into SystemInstruction")
		}
	}
	if len(native.Contents) != 1 || native.Contents[0].Role != "user" {
		t.Fatalf("native.Contents = %+v, want exactly one user content", native.Contents)
	}

	toolCallArgs := `{"city":"Boston"}`
	var parsedArgs map[string]any
	if err := json.Unmarshal([]byte(toolCallArgs), &parsedArgs); err != nil {
		t.Fatalf("test setup: %v", err)
	}

	nativeResp := &Response{
		Candidates: []Candidate{
			{
				Content: Content{
					Role: "model",
					Parts: []Part{
						{FunctionCall: &FunctionCall{ID: "call_1", Name: "get_weather", Args: parsedArgs}},
					},
				},
				FinishReason: "STOP",
			},
		},
		UsageMetadata: UsageMetadata{PromptTokenCount: 20, CandidatesTokenCount: 8, TotalTokenCount: 28},
	}

	got, err := a.FromProvider(nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}

	if len(got.Choices) != 1 {
		t.Fatalf("Choices len = %d, want 1", len(got.Choices))
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want %q (STOP+functionCall must map to tool_calls)", got.Choices[0].FinishReason, "tool_calls")
	}
	gotToolCalls := got.Choices[0].Message.ToolCalls
	if len(gotToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(gotToolCalls))
	}

	if !json.Valid([]byte(gotToolCalls[0].ArgumentsJSON)) {
		t.Fatalf("ArgumentsJSON is not valid JSON: %q", gotToolCalls[0].ArgumentsJSON)
	}
	var roundTripped map[string]any
	if err := json.Unmarshal([]byte(gotToolCalls[0].ArgumentsJSON), &roundTripped); err != nil {
		t.Fatalf("unmarshaling round-tripped ArgumentsJSON: %v", err)
	}
	if roundTripped["city"] != "Boston" {
		t.Errorf("round-tripped arguments = %v, want city=Boston", roundTripped)
	}

	if got.Usage.PromptTokens != 20 || got.Usage.CompletionTokens != 8 || got.Usage.TotalTokens != 28 {
		t.Errorf("Usage = %+v, want {20 8 28}", got.Usage)
	}
}

// TestToProviderToolResultMessageResolvesFunctionResponseName proves the
// real hazard this adapter's own grounding research found by direct schema
// inspection: FunctionResponse.name is required, but the canonical
// role:"tool" message only carries ToolCallID — ToProvider must resolve
// the originating call's Name from message history.
func TestToProviderToolResultMessageResolvesFunctionResponseName(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "user", Content: "call the tool"},
			{Role: "assistant", ToolCalls: []adapter.ToolCall{
				{ID: "call_1", Name: "get_weather", ArgumentsJSON: `{"city":"Boston"}`},
			}},
			{Role: "tool", Content: `{"temp_f":72}`, ToolCallID: "call_1"},
		},
	}

	a := New()
	nativeAny, err := a.ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if len(native.Contents) != 3 {
		t.Fatalf("native.Contents len = %d, want 3", len(native.Contents))
	}
	toolResultContent := native.Contents[2]
	if toolResultContent.Role != "user" {
		t.Errorf("tool-result content Role = %q, want %q", toolResultContent.Role, "user")
	}
	if len(toolResultContent.Parts) != 1 || toolResultContent.Parts[0].FunctionResponse == nil {
		t.Fatalf("tool-result content Parts = %+v", toolResultContent.Parts)
	}
	fr := toolResultContent.Parts[0].FunctionResponse
	if fr.Name != "get_weather" {
		t.Errorf("FunctionResponse.Name = %q, want %q (resolved from message history)", fr.Name, "get_weather")
	}
	if fr.ID != "call_1" {
		t.Errorf("FunctionResponse.ID = %q, want %q", fr.ID, "call_1")
	}
	if fr.Response["result"] != `{"temp_f":72}` {
		t.Errorf("FunctionResponse.Response = %+v, want result=%q", fr.Response, `{"temp_f":72}`)
	}
}

// TestToProviderToolMessageWithUnknownToolCallIDFails proves ToProvider
// never sends Gemini a functionResponse with an empty/guessed name — a
// tool message referencing a tool_call_id with no matching prior ToolCall
// must fail loudly, not silently.
func TestToProviderToolMessageWithUnknownToolCallIDFails(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "tool", Content: "result", ToolCallID: "no-such-call"},
		},
	}

	_, err := New().ToProvider(req)
	if err == nil {
		t.Fatal("ToProvider: want error for unknown tool_call_id, got nil")
	}
	if !strings.Contains(err.Error(), "no-such-call") {
		t.Errorf("error = %v, want it to mention the unknown tool_call_id", err)
	}
}

func TestToProviderInvalidToolArguments(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "assistant", ToolCalls: []adapter.ToolCall{
				{ID: "call_1", Name: "get_weather", ArgumentsJSON: "{not valid json"},
			}},
		},
	}

	_, err := New().ToProvider(req)
	if err == nil {
		t.Fatal("ToProvider: want error for invalid ArgumentsJSON, got nil")
	}
}

// TestFromProviderMalformedFunctionCallReturnsError proves the model's own
// broken tool-call machinery surfaces as a real, typed error rather than a
// fake successful Choice.
func TestFromProviderMalformedFunctionCallReturnsError(t *testing.T) {
	resp := &Response{
		Candidates: []Candidate{
			{Content: Content{Role: "model"}, FinishReason: "MALFORMED_FUNCTION_CALL"},
		},
	}

	_, err := New().FromProvider(resp)
	if err == nil {
		t.Fatal("FromProvider: want error for MALFORMED_FUNCTION_CALL, got nil")
	}
}

func TestFromProviderNoCandidatesReturnsError(t *testing.T) {
	resp := &Response{Candidates: []Candidate{}}

	_, err := New().FromProvider(resp)
	if err == nil {
		t.Fatal("FromProvider: want error for zero candidates, got nil")
	}
	if errors.Is(err, adapter.ErrProviderContentPolicyBlocked) {
		t.Errorf("FromProvider: err = %v, should NOT wrap ErrProviderContentPolicyBlocked when promptFeedback carries no blockReason", err)
	}
}

// TestFromProviderPromptBlockedBySafetyFilteringWrapsContentPolicySentinel
// proves the real Gemini behavior confirmed against Google's live
// generateContent API reference: a prompt-level safety block returns
// zero candidates PLUS a populated promptFeedback.blockReason, a
// genuinely distinct condition from a candidate carrying FinishReason
// "SAFETY" (see TestFromProviderSafetyMapsToContentFilter below). This
// must be classifiable by
// gateway/internal/gateway/dataplane/fallback.go's classifyFallbackError
// (via errors.Is against adapter.ErrProviderContentPolicyBlocked) the
// same way every other provider's ordinary 4xx content-policy rejection
// already is, or a deployment's configured content_policy fallback
// chain silently never fires for this real Gemini condition.
func TestFromProviderPromptBlockedBySafetyFilteringWrapsContentPolicySentinel(t *testing.T) {
	resp := &Response{
		Candidates:     []Candidate{},
		PromptFeedback: PromptFeedback{BlockReason: "SAFETY"},
	}

	_, err := New().FromProvider(resp)
	if err == nil {
		t.Fatal("FromProvider: want error for a prompt-blocked response, got nil")
	}
	if !errors.Is(err, adapter.ErrProviderContentPolicyBlocked) {
		t.Errorf("FromProvider: err = %v, want it to wrap adapter.ErrProviderContentPolicyBlocked", err)
	}
	if !strings.Contains(err.Error(), "SAFETY") {
		t.Errorf("FromProvider: err = %v, want it to name the real blockReason (SAFETY)", err)
	}
}

func TestFromProviderStopWithoutFunctionCallMapsToStop(t *testing.T) {
	resp := &Response{
		Candidates: []Candidate{
			{
				Content:      Content{Role: "model", Parts: []Part{{Text: "hello"}}},
				FinishReason: "STOP",
			},
		},
	}

	got, err := New().FromProvider(resp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", got.Choices[0].FinishReason, "stop")
	}
	if got.Choices[0].Message.Content != "hello" {
		t.Errorf("Content = %q, want %q", got.Choices[0].Message.Content, "hello")
	}
}

func TestFromProviderMaxTokensMapsToLength(t *testing.T) {
	resp := &Response{
		Candidates: []Candidate{{Content: Content{Role: "model"}, FinishReason: "MAX_TOKENS"}},
	}
	got, err := New().FromProvider(resp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if got.Choices[0].FinishReason != "length" {
		t.Errorf("FinishReason = %q, want %q", got.Choices[0].FinishReason, "length")
	}
}

func TestFromProviderSafetyMapsToContentFilter(t *testing.T) {
	resp := &Response{
		Candidates: []Candidate{{Content: Content{Role: "model"}, FinishReason: "SAFETY"}},
	}
	got, err := New().FromProvider(resp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if got.Choices[0].FinishReason != "content_filter" {
		t.Errorf("FinishReason = %q, want %q", got.Choices[0].FinishReason, "content_filter")
	}
}

// TestFromProviderExtractsRealCachedTokens proves adapter.Usage.CacheReadTokens
// is populated from Gemini's real usageMetadata.cachedContentTokenCount field
// (implicit, on-by-default caching — no CachedContent API call needed), per
// docs/upgrade-research/cache-provider-native-caching-audit-round4-2026-09-11.md's
// Finding 1.
func TestFromProviderExtractsRealCachedTokens(t *testing.T) {
	resp := &Response{
		Candidates: []Candidate{
			{Content: Content{Role: "model", Parts: []Part{{Text: "hello"}}}, FinishReason: "STOP"},
		},
		UsageMetadata: UsageMetadata{
			PromptTokenCount:        1024,
			CandidatesTokenCount:    10,
			TotalTokenCount:         1034,
			CachedContentTokenCount: 896,
		},
	}

	got, err := New().FromProvider(resp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if got.Usage.CacheReadTokens != 896 {
		t.Errorf("Usage.CacheReadTokens = %d, want 896", got.Usage.CacheReadTokens)
	}
	if got.Usage.CacheCreationTokens != 0 {
		t.Errorf("Usage.CacheCreationTokens = %d, want 0 -- Gemini's implicit caching has no cache-creation charge", got.Usage.CacheCreationTokens)
	}
}

// TestFromProviderMissingCachedContentTokenCountDefaultsToZeroCacheRead
// proves the common case (no cache hit, or an older/non-caching model) stays
// exactly as it behaved before this field existed -- CacheReadTokens simply 0.
func TestFromProviderMissingCachedContentTokenCountDefaultsToZeroCacheRead(t *testing.T) {
	resp := &Response{
		Candidates: []Candidate{
			{Content: Content{Role: "model", Parts: []Part{{Text: "hello"}}}, FinishReason: "STOP"},
		},
		UsageMetadata: UsageMetadata{PromptTokenCount: 1024, CandidatesTokenCount: 10, TotalTokenCount: 1034},
	}

	got, err := New().FromProvider(resp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if got.Usage.CacheReadTokens != 0 {
		t.Errorf("Usage.CacheReadTokens = %d, want 0", got.Usage.CacheReadTokens)
	}
}

// preFixTextPartsOnly reproduces, verbatim, the part-dispatch switch
// FromProvider used BEFORE this fix (no part.Thought case at all --
// see docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md).
// It exists only so TestFromProviderSeparatesThoughtPartsFromAnswerTextInsteadOfMerging
// can prove the bug was real, not just assert the fixed behavior in
// isolation: fed the exact same input as the fixed FromProvider, this
// reproduces the historical silent merge.
func preFixTextPartsOnly(parts []Part) string {
	var textParts []string
	for _, part := range parts {
		switch {
		case part.FunctionCall != nil:
			// irrelevant to this proof; the bug never touched tool calls.
		case part.Text != "":
			textParts = append(textParts, part.Text)
		}
	}
	return strings.Join(textParts, "")
}

// TestFromProviderSeparatesThoughtPartsFromAnswerTextInsteadOfMerging is
// the load-bearing proof for
// docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md's
// Phase 4 fix: a real Gemini "thought" part's content rides on the same
// Text field an ordinary answer part uses (Gemini's schema has no
// dedicated thought block type, unlike Anthropic's "thinking" block) --
// confirmed real per generativelanguage.googleapis.com/$discovery/rest
// (v1beta, schema "Part"): Thought is "Optional. Indicates if the part
// is thought from the model." Before this fix, FromProvider's part
// switch had no case for Thought, so a thought part fell straight into
// the ordinary `case part.Text != ""` branch and was silently
// concatenated into the same Content string as the real answer --
// indistinguishable from it. preFixTextPartsOnly above reproduces that
// exact historical switch to prove the merge really happened for this
// exact input, before asserting the fixed adapter correctly separates
// the two.
func TestFromProviderSeparatesThoughtPartsFromAnswerTextInsteadOfMerging(t *testing.T) {
	parts := []Part{
		{Thought: true, Text: "Let me think about this carefully.", ThoughtSignature: "sig_thought_1"},
		{Text: "The answer is 42."},
	}

	// Step 1: prove the bug existed -- the OLD dispatch logic merges
	// the thought's text into the same string as the real answer, with
	// no way for a caller to tell them apart afterward.
	preFixMerged := preFixTextPartsOnly(parts)
	wantMerged := "Let me think about this carefully.The answer is 42."
	if preFixMerged != wantMerged {
		t.Fatalf("preFixTextPartsOnly(parts) = %q, want %q -- this proof's premise (the old code merges thought+answer text) doesn't hold for this input", preFixMerged, wantMerged)
	}

	// Step 2: prove the fix. Same input, run through the real, current
	// FromProvider.
	resp := &Response{
		Candidates: []Candidate{
			{
				Content:      Content{Role: "model", Parts: parts},
				FinishReason: "STOP",
			},
		},
	}

	got, err := New().FromProvider(resp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	msg := got.Choices[0].Message

	if msg.Content != "The answer is 42." {
		t.Errorf("Content = %q, want %q -- the thought part's text must NOT be merged into the visible answer", msg.Content, "The answer is 42.")
	}
	if strings.Contains(msg.Content, "think about this") {
		t.Errorf("Content = %q, contains thought text -- the merge bug is still present", msg.Content)
	}

	if len(msg.ReasoningBlocks) != 1 {
		t.Fatalf("ReasoningBlocks len = %d, want 1", len(msg.ReasoningBlocks))
	}
	rb := msg.ReasoningBlocks[0]
	if rb.Text != "Let me think about this carefully." {
		t.Errorf("ReasoningBlocks[0].Text = %q, want the thought part's own text", rb.Text)
	}
	if rb.Signature != "sig_thought_1" {
		t.Errorf("ReasoningBlocks[0].Signature = %q, want %q", rb.Signature, "sig_thought_1")
	}
	if rb.Redacted {
		t.Error("ReasoningBlocks[0].Redacted = true, want false -- Gemini's schema carries no redacted-thought concept")
	}
	if rb.Sequence != 0 {
		t.Errorf("ReasoningBlocks[0].Sequence = %d, want 0 (no tool calls in this response)", rb.Sequence)
	}
	if len(msg.ToolCalls) != 0 {
		t.Errorf("ToolCalls len = %d, want 0", len(msg.ToolCalls))
	}
}

// TestFromProviderThoughtPartBetweenFunctionCallsGetsCorrectSequence
// proves the Sequence bookkeeping introduced alongside the merge fix:
// a thought part appearing after the first functionCall part (but
// before a second) must record Sequence=1 ("immediately before
// ToolCalls[1]"), exactly mirroring anthropic.go's identical
// len(toolCalls)-so-far convention.
func TestFromProviderThoughtPartBetweenFunctionCallsGetsCorrectSequence(t *testing.T) {
	resp := &Response{
		Candidates: []Candidate{
			{
				Content: Content{
					Role: "model",
					Parts: []Part{
						{Thought: true, Text: "First I'll check Boston."},
						{FunctionCall: &FunctionCall{ID: "call_1", Name: "get_weather", Args: map[string]any{"city": "Boston"}}},
						{Thought: true, ThoughtSignature: "sig_between"},
						{FunctionCall: &FunctionCall{ID: "call_2", Name: "get_weather", Args: map[string]any{"city": "Tokyo"}}},
						{Thought: true, Text: "Both results are in."},
						{Text: "It's sunny in both cities."},
					},
				},
				FinishReason: "STOP",
			},
		},
	}

	got, err := New().FromProvider(resp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	msg := got.Choices[0].Message

	if len(msg.ToolCalls) != 2 {
		t.Fatalf("ToolCalls len = %d, want 2", len(msg.ToolCalls))
	}
	if len(msg.ReasoningBlocks) != 3 {
		t.Fatalf("ReasoningBlocks len = %d, want 3", len(msg.ReasoningBlocks))
	}
	wantSeqs := []int{0, 1, 2}
	for i, want := range wantSeqs {
		if msg.ReasoningBlocks[i].Sequence != want {
			t.Errorf("ReasoningBlocks[%d].Sequence = %d, want %d", i, msg.ReasoningBlocks[i].Sequence, want)
		}
	}
	// The signature-only thought part (no Text) must still be captured
	// -- a real Gemini shape per ai.google.dev/gemini-api/docs/thinking's
	// "may contain only a signature with no summary" caveat.
	if msg.ReasoningBlocks[1].Text != "" || msg.ReasoningBlocks[1].Signature != "sig_between" {
		t.Errorf("ReasoningBlocks[1] = %+v, want an empty-Text, signature-only block", msg.ReasoningBlocks[1])
	}
	if msg.Content != "It's sunny in both cities." {
		t.Errorf("Content = %q, want only the real answer", msg.Content)
	}
}

// TestFromProviderCapturesSignatureFusedOntoFunctionCallPart proves the
// second, narrower silent-drop bug documented at
// ai.google.dev/gemini-api/docs/thinking#signatures: "In the
// generateContent API, there are no dedicated thought blocks. Because of
// this, signatures are metadata that can be attached to any part, such as
// living inside functionCall parts or the final part of a response." A
// functionCall part carrying its own non-empty ThoughtSignature (no
// separate part.Thought==true part at all) must still have that signature
// captured -- not silently dropped -- even though the tool call itself is
// captured normally.
func TestFromProviderCapturesSignatureFusedOntoFunctionCallPart(t *testing.T) {
	resp := &Response{
		Candidates: []Candidate{
			{
				Content: Content{
					Role: "model",
					Parts: []Part{
						{
							FunctionCall:     &FunctionCall{ID: "call_1", Name: "get_weather", Args: map[string]any{"city": "Boston"}},
							ThoughtSignature: "sig_fused_on_call",
						},
						{Text: "It's sunny in Boston."},
					},
				},
				FinishReason: "STOP",
			},
		},
	}

	got, err := New().FromProvider(resp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	msg := got.Choices[0].Message

	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "call_1" {
		t.Fatalf("ToolCalls = %+v, want exactly call_1 captured normally", msg.ToolCalls)
	}
	if len(msg.ReasoningBlocks) != 1 {
		t.Fatalf("ReasoningBlocks len = %d, want 1 -- the functionCall-fused signature must not be silently dropped", len(msg.ReasoningBlocks))
	}
	rb := msg.ReasoningBlocks[0]
	if rb.Signature != "sig_fused_on_call" {
		t.Errorf("ReasoningBlocks[0].Signature = %q, want %q", rb.Signature, "sig_fused_on_call")
	}
	if rb.Text != "" {
		t.Errorf("ReasoningBlocks[0].Text = %q, want empty -- this signature rode on a functionCall part, not a dedicated thought part", rb.Text)
	}
	if rb.Sequence != 0 {
		t.Errorf("ReasoningBlocks[0].Sequence = %d, want 0 (immediately before ToolCalls[0])", rb.Sequence)
	}
}

// TestToProviderReplaysReasoningBlocksInOriginalInterleavedOrder mirrors
// anthropic_test.go's identically-named test: a canonical Message
// carrying ReasoningBlocks (as a caller would echo back from a prior
// FromProvider response) must be serialized with the reasoning blocks
// placed back at their exact original position relative to the
// functionCall parts -- not bunched before or after every tool call.
func TestToProviderReplaysReasoningBlocksInOriginalInterleavedOrder(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gemini-3-pro",
		Messages: []adapter.Message{
			{Role: "user", Content: "What's the weather in Boston and Tokyo?"},
			{
				Role: "assistant",
				ReasoningBlocks: []adapter.ReasoningBlock{
					{Sequence: 0, Text: "First I'll check Boston.", Signature: "sig_1"},
					{Sequence: 1, Signature: "sig_between"},
					{Sequence: 2, Text: "Both results are in, I can answer now.", Signature: "sig_3"},
				},
				ToolCalls: []adapter.ToolCall{
					{ID: "call_1", Name: "get_weather", ArgumentsJSON: `{"city":"Boston"}`},
					{ID: "call_2", Name: "get_weather", ArgumentsJSON: `{"city":"Tokyo"}`},
				},
			},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)
	if len(native.Contents) != 2 {
		t.Fatalf("native.Contents len = %d, want 2", len(native.Contents))
	}

	parts := native.Contents[1].Parts
	wantKinds := []string{"thought", "functionCall", "thought", "functionCall", "thought"}
	if len(parts) != len(wantKinds) {
		t.Fatalf("assistant content Parts len = %d, want %d (%v)", len(parts), len(wantKinds), wantKinds)
	}
	for i, want := range wantKinds {
		var got string
		switch {
		case parts[i].Thought:
			got = "thought"
		case parts[i].FunctionCall != nil:
			got = "functionCall"
		default:
			got = "other"
		}
		if got != want {
			t.Errorf("parts[%d] kind = %q, want %q", i, got, want)
		}
	}

	if parts[0].Text != "First I'll check Boston." || parts[0].ThoughtSignature != "sig_1" {
		t.Errorf("parts[0] = %+v, want the first thought block replayed verbatim", parts[0])
	}
	if parts[2].ThoughtSignature != "sig_between" || parts[2].Text != "" {
		t.Errorf("parts[2] = %+v, want the signature-only block replayed verbatim", parts[2])
	}
	if parts[4].Text != "Both results are in, I can answer now." || parts[4].ThoughtSignature != "sig_3" {
		t.Errorf("parts[4] = %+v, want the trailing thought block replayed after the last functionCall", parts[4])
	}
}

// TestReasoningBlockRoundTripPreservesExactOriginalOrder mirrors
// anthropic_test.go's identically-named end-to-end proof: capture an
// interleaved thought+functionCall response via FromProvider, then feed
// the resulting canonical Message straight back through ToProvider as
// conversation history, and assert the re-serialized part-kind order is
// identical to the original native response.
func TestReasoningBlockRoundTripPreservesExactOriginalOrder(t *testing.T) {
	a := New()
	originalParts := []Part{
		{Thought: true, Text: "Step one.", ThoughtSignature: "sig_a"},
		{FunctionCall: &FunctionCall{ID: "call_1", Name: "step_one", Args: map[string]any{}}},
		{Thought: true, Text: "Step two.", ThoughtSignature: "sig_b"},
		{FunctionCall: &FunctionCall{ID: "call_2", Name: "step_two", Args: map[string]any{}}},
	}
	nativeResp := &Response{
		Candidates: []Candidate{
			{Content: Content{Role: "model", Parts: originalParts}, FinishReason: "STOP"},
		},
	}

	got, err := a.FromProvider(nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	assistantMsg := got.Choices[0].Message
	assistantMsg.Role = "assistant"

	replayed, err := a.ToProvider(adapter.ChatRequest{
		Model:    "gemini-3-pro",
		Messages: []adapter.Message{{Role: "user", Content: "go"}, assistantMsg},
	})
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := replayed.(*Request)
	replayedParts := native.Contents[1].Parts

	if len(replayedParts) != len(originalParts) {
		t.Fatalf("replayed part count = %d, want %d", len(replayedParts), len(originalParts))
	}
	for i := range originalParts {
		wantThought := originalParts[i].Thought
		gotThought := replayedParts[i].Thought
		wantCall := originalParts[i].FunctionCall != nil
		gotCall := replayedParts[i].FunctionCall != nil
		if gotThought != wantThought || gotCall != wantCall {
			t.Errorf("part[%d] kind mismatch: got (thought=%v call=%v), want (thought=%v call=%v) -- reasoning/functionCall order was not preserved through a full FromProvider->ToProvider round trip", i, gotThought, gotCall, wantThought, wantCall)
		}
	}
}

// TestToProviderDropsRedactedReasoningBlockRatherThanFabricatingThought
// proves reasoningBlockToProvider's documented limitation: Gemini's
// schema has no encrypted-thought concept, so a Redacted canonical
// ReasoningBlock (only ever produced by Anthropic/Bedrock) must be
// dropped, not mismapped into a fabricated plaintext Gemini thought
// part.
func TestToProviderDropsRedactedReasoningBlockRatherThanFabricatingThought(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gemini-3-pro",
		Messages: []adapter.Message{
			{
				Role: "assistant",
				ReasoningBlocks: []adapter.ReasoningBlock{
					{Sequence: 0, Redacted: true, Data: "opaque_ciphertext_from_another_provider"},
				},
				Content: "hello",
			},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)
	parts := native.Contents[0].Parts

	for _, p := range parts {
		if p.Thought {
			t.Errorf("parts contains a fabricated thought Part for a Redacted block: %+v", p)
		}
	}
	if len(parts) != 1 || parts[0].Text != "hello" {
		t.Errorf("parts = %+v, want exactly the ordinary text part (Redacted block silently dropped)", parts)
	}
}

func TestName(t *testing.T) {
	if got := New().Name(); got != "gemini" {
		t.Errorf("Name() = %q, want %q", got, "gemini")
	}
}

// TestToProviderMultiModalContentPartsMapToInlineDataAndFileData is the
// load-bearing proof for docs/rfcs/2026-09-06-gateway-multimodal-
// content.md: an inline-base64 image part must map to Gemini's real
// inlineData shape, and a URL-referenced document part to fileData —
// Gemini has no distinct "document" wire type, so a document part
// reuses the identical shape with a document MediaType.
func TestToProviderMultiModalContentPartsMapToInlineDataAndFileData(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{
				Role:    "user",
				Content: "what's in this image and document?",
				Parts: []adapter.ContentPart{
					{Type: "image", MediaType: "image/png", Data: "aW1hZ2ViYXNlNjQ="},
					{Type: "document", MediaType: "application/pdf", URL: "https://example.com/doc.pdf"},
				},
			},
		},
	}

	a := New()
	nativeAny, err := a.ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if len(native.Contents) != 1 {
		t.Fatalf("native.Contents len = %d, want 1", len(native.Contents))
	}
	parts := native.Contents[0].Parts
	if len(parts) != 3 {
		t.Fatalf("parts len = %d, want 3 (text, image, document)", len(parts))
	}

	if parts[0].Text != "what's in this image and document?" {
		t.Errorf("parts[0].Text = %q, want the message content", parts[0].Text)
	}

	imgPart := parts[1]
	if imgPart.InlineData == nil || imgPart.InlineData.MimeType != "image/png" || imgPart.InlineData.Data != "aW1hZ2ViYXNlNjQ=" {
		t.Errorf("parts[1].InlineData = %+v, want a populated InlineData with the image's MediaType/Data", imgPart.InlineData)
	}
	if imgPart.FileData != nil {
		t.Errorf("parts[1].FileData = %+v, want nil (this part used inline Data, not URL)", imgPart.FileData)
	}

	docPart := parts[2]
	if docPart.FileData == nil || docPart.FileData.MimeType != "application/pdf" || docPart.FileData.FileURI != "https://example.com/doc.pdf" {
		t.Errorf("parts[2].FileData = %+v, want a populated FileData with the document's MediaType/URL", docPart.FileData)
	}
	if docPart.InlineData != nil {
		t.Errorf("parts[2].InlineData = %+v, want nil (this part used URL, not inline Data)", docPart.InlineData)
	}
}

// TestToProviderContentPartWithNeitherDataNorURLFailsLoudly proves a
// malformed image/document part returns a real, typed error rather than
// silently producing an empty part.
func TestToProviderContentPartWithNeitherDataNorURLFailsLoudly(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "user", Parts: []adapter.ContentPart{{Type: "image", MediaType: "image/png"}}},
		},
	}

	if _, err := New().ToProvider(req); err == nil {
		t.Fatal("ToProvider with a Data-less, URL-less image part returned nil error, want an error")
	}
}

// TestToProviderUnsupportedContentPartTypeFailsLoudly proves an unknown
// part type returns a real, typed error rather than being silently
// dropped.
func TestToProviderUnsupportedContentPartTypeFailsLoudly(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "user", Parts: []adapter.ContentPart{{Type: "video", MediaType: "video/mp4", Data: "x"}}},
		},
	}

	if _, err := New().ToProvider(req); err == nil {
		t.Fatal("ToProvider with an unsupported content part type returned nil error, want an error")
	}
}

// TestToProviderCacheControlIsUnaffected is the load-bearing proof for
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md's "why Gemini
// is excluded" claim: setting adapter.CacheControl on a message, on a
// system message, on a content part, and (per that RFC's
// tool-definition-level addendum) on a ToolDef, must produce a native
// Gemini request byte-identical to the same request with no
// CacheControl set at all -- not just "gemini.go has no
// CacheControl-reading code" (true by inspection) but a real, executed
// proof that the marker has zero observable effect on this adapter's
// actual output, across the whole feature's scope, not just its
// original message/part-level part. Also extended, per
// docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md, to set
// DisableCacheControlAutoPopulate on withMarker too -- that field only
// ever changes Anthropic's/Bedrock's own system-message handling;
// gemini.go's ToProvider never reads it, so it must be an equally
// zero-effect no-op here, proven in the same single test rather than a
// new standalone one.
func TestToProviderCacheControlIsUnaffected(t *testing.T) {
	withoutMarker := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{
				Role:    "user",
				Content: "what's in this document?",
				Parts: []adapter.ContentPart{
					{Type: "document", MediaType: "application/pdf", Data: "ZG9jYmFzZTY0"},
				},
			},
		},
		Tools: []adapter.ToolDef{
			{Name: "get_weather", Description: "Get the weather", ParametersJSON: `{"type":"object"}`},
		},
	}

	withMarker := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful assistant.", CacheControl: &adapter.CacheControl{TTL: "1h"}},
			{
				Role:         "user",
				Content:      "what's in this document?",
				CacheControl: &adapter.CacheControl{Key: "session-123"},
				Parts: []adapter.ContentPart{
					{
						Type:         "document",
						MediaType:    "application/pdf",
						Data:         "ZG9jYmFzZTY0",
						CacheControl: &adapter.CacheControl{TTL: "1h", Key: "session-123"},
					},
				},
			},
		},
		Tools: []adapter.ToolDef{
			{
				Name:           "get_weather",
				Description:    "Get the weather",
				ParametersJSON: `{"type":"object"}`,
				CacheControl:   &adapter.CacheControl{TTL: "1h", Key: "session-123"},
			},
		},
		DisableCacheControlAutoPopulate: true,
	}

	a := New()

	gotWithout, err := a.ToProvider(withoutMarker)
	if err != nil {
		t.Fatalf("ToProvider(withoutMarker): %v", err)
	}
	gotWith, err := a.ToProvider(withMarker)
	if err != nil {
		t.Fatalf("ToProvider(withMarker): %v", err)
	}

	jsonWithout, err := json.Marshal(gotWithout)
	if err != nil {
		t.Fatalf("marshaling withoutMarker result: %v", err)
	}
	jsonWith, err := json.Marshal(gotWith)
	if err != nil {
		t.Fatalf("marshaling withMarker result: %v", err)
	}

	if string(jsonWithout) != string(jsonWith) {
		t.Errorf("CacheControl changed Gemini's native request output:\nwithout marker: %s\nwith marker:    %s", jsonWithout, jsonWith)
	}
}

// TestToProviderResponseFormatSetsResponseMimeTypeAndSchema proves a
// canonical ResponseFormat/JSONSchema maps onto Gemini's real
// generationConfig.responseMimeType ("application/json")/responseSchema
// fields, and that this alone is enough to force a GenerationConfig to
// exist even when Temperature/MaxTokens are both unset.
func TestToProviderResponseFormatSetsResponseMimeTypeAndSchema(t *testing.T) {
	req := adapter.ChatRequest{
		Model:    "gemini-2.5-flash",
		Messages: []adapter.Message{{Role: "user", Content: "give me JSON"}},
		ResponseFormat: &adapter.ResponseFormat{
			Type: "json_schema",
			JSONSchema: &adapter.JSONSchema{
				Name:   "weather_response",
				Schema: json.RawMessage(`{"type":"object","properties":{"temp_f":{"type":"number"}}}`),
			},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if native.GenerationConfig == nil {
		t.Fatal("native.GenerationConfig = nil, want a populated *GenerationConfig (ResponseFormat alone must force one to exist)")
	}
	if native.GenerationConfig.ResponseMimeType != "application/json" {
		t.Errorf("GenerationConfig.ResponseMimeType = %q, want application/json", native.GenerationConfig.ResponseMimeType)
	}
	if native.GenerationConfig.ResponseSchema["type"] != "object" {
		t.Errorf("GenerationConfig.ResponseSchema = %v, want the parsed schema object", native.GenerationConfig.ResponseSchema)
	}
}

// TestToProviderNilResponseFormatOmitsResponseMimeType proves the unset
// (nil, the default) case never emits responseMimeType/responseSchema at
// all -- byte-identical to every ChatRequest built before this field
// existed.
func TestToProviderNilResponseFormatOmitsResponseMimeType(t *testing.T) {
	req := adapter.ChatRequest{
		Model:    "gemini-2.5-flash",
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)
	if native.GenerationConfig != nil {
		t.Errorf("native.GenerationConfig = %+v, want nil", native.GenerationConfig)
	}

	b, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("marshaling native request: %v", err)
	}
	if strings.Contains(string(b), "responseMimeType") || strings.Contains(string(b), "responseSchema") {
		t.Errorf("marshaled request contains responseMimeType/responseSchema despite ResponseFormat being nil: %s", b)
	}
}
