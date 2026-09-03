# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `gateway` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.2.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) — load-bearing for the Go module path/tag (`v0.1.0`, `v0.2.0`, ...).

## Added

- `api/gatewayevents/v1` enrichment, per `docs/rfcs/2026-09-03-gatewayevents-decision-enrichment.md`: `GatewayDecisionEvent` gains 5 new fields (7–11) closing the exact gap the original contract RFC named and deferred — `rate_limit_fail_open` (bool: the rate limiter's backend errored and the request was let through anyway), `fallback_happened`/`fallback_from_deployment`/`fallback_reason` (whether a request fell back to a second deployment, from which one, and why), and `budget_spent_usd` (the key's real cumulative spend at the moment the budget check ran, decimal-as-string, populated even on a `BUDGET_EXCEEDED` rejection). Purely additive proto change (`buf breaking` confirmed clean under the repo's `FILE` category) — no `UPGRADE.md` entry. `checkRateLimit` widens its return to `(ok, failedOpen bool)`; a new unexported `fallbackInfo` struct threads fallback detail through both the buffered inline fallback block and `streamDeploymentWithFallback`'s return; a new `budget.Tracker.SpentUSD` getter is called at the `budget.Allow` call site (never inside `finalize`) so the captured spend reflects decision-time, not finalize-time.
- Guardrails v1, per `docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md`: a new `internal/guardrail` package (pure-Go, stdlib-only, zero new `go.mod` entries) runs pre-call and post-call content checks — regex/checksum PII+secrets detection (email, phone, US SSN, IBAN with a real ISO 7064 mod-97 checksum, credit card with a real Luhn checksum, IPv4/IPv6, API-key/secret prefixes gated by Shannon entropy) plus a keyword/hidden-Unicode prompt-injection heuristic. Category-tiered fail-open/fail-closed policy on both the detection and detector-error axes (`credential`/`financial_id`/`government_id` block; `contact_info`/`network_id`/`prompt_injection` warn-with-logging) — never inherited from the rate limiter's own fail-open default. Pre-call runs identically on the buffered and streaming paths, after L1/L2/L3 cache lookup and before the router; post-call is enforcement-capable on the buffered path but **audit-only on streaming** (every chunk is already flushed to the client before a complete response exists to check — a named, accepted residual risk, not silently dropped). A guardrail policy/detector version change forces every existing cache entry to a real miss: `cache.Key`/`cache.NormalizedKey` gained a `guardrailPolicyVersion` hash input (L1/L2 have no metadata envelope to attach a stored field to), and `LexicalCandidate`/`LexicalCache.Put` gained an explicit, checked `GuardrailPolicyVersion` field (L3). New `OUTCOME_GUARDRAIL_BLOCKED` value on `api/gatewayevents/v1`'s `Outcome` enum (purely additive, `buf breaking` confirmed clean) so a guardrail rejection is never misclassified as an upstream error. New optional `guardrails:` config section (`policy_version`, `category_overrides`); a `Config.Guardrails` dependency is hard-required on `dataplane.Pipeline`, never optional. Deliberately **not** NER, **not** an ML/third-party moderation model, **not** CSAM/CBRN detection in this pass — no mature, no-cgo, no-model-file Go NER library exists today, the same class of gap Cache L3-lite already found and narrowed around for real embeddings.

## Changed

## Deprecated

## Removed

## Fixed

## Security

- Closed a real, live documentation-vs-code gap: `THREAT_MODEL.md` and `SECURITY.md` already asserted a "PII/content guardrail pre- and post-call" mitigation for Information Disclosure and Elevation of Privilege (and OWASP LLM01/LLM02 coverage) while zero guardrail code existed anywhere in the repo. Guardrails v1 (see Added, above) makes that claim true.
