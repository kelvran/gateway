# Why the L3 cache is lexical and hard-gated

This page explains the reasoning behind Kelvran's third cache layer: why it matches on word shingles instead of embeddings, why every candidate must clear a set of exact-match gates rather than a similarity score, what the cited attack research does and does not show, and why the gate has no configuration knob. It is for operators and contributors who want to understand the design before they tune, extend or question it. For the settings themselves see [the caching how-to](../how-to/caching.md) and [the configuration reference](../reference/config.md).

## The problem the gate answers

[PRD.md](../../PRD.md) names "caching is a coin flip, not a judgment" as one of the three gaps Kelvran exists to close. Every semantic cache the project surveyed decides reuse on a single similarity threshold. "Close enough" says nothing about whether the cached answer is still *true* for the new question.

Three papers turned that observation from taste into a requirement. The [L3-lite RFC](../rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md) records that all three were confirmed real before it was written, one of them (CacheAttack) spot-checked directly against its arXiv abstract:

- **CacheAttack** (Zhang et al., ICML 2026) reports an 86% response-hijack rate against similarity-keyed caches, 90.6% against agentic tool invocation, with a financial-agent case study. Its structural claim matters more than the numbers: cache-hit locality and collision resistance are mutually exclusive goals, and "no lossless solution" exists.
- **"When Cache Poisoning Meets LLM Systems"** (Wu et al., NDSS 2026) reports 82-89% poisoning success against production semantic-cache integrations. Every defence that inspected the *query alone* failed. The one check that worked (F1 0.87) compares the cached *response* against the incoming query.
- **KeyPooling** (Sun et al., arXiv:2608.17485) shows cross-tenant leakage through pooled upstream credentials in all five gateways it studied.

[THREAT_MODEL.md](../../THREAT_MODEL.md) turns these into three Cache STRIDE rows (Tampering, Information Disclosure, Elevation of Privilege) and commits to an entity/number/date hard-gate, a freshness model, write-time provenance and a similarity floor "never below ~0.9". [AGENTS.md](../../AGENTS.md) adds the operational rule: never ship an L3 change that removes or weakens the entity/freshness hard-gate in favour of a bare similarity threshold.

The RFC also states what the papers do *not* show. None of them tests entity/number/date extraction as a defence. That pattern is industry practice layered on top of the threat model's own requirement. The single mitigation the papers do validate, a query-to-response consistency classifier, is not implemented, because it needs the same ML infrastructure the project declined to adopt. The gap is recorded as an unresolved question, not glossed over.

## Why lexical MinHash, not embeddings

The RFC's title says "L3-lite" because the first implementation deliberately stopped short of semantic matching. Three constraints, each checked against this repository rather than assumed:

**A hit must never touch an upstream.** [gateway/ARCHITECTURE.md](../../gateway/ARCHITECTURE.md) places the cache lookup before the router so that a hit costs no provider call. An embedding API call on every lookup *is* an upstream call. It would void the one guarantee the subsystem exists to provide, on both the read and the write path.

**The binary must stay static.** The in-process alternative, a Go ML runtime, was cgo-only or pre-v1 at the time. cgo breaks the scratch-image build, and an 80-100 MB model file is a different artifact from a roughly 5 MB image. [ADR 0002](../decisions/0002-cache-embedded-in-gateway.md) keeps the cache inside the gateway binary; a model sidecar would reopen that decision.

**The gate work is the same either way.** Whatever matches candidates, every candidate still needs the entity, freshness and provenance checks. Building those first against a cheap matcher wastes nothing if an embedding matcher is ever added.

So `gateway/internal/cache/lexical.go` imports only the standard library. It splits the L2-normalised text, which is the JSON serialisation of the whole message array (every role, the system prompt included, see `normalizeMessages` in `dataplane.go`), into 3-word whitespace-delimited shingles, hashes each with FNV-64a, and computes a 128-value MinHash signature using `a*x+b mod 2^64` permutations whose coefficients come from `splitmix64` (stable across Go versions, unlike `math/rand`) with `a` forced odd so each permutation is a bijection. Similarity is the fraction of matching signature positions, an unbiased Jaccard estimate. Reusing L2's normalised text means L3 inherits L2's refusal to fold case or collapse code indentation instead of inventing a second, less careful text pass.

This is lexical near-duplicate matching: typos, a swapped word, light reordering. It is not paraphrase understanding, and the project says so in the package comment, the RFC, [DECISIONS.md](../../DECISIONS.md) and [gateway/ARCHITECTURE.md](../../gateway/ARCHITECTURE.md). Older prose in a few documents still calls L3 "semantic"; the code is the authority.

