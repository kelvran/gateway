package adapter

import "testing"

func TestHasOneHourCacheBreakpoint(t *testing.T) {
	oneHour := &CacheControl{TTL: "1h"}
	fiveMin := &CacheControl{TTL: "5m"}
	cases := []struct {
		name string
		req  ChatRequest
		want bool
	}{
		{"no cache control", ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, false},
		{"default ttl", ChatRequest{Messages: []Message{{Role: "user", Content: "hi", CacheControl: &CacheControl{}}}}, false},
		{"5m ttl", ChatRequest{Messages: []Message{{Role: "user", Content: "hi", CacheControl: fiveMin}}}, false},
		{"message-level 1h", ChatRequest{Messages: []Message{{Role: "system", Content: "s"}, {Role: "user", Content: "hi", CacheControl: oneHour}}}, true},
		{"part-level 1h", ChatRequest{Messages: []Message{{Role: "user", Parts: []ContentPart{{Type: "text", Text: "hi", CacheControl: oneHour}}}}}, true},
		{"tool-level 1h", ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}, Tools: []ToolDef{{Name: "t", CacheControl: oneHour}}}, true},
	}
	for _, tc := range cases {
		if got := HasOneHourCacheBreakpoint(tc.req); got != tc.want {
			t.Errorf("%s: HasOneHourCacheBreakpoint = %v, want %v", tc.name, got, tc.want)
		}
	}
}
