package dataplane

// Mid-stream runaway-completion guard, per
// docs/rfcs/2026-09-08-gateway-streaming-runaway-completion-guard.md.
//
// dataplane.go's checkRateLimit/budget.Reserve and streaming.go's
// HandleChatCompletionStream call limiter.ReserveTPM/budget.Reserve exactly
// ONCE, before a streamed request begins, and finalize's ReconcileTPM/
// Reconcile (dataplane.go, run via defer) run exactly ONCE, after the
// entire stream has finished. Between those two points, a single streaming
// request could otherwise run for an arbitrarily long time and generate an
// arbitrarily large completion with zero mid-flight check against what was
// ever reserved for it — distinct from the concurrent-request TOCTOU races
// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md already
// closed (that fix bounds N concurrent requests against each other; this
// gap is about ONE already-approved request's own real cost vastly
// exceeding its own reservation).
//
// docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md already established, and
// this file does not revisit, that no tokenizer or token-count estimator
// exists anywhere in this codebase — real usage is only known from a
// provider's own final usage frame, if one ever arrives. This guard
// therefore cannot count real tokens. It instead checks a conservative,
// provider-agnostic PROXY — accumulated output character length (see
// streamaccumulator.go's totalContentLen, the only thing genuinely
// available as chunks arrive) — against a character-length ceiling derived
// from information already known at request time.

const (
	// streamRunawayCharsPerToken is a conservative, provider-agnostic
	// PROXY for token density — explicitly NOT a real token count. ~4
	// characters per token is the commonly cited heuristic for English
	// text (e.g. OpenAI's own public tokenizer guidance: "a helpful rule
	// of thumb is that one token generally corresponds to ~4 characters
	// of text for common English text"). This constant carries the exact
	// same "proxy, not precision" caveat docs/rfcs/2026-09-05-gateway-
	// tpm-rate-limit.md already states for TPM — this guard does not
	// attempt to solve the missing-tokenizer problem any more precisely
	// than that RFC already declined to.
	streamRunawayCharsPerToken = 4

	// streamRunawayMaxTokensMultiplier is how far past
	// req.MaxTokens*streamRunawayCharsPerToken a streamed completion may
	// grow, when the client set MaxTokens at all, before the guard trips.
	// Deliberately generous — 10x, not 2x or 3x — for two independent
	// reasons, either of which alone would justify real slack:
	//
	//  1. The chars-per-token proxy is itself imprecise. Code-heavy,
	//     CJK, or otherwise non-average-English content can legitimately
	//     run at a very different density than 4 chars/token in either
	//     direction; a tight multiplier would false-positive on ordinary,
	//     legitimate traffic simply shaped differently than the
	//     heuristic assumes.
	//  2. A well-behaved upstream provider already enforces MaxTokens as
	//     ITS OWN hard cap on completion length — this guard is
	//     defense-in-depth against a misbehaving/malicious upstream (or a
	//     provider bug) that doesn't, not the primary enforcement
	//     mechanism for the ordinary case.
	//
	// 10x sits comfortably past any plausible combination of both slack
	// sources while still bounding a genuinely runaway stream to a small,
	// finite multiple of what was actually asked for, rather than leaving
	// it fully unbounded.
	streamRunawayMaxTokensMultiplier = 10

	// streamRunawayAbsoluteCharsCeiling is the fallback ceiling applied
	// when the client set no MaxTokens at all — there is then no
	// per-request baseline to multiply, so a fixed absolute floor is
	// used instead. Sized comfortably above the largest legitimate
	// SINGLE completion this gateway's configured providers can
	// plausibly produce — a large generated-code-file response, not
	// merely an "average" chat reply: as of this writing, the largest
	// publicly documented single-completion output-token limits among
	// providers this codebase has adapters for (openai, anthropic,
	// gemini, bedrock, openaicompat) sit in the ~100K-128K output-token
	// range (e.g. Anthropic's extended-output mode, OpenAI's larger
	// reasoning-output models). At streamRunawayCharsPerToken's 4
	// chars/token, 128,000 tokens is 512,000 characters; this constant
	// rounds up further, to 750,000 characters (~187,500 tokens at the
	// same proxy), so it sits comfortably above even that ceiling rather
	// than merely just above an "average" response.
	streamRunawayAbsoluteCharsCeiling = 750_000
)

// streamRunawayCharsCeiling computes the mid-stream runaway guard's
// character-length ceiling for one request, per the constants above.
// maxTokens is req.MaxTokens (adapter.ChatRequest's own field) — nil or
// <= 0 (the client set no completion-length cap) falls back to the fixed
// absolute ceiling; any positive value scales the ceiling to that specific
// request's own declared expectation instead.
func streamRunawayCharsCeiling(maxTokens *int) int {
	if maxTokens == nil || *maxTokens <= 0 {
		return streamRunawayAbsoluteCharsCeiling
	}
	return *maxTokens * streamRunawayCharsPerToken * streamRunawayMaxTokensMultiplier
}
