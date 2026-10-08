package dataplane

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// Completion envelope: the three OpenAI wire fields that identify a chat
// completion -- `id`, `object`, `created` -- stamped by the gateway on the
// completion IT delivers, never by an adapter.
//
// Why here and not in the adapters: Bedrock's Converse API has no
// response-id field at all (bedrock.go hazard #4), so its adapter honestly
// reports ID "" -- and until 2026-10-08 that empty string, plus a missing
// `object`/`created`, reached clients verbatim (live defect F7 in
// docs/upgrade-research/kelvran-deep-research-round4-discoverability-
// 2026-10-08.md). The official OpenAI SDKs tolerate it; tooling that keys
// traces or dedups retries on `id`, or asserts `object ==
// "chat.completion"`, does not. The adapter layer stays honest (no
// provider id is fabricated there, every adapter golden is unchanged); the
// dataplane, which alone knows it is the one delivering this completion,
// issues the id for it.
//
// Why not the trace id: ExtractContext honours a client-sent traceparent,
// so the client would control a supposedly unique id; one agent run that
// shares a trace across calls would repeat it; and an unsampled context
// yields the zero id.
//
// Fill-only-when-empty is the one rule every stamp below follows: an
// upstream-supplied OpenAI/Anthropic/Gemini id is preserved as-is, so a
// client can still correlate with the provider's own logs, and a cache or
// idempotency replay returns the id that was stored -- the id names the
// completion, not the HTTP exchange. Kelvran's ids are "chatcmpl-" + 32
// lowercase hex characters; OpenAI's own use a mixed-case alphabet and a
// different length, so the two are usually telling apart by eye -- but
// nothing may DEPEND on that: a client wanting provenance should read the
// response's `model` and the gateway's own request log, not parse the id.

const (
	completionIDPrefix = "chatcmpl-"
	// completionIDRandomBytes is 16 bytes (128 bits) of crypto/rand
	// entropy, rendered as 32 hex characters: collision-free at any
	// realistic volume and visibly a different shape from OpenAI's ids.
	completionIDRandomBytes = 16
	completionObject        = "chat.completion"
	completionChunkObject   = "chat.completion.chunk"
)

// clock is the pipeline time every envelope stamp reads. NewPipeline
// always sets p.now (time.Now in production, a pinned fake in tests); the
// nil fallback exists only so this package's own unit tests, which build
// bare &Pipeline{...} literals to exercise narrow call paths through
// callDeployment (capacity/TPM gates, fallback chains, retry-after), keep
// working without each setting a clock. It is not a production code path:
// every Pipeline a client request reaches came from NewPipeline.
func (p *Pipeline) clock() time.Time {
	if p.now == nil {
		return time.Now()
	}
	return p.now()
}

// newCompletionID mints one gateway-issued completion id.
func newCompletionID() string {
	var b [completionIDRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read is documented (Go 1.24+) never to return an
		// error on a supported platform. If the OS entropy source is
		// genuinely unusable the process cannot mint unique ids and must
		// not pretend to with a predictable one.
		panic(fmt.Sprintf("dataplane: crypto/rand unavailable for completion ids: %v", err))
	}
	return completionIDPrefix + hex.EncodeToString(b[:])
}

// stampCompletionEnvelope fills ONLY the empty envelope fields of resp:
// a provider-supplied id is kept, and a replayed (cached / idempotent)
// response keeps the id and created it was stored with. now is the
// pipeline clock (p.now), never time.Now, so tests can pin it.
func stampCompletionEnvelope(resp *adapter.ChatResponse, now time.Time) {
	if resp.ID == "" {
		resp.ID = newCompletionID()
	}
	if resp.Object == "" {
		resp.Object = completionObject
	}
	if resp.Created == 0 {
		resp.Created = now.Unix()
	}
}

// streamEnvelope is the identity of one streamed completion, minted ONCE
// before a stream's read loop so every chunk of the stream -- and the
// accumulated response that gets cached -- shares one id and one created
// timestamp, exactly as OpenAI's own streams do.
type streamEnvelope struct {
	id      string
	model   string
	created int64
}

// newStreamEnvelope mints the envelope for one stream served by a
// deployment whose canonical model is model.
func newStreamEnvelope(model string, now time.Time) streamEnvelope {
	return streamEnvelope{id: newCompletionID(), model: model, created: now.Unix()}
}

// stampChunk fills ONLY the empty envelope fields of c, before the chunk
// is accumulated and written. Model is filled only when the provider sent
// none (Bedrock): an OpenAI chunk keeps its own snapshot model name, so
// chunks and the buffered response can still disagree on `model` for that
// provider -- a recorded follow-up, deliberately not changed here.
func (e streamEnvelope) stampChunk(c *streaming.ChatCompletionChunk) {
	if c.ID == "" {
		c.ID = e.id
	}
	if c.Model == "" {
		c.Model = e.model
	}
	if c.Object == "" {
		c.Object = completionChunkObject
	}
	if c.Created == 0 {
		c.Created = e.created
	}
}

// stampResponse fills the accumulated response of this stream from the
// same envelope, so the cached JSON (and any later fake-streamed replay of
// it) carries the id and created every live chunk already carried.
func (e streamEnvelope) stampResponse(resp *adapter.ChatResponse) {
	if resp.ID == "" {
		resp.ID = e.id
	}
	if resp.Object == "" {
		resp.Object = completionObject
	}
	if resp.Created == 0 {
		resp.Created = e.created
	}
}
