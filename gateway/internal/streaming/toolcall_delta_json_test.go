package streaming

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The streamed tool-call delta carries two shapes during the deprecation
// window opened in gateway/v0.20.0 (DEPRECATED.md): the flat members every
// chunk has carried since the first release, and OpenAI's
// ChatCompletionMessageToolCallChunk nesting (`type`, `function.{name,
// arguments}`) that the official SDKs' stream accumulators read. These tests
// pin the exact bytes of both, in both directions.

func TestToolCallDeltaMarshalsBothShapesOnTheIntroducingFragment(t *testing.T) {
	got, err := json.Marshal(ToolCallDelta{Index: 0, ID: "call_1", Name: "get_weather", ArgumentsJSON: `{"ci`})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"index":0,"id":"call_1","type":"function","name":"get_weather","arguments_json":"{\"ci","function":{"name":"get_weather","arguments":"{\"ci"}}`
	if string(got) != want {
		t.Errorf("introducing fragment:\n got  %s\n want %s", got, want)
	}
}

func TestToolCallDeltaMarshalsAContinuationFragmentWithoutTypeOrName(t *testing.T) {
	got, err := json.Marshal(ToolCallDelta{Index: 0, ArgumentsJSON: `ty":`})
	if err != nil {
		t.Fatal(err)
	}
	// OpenAI's continuation chunks carry neither id nor type nor a function
	// name; the SDK accumulators append function.arguments and nothing else.
	const want = `{"index":0,"arguments_json":"ty\":","function":{"arguments":"ty\":"}}`
	if string(got) != want {
		t.Errorf("continuation fragment:\n got  %s\n want %s", got, want)
	}
}

// Bedrock's ConverseStream opens a tool block with a fragment that carries only
// the index (seen live on claude-haiku-4-5, 2026-10-12), and openai-python's
// stream helper asserts `function` on every tool-call delta before reading
// `function.arguments or ""`. So `function` is never omitted and `arguments`
// is always a string, as in OpenAI's own stream.
func TestToolCallDeltaMarshalsAnIndexOnlyFragmentWithAnEmptyFunctionArguments(t *testing.T) {
	got, err := json.Marshal(ToolCallDelta{Index: 2})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"index":2,"function":{"arguments":""}}` {
		t.Errorf("index-only fragment = %s, want {\"index\":2,\"function\":{\"arguments\":\"\"}}", got)
	}
}

// The Anthropic and Bedrock decoders introduce a call with id and name and no
// arguments yet; OpenAI's introducing chunk writes `"arguments":""` there.
func TestToolCallDeltaMarshalsANameOnlyIntroducingFragmentLikeOpenAI(t *testing.T) {
	got, err := json.Marshal(ToolCallDelta{Index: 0, ID: "toolu_1", Name: "get_weather"})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"index":0,"id":"toolu_1","type":"function","name":"get_weather","function":{"name":"get_weather","arguments":""}}`
	if string(got) != want {
		t.Errorf("name-only introducing fragment:\n got  %s\n want %s", got, want)
	}
}

func TestToolCallDeltaUnmarshalsFlatNestedAndBothShapes(t *testing.T) {
	want := ToolCallDelta{Index: 1, ID: "call_1", Name: "get_weather", ArgumentsJSON: `{"city":"Boston"}`}
	cases := map[string]string{
		// what every fixture and every pre-v0.20.0 client wrote
		"flat only": `{"index":1,"id":"call_1","name":"get_weather","arguments_json":"{\"city\":\"Boston\"}"}`,
		// OpenAI's own shape, and the only shape after the flat members are removed
		"nested only": `{"index":1,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Boston\"}"}}`,
		// what the gateway itself writes during the window
		"both": `{"index":1,"id":"call_1","type":"function","name":"get_weather","arguments_json":"{\"city\":\"Boston\"}","function":{"name":"get_weather","arguments":"{\"city\":\"Boston\"}"}}`,
	}
	for name, in := range cases {
		var got ToolCallDelta
		if err := json.Unmarshal([]byte(in), &got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: got %+v, want %+v", name, got, want)
		}
	}
}

func TestToolCallDeltaUnmarshalPrefersTheFlatMemberWhenBothArePresent(t *testing.T) {
	// A disagreement is not an error: the flat member is the one every chunk
	// carried before the nesting existed, so a hand-built frame that sets
	// only it keeps meaning what it meant.
	var got ToolCallDelta
	if err := json.Unmarshal([]byte(`{"index":0,"name":"flat","arguments_json":"{}","function":{"name":"nested","arguments":"[]"}}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "flat" || got.ArgumentsJSON != "{}" {
		t.Errorf("got %+v, want the flat members to win", got)
	}
}

func TestToolCallDeltaRoundTripsThroughJSON(t *testing.T) {
	for _, d := range []ToolCallDelta{
		{Index: 0},
		{Index: 0, ID: "call_1", Name: "get_weather"},
		{Index: 0, ID: "call_1", Name: "get_weather", ArgumentsJSON: `{"city":"Boston"}`},
		{Index: 3, ArgumentsJSON: `on"}`},
		{Index: 1, Name: "lookup"},
	} {
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var back ToolCallDelta
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if back != d {
			t.Errorf("%s round-tripped to %+v, want %+v", raw, back, d)
		}
	}
}

// TestToolCallDeltaFramesReassembleThroughTheNestedMembersAlone reads a
// streamed call the way an OpenAI SDK accumulator does -- id and type as
// metadata, function.name and function.arguments appended per index, the
// flat members never consulted -- and gets the whole call back.
func TestToolCallDeltaFramesReassembleThroughTheNestedMembersAlone(t *testing.T) {
	frames := []ToolCallDelta{
		{Index: 0, ID: "call_1", Name: "get_weather", ArgumentsJSON: `{"city": "Bos`},
		{Index: 0, ArgumentsJSON: `ton"}`},
		{Index: 1, ID: "call_2", Name: "get_time", ArgumentsJSON: ``},
		{Index: 1, ArgumentsJSON: `{"zone":"UTC"}`},
	}
	type nested struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	acc := map[int]*nested{}
	for _, f := range frames {
		raw, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		var n nested
		if err := json.Unmarshal(raw, &n); err != nil {
			t.Fatal(err)
		}
		cur, ok := acc[n.Index]
		if !ok {
			cur = &nested{Index: n.Index}
			acc[n.Index] = cur
		}
		if n.ID != "" {
			cur.ID = n.ID
		}
		if n.Type != "" {
			cur.Type = n.Type
		}
		cur.Function.Name += n.Function.Name
		cur.Function.Arguments += n.Function.Arguments
	}
	want := map[int][4]string{
		0: {"call_1", "function", "get_weather", `{"city": "Boston"}`},
		1: {"call_2", "function", "get_time", `{"zone":"UTC"}`},
	}
	for idx, w := range want {
		got := acc[idx]
		if got == nil {
			t.Fatalf("index %d never accumulated", idx)
		}
		if g := [4]string{got.ID, got.Type, got.Function.Name, got.Function.Arguments}; !reflect.DeepEqual(g, w) {
			t.Errorf("index %d: got %q, want %q", idx, g, w)
		}
	}
}
