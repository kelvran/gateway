package anthropic

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// The compaction fixtures are real responses captured from Bedrock
// InvokeModel on 2026-10-14 (Anthropic's native Messages shape; round-5 Tier
// A item 3). On-demand compaction (anthropic-beta compact-2026-09-04) reports
// top-level input_tokens/output_tokens 0 and the summarization call only in
// usage.iterations; threshold compaction (compact-2026-01-12) reports the
// reply's own counts at top level and the compaction iteration only in
// usage.iterations. Anthropic's rule on both pages: sum across every entry.

func fromProviderFixture(t *testing.T, name string) adapter.ChatResponse {
	t.Helper()
	var native Response
	if err := json.Unmarshal(mustReadTestdata(t, name), &native); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
	}
	got, err := New().FromProvider(&native)
	if err != nil {
		t.Fatalf("FromProvider(%s): %v", name, err)
	}
	return got
}

func fromProviderJSON(t *testing.T, body string) adapter.ChatResponse {
	t.Helper()
	var native Response
	if err := json.Unmarshal([]byte(body), &native); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := New().FromProvider(&native)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	return got
}

// On-demand: the only content is a compaction block (unknown to the canonical
// schema, so the response is unrepresentable and never cached), stop_reason
// is "compaction", and the 172/241 the model consumed live only in
// usage.iterations -- metered as zero before this change.
func TestFromProviderSumsUsageIterationsOnDemandCompaction(t *testing.T) {
	got := fromProviderFixture(t, "response_anthropic_native_compaction_on_demand.json")
	want := adapter.Usage{PromptTokens: 172, CompletionTokens: 241, TotalTokens: 413, CompactionIterations: 1, ServiceTier: "standard"}
	if got.Usage != want {
		t.Errorf("Usage = %+v, want %+v", got.Usage, want)
	}
	if got.StopReason != "compaction" || got.Choices[0].FinishReason != "compaction" {
		t.Errorf("stop_reason/finish_reason = %q/%q, want compaction/compaction", got.StopReason, got.Choices[0].FinishReason)
	}
	if !got.Unrepresentable {
		t.Error("a compaction block is not in the canonical schema; the response must be unrepresentable (never cached or replayed)")
	}
}

// Threshold: top-level 165/56 is the reply alone; the compaction iteration's
// 57411/64 is only in usage.iterations. The sum of every entry is what the
// request consumed and was billed -- not top-level, and not top-level plus
// the array (which would double count the message entry).
func TestFromProviderSumsUsageIterationsThresholdCompaction(t *testing.T) {
	got := fromProviderFixture(t, "response_anthropic_native_compaction_threshold.json")
	want := adapter.Usage{PromptTokens: 57411 + 165, CompletionTokens: 64 + 56, TotalTokens: 57411 + 165 + 64 + 56, CompactionIterations: 1, ServiceTier: "standard"}
	if got.Usage != want {
		t.Errorf("Usage = %+v, want %+v", got.Usage, want)
	}
	if got.Choices[0].FinishReason != "stop" || got.StopReason != "end_turn" {
		t.Errorf("finish/stop = %q/%q, want stop/end_turn", got.Choices[0].FinishReason, got.StopReason)
	}
	if !got.Unrepresentable {
		t.Error("the leading compaction block makes the response unrepresentable")
	}
}

// Every iteration entry carries the cache members (proven: present, zero, on
// the live bodies), so the sum feeds the cache tiers and the 1h slice too,
// and PromptTokens stays cache-inclusive.
func TestFromProviderIterationsCarryCacheMembersIntoTheCacheTiers(t *testing.T) {
	got := fromProviderJSON(t, `{"id":"msg_c","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"iterations":[{"type":"compaction","input_tokens":2000,"output_tokens":300,"cache_creation_input_tokens":6000,"cache_read_input_tokens":100000,"cache_creation":{"ephemeral_5m_input_tokens":1000,"ephemeral_1h_input_tokens":5000}},{"type":"message","input_tokens":50,"output_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":400,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}}]}}`)
	want := adapter.Usage{
		PromptTokens: 2000 + 6000 + 100000 + 50 + 400, CompletionTokens: 310, TotalTokens: 2000 + 6000 + 100000 + 50 + 400 + 310,
		CacheReadTokens: 100400, CacheCreationTokens: 6000, CacheCreation1hTokens: 5000, CompactionIterations: 1,
	}
	if got.Usage != want {
		t.Errorf("Usage = %+v, want %+v", got.Usage, want)
	}
}