## What "hard gate" means here

A bare threshold asks "how similar?" and serves anything above a number. A hard gate asks a series of yes/no questions and serves only if every answer is yes. `checkLexicalCache` in `gateway/internal/gateway/dataplane/dataplane.go` is that series, kept in one function on purpose so the single highest-consequence check in the codebase is auditable in one place rather than buried inside a swappable cache implementation. The RFC rejected the alternative for exactly that reason, applying the boundary `gateway/internal/cache/key.go` documents: `internal/cache` never imports `internal/adapter`, so schema-aware helpers such as `normalizeMessages` (L2's) and the gate itself live in `dataplane`.

Every rejection falls through to a real upstream call. There is no "partial credit" branch, and a search error fails closed rather than skipping the gate.

The checks fall into three families.

**Before any search.** A query is first checked for volatility. If any *user-role* message matches `weather|price|stock|score|today|current|currently|now|latest`, L3 is skipped outright. Time-sensitive questions have no honest freshness budget, so this is a bypass, not a score adjustment. The match is scoped to user messages only: a system prompt is static across every request through a deployment, so a system prompt that mentions "current stock prices" must not disable L3 for the whole deployment.

**Set-equality gates on the query text.** The entity/number/date fingerprint (`Fingerprint` in `gateway/internal/gateway/dataplane/entities.go`) collects every regex-matched number (including currency prefixes and percent suffixes), every date, and every run of one or more capitalised words as a coarse proper-noun proxy, skipping each message's first word (sentence-initial capitalisation is grammar, not an entity, and counting it would fail near-verbatim rephrasings that merely start with a different question word) and the standalone pronoun `I`. It runs over every message, system prompt included. The query's set must *equal* the stored set: not overlap, not subset, not a score. A query mentioning `$92` can never match an entry written for `$250`. The extractor is biased toward over-inclusion because the two failure modes are not symmetric: a false positive costs hit rate, a false negative serves a wrong answer. A second set-equality gate, `NegationFingerprint`, compares the closed list of negation particles and contractions present (`not`, `never`, `without`, `don't`, `can't` and so on, after normalising typographic apostrophes), so "take X" and "don't take X" can never share an entry even though their entity fingerprints are identical.

**The freshness/risk model and provenance equality.** `freshnessRiskModel` rejects a candidate older than 24 hours, below 0.9 similarity, or written for a different model ID. After it, a run of exact string-equality gates compares provenance captured at write time against the current request: guardrail policy version, response-format fingerprint, prompt fingerprint, reasoning-blocks fingerprint, thinking-binding mode, tools/tool_choice fingerprint and, since gateway/v0.19.0, the thinking-configuration fingerprint. Both-empty counts as a match; a fabricated default never does.

All of these inputs are stored on `LexicalCandidate` when the entry is written, and `writeCache` writes L1, L2 and L3 together on every miss. The gate therefore compares what the request *is* against what the stored request *was*, never against a reconstruction.

## Why the gate kept growing

L1 and L2 get most of these checks for free: every request-semantic field is hashed into their exact keys, so a request that differs in `response_format` simply has a different key. L3 has no key. Its match is a fuzzy search over a signature, so every field that L1/L2 fold into a hash has to become an explicit gate in `checkLexicalCache`. [DECISIONS.md](../../DECISIONS.md), the RFCs and the gateway changelog record each addition as a closed collision class rather than a feature:

- 2026-09-03: the original three gates (volatility bypass, entity fingerprint, freshness/risk model) plus the guardrail-version gate from the [guardrails RFC](../rfcs/2026-09-03-guardrails-pii-regex-classifier.md). A policy or detector version bump forces every existing entry to miss: L1/L2 through the key hash, L3 through the stored version.
- 2026-09-11: response-format and prompt-identity gates, after an audit found a schema-JSON request could be served a plain-text entry, and a prompt version bump whose new text was near-verbatim to the old one could resurrect retired content.
- 2026-09-12: the negation-particle gate, and the reasoning-blocks gate from the [reasoning-content RFC](../rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md), because replayed reasoning is causally read by the model and changes output.
- 2026-09-24: the thinking-binding-mode gate, the same RFC's addendum, so a caller who asked for strict reasoning continuity is never served an entry written under a looser mode.
- 2026-09-25: a read/write asymmetry fix. `checkLexicalCache` had searched L3 by bare virtual-key ID while `writeCache` wrote by the end-user-scoped partition key, so entries scoped to an end user were unreachable, or legacy unscoped entries were reachable across end users. Both paths now use the same scope key.
- The tools/tool_choice gate from the [cache-key tools RFC](../rfcs/2026-10-08-gateway-cache-key-tools-fingerprint.md): first shipped in `gateway/v0.18.0`. Identical messages with `tool_choice: "required"` and `tool_choice: "none"` had collided at every layer.
- The thinking-configuration gate ([ingress RFC](../rfcs/2026-10-09-gateway-anthropic-messages-ingress.md) §3, item 11 slice S4): since gateway/v0.19.0. A reply made without extended thinking is never served to a near-duplicate that asked for it, or the reverse.

The rule that emerged, stated in the tools RFC: every new request-semantic field folds into L1, L2 *and* L3, never into a subset. The reasoning-content work learned this the hard way twice: `ReasoningBlocks` reached L1/L2 only by accident of message serialisation while L3 had no gate, and the first `ThinkingBindingMode` pass folded the field into no layer at all.

Tenant isolation is structural for the same reason. `gateway/internal/cache/inprocess/lexical.go` keeps one LRU bucket per tenant and searches only that bucket. It is never a shared list filtered by tenant ID after the fact, which is the KeyPooling mitigation THREAT_MODEL.md describes as "baked into the vector-index partition itself". The accepted cost is that memory scales with active tenants times `max_entries`, rather than one shared cap as in L1/L2.

## What the gate does not do

**It does not tolerate paraphrase, and a shared system prompt can carry unrelated questions over the floor.** A live dry-run on 2026-09-13 measured the first half: a genuine paraphrase of a 70-word explanation scored about 0.0078 Jaccard against the original (a clean miss, nowhere near the 0.9 floor), while the same text with one synonym swapped scored about 0.9143 exact Jaccard and 0.9140625 (117 of 128 positions) through the real MinHash signature (a clean hit, just above the floor). `Search` returns the five nearest signatures with no floor of its own, so a paraphrase candidate is still checked by the entity and negation gates (and counted under `entity_mismatch` and `negation_mismatch`) before `freshnessRiskModel` rejects it on similarity; the floor guarantees it is never served, not that the earlier gates never see it. The second half follows from what is shingled: the signature is computed over the L2-normalised JSON of the whole message array, so a system prompt contributes shingles to every request that carries it. Jaccard over shared-plus-distinct shingles is roughly S/(S+2Q); with a static 200-word system prompt, two unrelated user questions of about ten words score about 0.91 exact Jaccard (what the gate actually compares is a 128-position MinHash estimate of that value, whose standard error near 0.91 is about 0.027, so the estimate for this case lands on either side of the 0.9 floor from one pair of questions to the next), above the floor, and at 300 words about 0.94 (estimate around 0.95 to 0.96); at 100 words the same pair still misses, at about 0.83. Those candidates reach the entity, negation and provenance gates, and if neither question contains a number, a date, a capitalised word after its first word or a negation particle, every gate passes and the first answer is served for the second question until the L3 TTL expires. Under a long system prompt the set-equality gates, not the similarity floor, are the defence. That is the trade-off the project accepted: typo and near-verbatim tolerance, which is what defeats a CacheAttack-style near-miss collision, in exchange for no semantic hit-rate uplift and a floor that does most of its work only when the shared prefix is short.

**It does not catch an antonym flip without a negation particle.** "Withhold the drug" and "administer the drug" carry no negation particle, so both negation fingerprints are empty and equal. The 2026-09-08 DECISIONS.md entry tried three designs for this case and rejected all three: a negation-cue gate alone does not fire, a curated antonym list is unbounded for a general-purpose gateway, and widening the entity fingerprint to every content word breaks the entity-free-paraphrase test. `TestNegationFingerprintDoesNotCatchAntonymVerbFlip` in `gateway/internal/gateway/dataplane/entities_test.go` exists to fail if that boundary ever silently moves. The 2026-09-12 negation gate was added as a narrower, separate fix and explicitly does not reopen the decision.

**It does not transfer the 0.9 floor with any calibration.** THREAT_MODEL.md specified "~0.9" for embedding-cosine similarity. The code applies the same number to a Jaccard estimate, which has different statistical properties. The constant's own doc comment and the RFC both call this an unvalidated transfer. The 2026-09-13 numbers show it behaves sensibly on one pair of examples; they are not a calibration against traffic.

Not available today:

- An embedding-based semantic L3. Deferred until miss telemetry shows a real paraphrase-miss rate. PRD.md records that this gate has not fired.
- The NDSS-validated query-to-response consistency classifier.
- Per-content-type similarity thresholds or staleness tiers. The RFC asked for them; the code ships one global 0.9 and one global 24 h and defers tiering until calibration data exists.
- `LexicalCache.Delete`. `POST /admin/cache/erase` removes L1 and L2 entries only; an L3 entry leaves only by TTL. [SECURITY.md](../../SECURITY.md) and [the Admin API reference](../reference/admin-api.md) state this.
- A Redis-backed or otherwise shared L3. A [design RFC](../rfcs/2026-09-11-gateway-redis-backed-cache-design.md) exists; the only implementation is in-process and per replica.
- Cross-instance L3 coherence wired into the live pipeline. The [cross-instance telemetry RFC's](../rfcs/2026-09-07-cache-cross-instance-telemetry.md) correlation unit is a standalone analysis tool.

## Why there is no knob

The `cache:` section in [gateway/config.example.yaml](../../gateway/config.example.yaml) accepts `ttl_seconds`, `max_entries` and `jitter_fraction` for each of the three layers, and nothing else. The similarity floor, the staleness budget, the shingle size, the signature length and the candidate count (`l3SearchK = 5`: only the five nearest signatures in the tenant's bucket are ever gated, so a valid entry can be crowded out by closer candidates that fail a gate) are Go constants in `gateway/internal/gateway/dataplane/dataplane.go`. The volatility pattern and the negation-particle list are compiled regular expressions in code.

This is deliberate. AGENTS.md forbids weakening the gate "in favor of a bare similarity threshold". A `min_similarity` knob would let an operator do exactly that at deploy time, outside code review, and the project's own survey found that mainstream products ship that knob with no entity or freshness gate behind it. Exposing it would move Kelvran from the conservative position to the mainstream one. Changing a constant requires a commit, a review, and a release, which is the intended friction.

One consequence worth knowing: the configured L3 TTL (default 5 minutes) usually expires an entry long before the 24-hour staleness budget does. The budget is a ceiling that no TTL setting can lift, not the day-to-day expiry mechanism.

## Observing the gate

The three original gates and the negation gate emit one dimensioned OTel counter, `kelvran.cache.l3.gate_outcome`, with attributes `kelvran.cache.l3.gate` (`volatile_bypass`, `entity_mismatch`, `negation_mismatch`, `freshness_risk_model`), `kelvran.cache.l3.outcome` (`pass` or `reject`) and `kelvran.instance.id`. One instrument rather than four lets a single query compare reject rates across gates, which is the ablation question an operator asks when hit rate is lower than expected. The later string-equality gates are deliberately not counted: the counter exists for the per-gate ablation question the 2026-09-06 research posed about the three original gates (the negation gate joined it on 2026-09-12), and five of the later gates' comments in `checkLexicalCache` opt out of the counter explicitly; the response-format and prompt-identity gates inherit the guardrail gate's convention by reference. A served L3 hit also carries `kelvran.cache.layer`, `kelvran.cache.similarity` and `kelvran.cache.age_ms` on its span, so a wrong downstream action can be traced to the exact similarity and age that produced it. Names and export forms are in [the metrics reference](../reference/metrics-and-logs.md) and [TELEMETRY.md](../operations/TELEMETRY.md).

## Trade-offs the project accepted

- **Hit rate for correctness, every time.** Set equality instead of overlap, over-inclusive entity extraction, cosmetic tool-schema differences causing misses: each choice lowers hit rate so that the failure mode is "called upstream unnecessarily" rather than "served a wrong answer".
- **A blunt gate over a clever one.** No gazetteer, no NER model, no antonym list. Every gate input is a regex or a string comparison that a reviewer can read in one sitting.
- **Candour about the ceiling.** CacheAttack's "no lossless solution" applies to this design too. The gates reduce collision risk; they do not eliminate it, and the strongest published mitigation is not built.
- **Memory per tenant.** Structural partitioning costs more than a shared cap. The project treats isolation as a security requirement, not a style choice.

## Related reading

- [Security model](security-model.md) for how the cache rows fit the wider threat model.
- [Architecture](architecture.md) for where the L3 lookup sits in the request lifecycle.
- [Design decisions](design-decisions.md) for the other places this repository chose the narrower implementation.
- [Caching how-to](../how-to/caching.md) for the settings an operator can actually change.
