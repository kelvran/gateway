package adapter

// Registry is a lookup map of every provider adapter this binary knows
// about, keyed by Adapter.Name(). Task 4 of the scaffolding plan requires
// all five adapters (openai, anthropic, gemini, bedrock, openaicompat) to
// compile registered together in a lookup map; this is that map's home,
// since it depends only on the Adapter interface this package already
// defines and stays a leaf the concrete adapter packages import into,
// never the reverse.
//
// NOTE: constructing the Registry lives in cmd/gateway (Task 8), not here,
// to avoid this package importing every concrete adapter package — that
// would invert the intended dependency direction (adapters depend on the
// canonical types in this package, not the other way around). This map
// type alias exists purely so callers share one vocabulary for "a lookup
// map of adapters by name."
type Registry map[string]Adapter