// With a compaction beta on and no compaction, iterations holds one message
// entry equal to the top-level counts (captured live: Haiku 5.5 under
// compact-2026-09-04 without the parameter, Sonnet 4.6 under compact-2026-01-12
// below its trigger): the sum is the same usage as before and nothing is
// counted as a compaction.
func TestFromProviderMessageOnlyIterationsMatchTopLevel(t *testing.T) {
	got := fromProviderFixture(t, "response_anthropic_native_iterations_message_only.json")
	if want := (adapter.Usage{PromptTokens: 114, CompletionTokens: 16, TotalTokens: 130, ServiceTier: "standard"}); got.Usage != want {
		t.Errorf("Haiku 5.5: Usage = %+v, want %+v", got.Usage, want)
	}
	got = fromProviderFixture(t, "response_anthropic_native_threshold_below_trigger.json")
	if want := (adapter.Usage{PromptTokens: 153, CompletionTokens: 16, TotalTokens: 169, ServiceTier: "standard"}); got.Usage != want {
		t.Errorf("Sonnet 4.6 below the trigger: Usage = %+v, want %+v", got.Usage, want)
	}
}

// Sonnet 5.5 accepts on-demand compaction although its Bedrock model card
// omits it (captured live 2026-10-14): the same shape, 172/307.
func TestFromProviderSumsUsageIterationsOnDemandCompactionSonnet55(t *testing.T) {
	got := fromProviderFixture(t, "response_anthropic_native_compaction_on_demand_sonnet55.json")
	want := adapter.Usage{PromptTokens: 172, CompletionTokens: 307, TotalTokens: 479, CompactionIterations: 1, ServiceTier: "standard"}
	if got.Usage != want || got.StopReason != "compaction" || !got.Unrepresentable {
		t.Errorf("Usage = %+v (stop %q, unrepresentable %v), want %+v / compaction / true", got.Usage, got.StopReason, got.Unrepresentable, want)
	}
}

// The per-count floor: a partially populated array (the shape AWS's own
// documentation describes, never seen live) can never meter a response below
// the top-level counts it arrived with.
func TestFromProviderIterationsAreFlooredAtTheTopLevelCounts(t *testing.T) {
	got := fromProviderJSON(t, `{"id":"msg_f","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":500,"output_tokens":50,"cache_read_input_tokens":200,"iterations":[{"type":"message","input_tokens":100,"output_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}]}}`)
	want := adapter.Usage{PromptTokens: 700, CompletionTokens: 50, TotalTokens: 750, CacheReadTokens: 200}
	if got.Usage != want {
		t.Errorf("Usage = %+v, want the top-level counts kept %+v", got.Usage, want)
	}
}

// advisor_message / fallback_message entries name their own model; their
// tokens are summed like every entry and the count is surfaced so the
// dataplane can say they were priced at the serving model's rates.
func TestFromProviderCountsForeignModelIterations(t *testing.T) {
	got := fromProviderJSON(t, `{"id":"msg_a","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":100,"output_tokens":10,"iterations":[{"type":"advisor_message","model":"claude-opus-5-5","input_tokens":300,"output_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},{"type":"message","model":"claude-haiku-5-5","input_tokens":100,"output_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}]}}`)
	want := adapter.Usage{PromptTokens: 400, CompletionTokens: 30, TotalTokens: 430, ForeignModelIterations: 1, ForeignIterationModels: "claude-opus-5-5"}
	if got.Usage != want {
		t.Errorf("Usage = %+v, want %+v", got.Usage, want)
	}
}

// An absent, null or empty iterations array leaves today's top-level mapping
// untouched (every pre-beta response).
func TestFromProviderWithoutIterationsKeepsTheTopLevelUsage(t *testing.T) {
	for name, body := range map[string]string{
		"absent": `{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1}}`,
		"null":   `{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1,"iterations":null}}`,
		"empty":  `{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1,"iterations":[]}}`,
	} {
		got := fromProviderJSON(t, body)
		if want := (adapter.Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6}); got.Usage != want {
			t.Errorf("%s: Usage = %+v, want %+v", name, got.Usage, want)
		}
	}
}

