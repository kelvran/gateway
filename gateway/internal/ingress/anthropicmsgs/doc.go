// Package anthropicmsgs is the Anthropic Messages ingress (RFC-1,
// docs/rfcs/2026-10-09-gateway-anthropic-messages-ingress.md §1): it parses
// a /v1/messages request body into the canonical adapter.ChatRequest plus
// an adapter.Passthrough that keeps the raw body and every member the
// canonical schema did not consume (Parse), and renders canonical responses
// back into Anthropic's shapes (EncodeResponse, SSEEncoder, EncodeError).
//
// It is a leaf: it depends only on the two canonical schemas (adapter and
// streaming), never on a provider adapter or the dataplane -- the
// .go-arch-lint.yml component ingress-anthropicmsgs enforces that -- so the
// block conversions it needs are its own. Nothing imports it yet (item 11
// slice S7); the route and the dataplane wiring arrive in S9b/S10a.
package anthropicmsgs