// decodeStreamFixture returns the one usage the decoder reports, the finish
// chunk, and whether ANY chunk was marked unrepresentable (the accumulator
// folds the per-chunk flag into the response; an unknown block is flagged on
// its own content_block_start chunk, an unknown event on the finish chunk).
func decodeStreamFixture(t *testing.T, name string) (*adapter.Usage, streaming.ChatCompletionChunk, bool) {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	defer func() { _ = f.Close() }()
	decoder := New().NewStreamDecoder()
	reader := streaming.NewReader(f)
	var usage *adapter.Usage
	var finish streaming.ChatCompletionChunk
	unrepresentable := false
	seen := 0
	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reader.Next(): %v", err)
		}
		chunks, _, gotUsage, decErr := decoder.Decode(ev)
		if decErr != nil {
			t.Fatalf("Decode(%+v): %v", ev, decErr)
		}
		if gotUsage != nil {
			seen++
			usage = gotUsage
		}
		for _, c := range chunks {
			unrepresentable = unrepresentable || c.Unrepresentable
			if len(c.Choices) > 0 && c.Choices[0].FinishReason != nil {
				finish = c
			}
		}
	}
	if seen != 1 || usage == nil {
		t.Fatalf("usage reported %d times, want exactly once", seen)
	}
	return usage, finish, unrepresentable
}

// Streamed on-demand compaction (proven): message_start carries zeros and no
// iterations, the compaction block arrives whole on content_block_start, and
// the final message_delta carries stop_reason compaction, output_tokens 0 and
// the iterations array -- the only place the 172/224 appear.
func TestStreamDecoder_SumsUsageIterationsOnDemandCompaction(t *testing.T) {
	usage, finish, unrepresentable := decodeStreamFixture(t, "stream_compaction_on_demand.txt")
	want := adapter.Usage{PromptTokens: 172, CompletionTokens: 224, TotalTokens: 396, CompactionIterations: 1, ServiceTier: "standard"}
	if *usage != want {
		t.Errorf("usage = %+v, want %+v", *usage, want)
	}
	if finish.Choices == nil || *finish.Choices[0].FinishReason != "compaction" || finish.Choices[0].StopReason != "compaction" {
		t.Errorf("finish chunk = %+v, want finish_reason/stop_reason compaction", finish)
	}
	if !unrepresentable {
		t.Error("the compaction block is unknown to the decoder; the stream must be marked unrepresentable (never cached or replayed)")
	}
}

// Streamed threshold compaction (proven): message_start reports the FIRST
// iteration's input (the compaction's 57411), message_delta the reply's own
// 165/16 plus the full iterations array. Today's decoder would bill 57411
// prompt and 16 completion; the sum is 57576 and 80.
func TestStreamDecoder_SumsUsageIterationsThresholdCompaction(t *testing.T) {
	usage, finish, unrepresentable := decodeStreamFixture(t, "stream_compaction_threshold.txt")
	want := adapter.Usage{PromptTokens: 57411 + 165, CompletionTokens: 64 + 16, TotalTokens: 57411 + 165 + 64 + 16, CompactionIterations: 1, ServiceTier: "standard"}
	if *usage != want {
		t.Errorf("usage = %+v, want %+v", *usage, want)
	}
	if finish.Choices == nil || *finish.Choices[0].FinishReason != "stop" || finish.Choices[0].StopReason != "end_turn" {
		t.Errorf("finish chunk = %+v, want finish_reason stop / stop_reason end_turn", finish)
	}
	if !unrepresentable {
		t.Error("the compaction block and its compaction_delta are unknown to the decoder; the stream must be marked unrepresentable")
	}
}

// SYNTHETIC placement: the live stream carries iterations on message_delta
// only, but both usage objects declare the member (anthropic-sdk-go v1.80.0),
// so an array that arrives on message_start alone is honoured too.
func TestStreamDecoder_FallsBackToMessageStartIterations(t *testing.T) {
	usage, _, _ := decodeStreamFixture(t, "stream_compaction_iterations_on_message_start.txt")
	want := adapter.Usage{PromptTokens: 172, CompletionTokens: 224, TotalTokens: 396, CompactionIterations: 1, ServiceTier: "standard"}
	if *usage != want {
		t.Errorf("usage = %+v, want %+v", *usage, want)
	}
}
