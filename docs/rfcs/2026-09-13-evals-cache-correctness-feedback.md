# RFC: Evals cache-correctness feedback loop for L3 semantic-hit wrong-answer rate — design only, no code

## Status

Design-only, explicitly deferred pending its own named trigger — real production traffic
generating enough L3-lite cache hits to sample meaningfully. `DECISIONS.md`'s `[2026-09-12]`
Round 7 requirements-traceability audit entry (Phase 9) already disclosed this exact gap as its
one `not_yet`-verdict finding: `PRD.md:40`'s "near-zero measured wrong-answer rate" success
metric has no corresponding measurement anywhere, and named its own trigger in the same
sentence — "the real trigger is a correctness-feedback pipeline that doesn't exist yet
(sampling L3 cache-hit-served responses through Evals' judge/panel machinery, joinable to the
existing `kelvran.cache.hit`/`lookup` counters)."

That same entry also says "no RFC currently scopes this, and none should be written
speculatively ahead of a real trigger." This RFC does not reverse that judgment — it ships no
code, and the trigger named above still has not fired. It mirrors the exact "designed in full,
deferred" shape this project already applies elsewhere once a finding is fresh and cheap to
record even though implementation pressure doesn't yet exist:
`docs/rfcs/2026-09-11-gateway-redis-backed-cache-design.md` and
`docs/rfcs/2026-09-11-gateway-mcp-outbound-credential-design.md` both scope a "how" in full while
explicitly deferring the "when." Applying that same established pattern to the one gap Round 7
Phase 9 flagged but declined to scope is a deliberate, disclosed choice this RFC makes rather
than a silent inconsistency — see Alternatives Considered for why "wait, write nothing" was
rejected on the same grounds those two prior RFCs already rejected it. Kelvran's L3-lite
entity/freshness hard-gate (`gateway/internal/cache`, protected by `AGENTS.md`'s "never weaken
this" rule) continues to architecturally prevent wrong answers exactly as it does today; nothing
in this RFC changes that, and nothing here is built.

## Date

2026-09-13

## Author(s)

Session agent (Claude Code), drafting the design `DECISIONS.md`'s Round 7 Phase 9 named but
explicitly declined to scope itself.

## Summary

Kelvran measures cache hit rate (`kelvran.cache.lookup`, per
`docs/upgrade-research/cache-cost-observability-2026-09-11.md` Finding 1) and instruments the
L3-lite hard-gate's own pass/reject decisions (`kelvran.cache.l3.gate_outcome`, per
`docs/upgrade-research/cache-2026-09-06.md` Finding 1) — but nothing anywhere joins a real,
served L3 cache hit to a ground-truth correctness verdict. The gate-outcome counter measures how
often the gate *intervenes*; it says nothing about whether the hits the gate *lets through* are
actually right. `PRD.md:40`'s success metric asks for exactly that second number, and it does not
exist.

This RFC designs — without implementing — a sampling-and-judging pipeline built entirely from two
already-shipped subsystems: the existing `GatewayDecisionEvent` / object-storage ingestion
pipeline (`docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md`) to carry a small,
opt-in, sampled slice of L3-hit content out of the gateway process, and Evals' existing
LLM-judge/panel machinery (`evals/evals/judge/llm_judge.py`) — driven by a new, narrower rubric,
not the existing reference-guided one — to turn that sample into a real, Wilson-bounded L3
wrong-answer-rate statistic. Concretely, it proposes: (1) three new additive
`GatewayDecisionEvent` fields — an unconditional `cache_layer`, and two opt-in, L3-hit-only content
fields; (2) a new, disabled-by-default gateway-side sampling gate nested under the existing
`CacheL3Config`, mirroring the Vector shipper's already-real 1-in-10 `OUTCOME_OK` sampling
precedent; (3) a new, self-consistency judge rubric ("is this served response still correct for
the *new* request," not "does this output match a reference answer") alongside — not replacing —
the existing one; and (4) a minimum-viable `evals cache-correctness-report` CLI command mirroring
`cost-report`'s own shape, with an optional hook into the existing `trend_alert.py`
`TrendSnapshot`/`trend alert` machinery. None of this is implemented by this RFC. The real,
still-unfired trigger is production traffic generating enough L3-lite cache hits to make a sample
statistically meaningful — see Status and Unresolved Questions.

## Motivation

`PRD.md:40` states the requirement in one sentence, and is explicit that the two numbers travel
together: "Cache hit rate on repeated/near-duplicate traffic, with a **near-zero measured
wrong-answer rate** from semantic hits (the two numbers must be reported together — a hit rate
without a correctness number is not a success metric here, it's a vanity metric)." "Semantic
hits" is this codebase's own name for L3 — the only layer whose match criterion is fuzzy
near-duplicate similarity rather than exact/normalized equality (`docs/rfcs/2026-09-03-cache-l3-
lite-lexical-hard-gated.md`). `docs/operations/TELEMETRY.md`'s own Key SLIs/SLOs section already
names this precise absence as a real, disclosed gap, not a new observation: "semantic-hit
false-positive rate (this last one requires the correctness-tracking discipline `PRD.md`'s
Success Metrics section already commits to...)" (`docs/operations/TELEMETRY.md:31`).

**Why the already-shipped gate-outcome metric does not close this gap.** It would be easy to
mistake `kelvran.cache.l3.gate_outcome` (`gateway/internal/telemetry/telemetry.go:282-332`) for
already answering this. It does not, and the research that motivated it says so directly:
`docs/upgrade-research/cache-cost-observability-2026-09-11.md` Finding 2 scoped its own
"build_now" recommendation narrowly to "exposing [the gate's] own trigger/rejection rate as a
first-class metric" — a per-gate ablation signal for `volatile_bypass`/`entity_mismatch`/
`freshness_risk_model` (and, per `DECISIONS.md`'s `[2026-09-12]` entry, `negation_mismatch`),
answering "how often does the gate say no." That is a property of the *gate's own decision
logic*, entirely computable in-process at request time with no external ground truth. It is a
different question from "of the hits the gate said yes to, how many were actually wrong" — that
second question requires a ground-truth judgment of the served content against the request that
triggered it, which no in-process, gate-internal signal can ever supply. Both are real, useful,
and already correctly scoped as distinct by the research that produced them; this RFC is about
the second one, which remains fully unaddressed.

**Why no correctness judgment is possible today, even in principle.** This is not merely "no CLI
command exists" — no *data* that could ever answer this question reaches `evals` today, at any
layer:

- `GatewayDecisionEvent` (`api/gatewayevents/v1/gatewayevents.proto`) carries no cache-layer field
  at all. `kelvran.cache.hit`/`kelvran.cache.layer`/`kelvran.cache.similarity`/`kelvran.cache.age_ms`
  exist only as OTel span attributes (`gateway/internal/telemetry/result.go:72-94`,
  `RecordChatCompletionResult`), populated from the request-scoped `cacheProvenance` struct
  (`gateway/internal/gateway/dataplane/dataplane.go:994-1007`) — confirmed directly against the
  live `.proto` file (read in full for this RFC): fields 1 through 14 cover trace/span identity,
  outcome, fallback, budget, `agent_run_id`/`cost_usd`/`savings_usd` — nothing about which layer,
  if any, served a hit.
- No prompt or completion content exists anywhere in the schema, by explicit design.
  `evals/evals/ingestion/mapping.py`'s own module docstring states this directly: "No
  prompt/completion content anywhere in this schema" — `Run.stdout` and `EvalCase.reference` are
  left empty/`None` rather than fabricated when mapping an ingested event. This is the correct,
  deliberate consequence of `docs/operations/TELEMETRY.md`'s Privacy & Redaction stance: "Prompt/
  completion content in trace events is opt-in, not default-on — this is a deliberate privacy
  stance, not an oversight" (`docs/operations/TELEMETRY.md:47-49`). This RFC does not propose
  changing that stance — see Detailed Design for how the new fields stay opt-in and stay scoped
  to only the narrow slice of traffic this measurement genuinely needs.

Without a `cache_layer` signal, `evals` cannot even identify which rows in an ingested
`gatewayevents_v1` stream are L3 hits at all, let alone judge them. Without some form of opt-in
content, there is nothing for a judge to look at even once the L3 hits are identified. Both gaps
have to close together for this metric to become measurable at all — which is exactly why Round 7
Phase 9 named a *pipeline*, not a single field, as the trigger.

## Detailed Design

### Scope: L3 only, and only L3, matching `PRD.md:40`'s own wording

L1 (exact-match) and L2 (normalized-match) cache hits are the result of a deterministic equality
check (byte-identical or whitespace/case-normalized-identical requests) — a wrong answer served
from either would mean a bug in the match/normalization logic itself, a different failure class
from what L3's fuzzy near-duplicate matching can produce, and one already covered by ordinary
correctness testing of that logic (`gateway/internal/cache`'s own unit/golden tests), not a
statistical sampling problem. `PRD.md:40`'s literal wording — "wrong-answer rate... from
**semantic hits**" — already scopes this to L3 specifically; L1/L2 hits are deliberately out of
scope for the mechanism this RFC designs, and remain so unless a future finding shows otherwise
(see Alternatives Considered).

### 1. New, additive `GatewayDecisionEvent` fields

Mirroring `docs/rfcs/2026-09-12-gateway-cache-savings-agent-attribution.md`'s own precedent
(name an existing in-process value on the existing contract, don't invent a new capture point) —
`cacheInfo.Layer` (`gateway/internal/gateway/dataplane/dataplane.go:1002`) already exists at
exactly the `finalize` construction site `GatewayDecisionEvent` is built at
(`dataplane.go:2100-2127`); it is simply never named on the wire today.

```protobuf
  // Added per docs/rfcs/2026-09-13-evals-cache-correctness-feedback.md
  // (design-only at the time of writing -- see that RFC's own Status).
  //
  // cache_layer: "" on a miss (proto3 default -- a real, meaningful
  // value here, mirroring cost_usd's own "0 is real" convention, not
  // savings_usd's ""-is-not-a-hit convention, since a MISS is a real,
  // common, non-error outcome, unlike "never a cache hit" on a request
  // that in fact never had one). "L1"/"L2"/"L3" on a hit, the exact
  // same cacheInfo.Layer value already on the OTel span's
  // kelvran.cache.layer attribute -- this field is the first place
  // that value exists on the one contract built for durable, offline
  // analysis; without it, evals cannot even identify which ingested
  // rows are L3 hits, let alone judge them.
  string cache_layer = 15;

  // l3_sample_request_content / l3_sample_response_content: OPT-IN,
  // populated ONLY when ALL of the following hold for this specific
  // request: (a) cache_layer == "L3" (see this RFC's own Detailed
  // Design "Scope" section for why L1/L2 are excluded), (b) the
  // gateway's own CacheL3Config.CorrectnessSampling.Enabled is true
  // (per-deployment operator opt-in, default false -- see below), and
  // (c) this request's own trace_id was selected by that sampling
  // gate's own SampleRate decision. "" (the proto3 default) on every
  // other row, including every L1/L2 hit, every miss, and every
  // L3 hit that simply wasn't sampled -- "" here means "not applicable
  // or not sampled," never "sampled and empty," since a genuinely
  // empty request/response is not a real case this schema needs to
  // distinguish.
  //
  // l3_sample_request_content is the serialized content of the NEW
  // incoming request that triggered this L3 hit -- via the same
  // serializeMessages(req.Messages) helper dataplane.go already uses
  // for cache-key construction and pre-call guardrail scanning
  // (dataplane.go:2329-2335), not a new serialization path.
  // l3_sample_response_content is the content of the response Kelvran
  // actually served from the cache for this request. Both are needed,
  // not just one -- a correctness verdict on "is this served response
  // still right" is meaningless without knowing what was actually
  // asked; a bare cache_layer=="L3" flag with no content tells evals a
  // hit happened, not whether it was right.
  string l3_sample_request_content = 16;
  string l3_sample_response_content = 17;
```

This is additive and non-breaking per the same `buf breaking` FILE-level rule the two prior
same-shaped RFCs (`agent_run_id`/`cost_usd`, `savings_usd`) already relied on — no `v1` version
bump.

### 2. Gateway-side opt-in sampling gate, nested under the existing `CacheL3Config`

`gateway/internal/gateway/controlplane/config.go:360-367` already defines `CacheL3Config`
(`TTLSeconds`, `MaxEntries`, `JitterFraction`) as the one place L3-lite's own operator-facing
config lives, nested inside `CacheConfig` (`config.go:376-385`). A new nested sub-config extends
this exact shape rather than introducing a new top-level config section:

```go
// CacheL3CorrectnessSamplingConfig configures the opt-in content-capture
// sampling gate this RFC designs. A zero-valued struct (Enabled: false)
// means the default, current behavior exactly -- l3_sample_request_content/
// l3_sample_response_content are never populated, matching this
// codebase's existing "opt-in, not default-on" content-capture stance
// (docs/operations/TELEMETRY.md's Privacy & Redaction section).
type CacheL3CorrectnessSamplingConfig struct {
    Enabled bool
    // SampleRate is a 1-in-N decision, mirroring the Vector shipper's
    // own `rate: 10` convention for OUTCOME_OK sampling
    // (docs/operations/vector-gatewayevents-s3.yaml) -- not a fraction,
    // to reuse the exact same mental model an operator already has for
    // the sibling sampling decision downstream of this one.
    SampleRate int
}

type CacheL3Config struct {
    TTLSeconds int
    MaxEntries int
    JitterFraction float64
    CorrectnessSampling CacheL3CorrectnessSamplingConfig
}
```

The sampling decision itself must happen gateway-side, not at the Vector shipper: Vector only ever
sees whatever the gateway's own stdout JSON log line already contains
(`vector-gatewayevents-s3.yaml`'s own comment: "gatewayevents_v1 lines are small (no
prompt/completion content...)"). If the gateway process never captures `req.Messages`/the served
response into the event at construction time, no downstream shipper can recover it. The decision
would be made once per real L3 hit (`cacheInfo.Layer == "L3"`), via a deterministic function of
`trace_id` (or the request's own L1 key) mod `SampleRate` — the same "consistent decision per
identifier, not per-line-random" property the Vector shipper's own `key_field: "traceId"`
sampling transform already relies on (`vector-gatewayevents-s3.yaml:88-95`), so a retried/
duplicated event for the same request always samples the same way.

### 3. Interaction with the existing Vector-shipper 1-in-10 `OUTCOME_OK` sample

An L3 cache hit is still `OUTCOME_OK` (a successfully-served response) — today's shipper config
already subjects it to the existing 1-in-10 sample (`vector-gatewayevents-s3.yaml`'s
`gatewayevents_sample` transform, `exclude: '.outcome != "OUTCOME_OK"'`). Left unchanged, the two
sampling decisions would compound uncontrollably: a row deliberately selected by the new
gateway-side gate (an operator-tuned, intentional sample size) could still be silently dropped by
the unrelated, independently-tuned shipper-level sample. The minimum-viable fix mirrors the
existing "always keep every non-OK outcome" rule with a second always-keep clause for any row
this RFC's own sampling gate already selected:

```
exclude: '.outcome != "OUTCOME_OK" || .l3SampleResponseContent != ""'
```

(`l3SampleResponseContent` — protojson's camelCase rendering of `l3_sample_response_content`,
matching this same file's own `virtualKeyId`/`traceId` field-name convention.) This keeps the two
sampling decisions independent and each fully attributable to its own operator-tuned rate, rather
than one silently attriting the other.

### 4. Evals ingestion — `decode.py`/`object_store.py` need zero changes; `mapping.py` is not on this path at all

`decode_gateway_decision_event` (`evals/evals/ingestion/decode.py:16-19`) is a bare
`Parse(raw, GatewayDecisionEvent())` call — field-name-agnostic once `make gen-proto` regenerates
`gatewayevents_pb2.py`, exactly the same "no change" claim the accepted savings RFC already made
for the same reason. `object_store.py`'s `list_object_keys`/`iter_object_lines` are scheme-dispatch
helpers with no knowledge of `GatewayDecisionEvent`'s field set at all — also unaffected.

`ingestion/mapping.py`'s `gateway_decision_event_to_eval_case_and_run` is deliberately **not**
part of this design. That function exists to promote an ingested event into the
`EvalCase`/`Run`/regression-corpus pipeline (`evals promote`) — a different workflow from directly
computing an aggregate statistic over a filtered event stream. `cost-report`
(`evals/evals/cli.py:1600-1672`) already establishes the precedent for the latter: it reads
`list_object_keys`/`iter_object_lines`/`decode_gateway_decision_event` directly and accumulates a
running total itself, bypassing `mapping.py` entirely. The command this RFC proposes (below) does
the same.

### 5. A new judge rubric — not a reuse of the existing reference-guided one

`judge()`/`build_judge_prompt()` (`evals/evals/judge/llm_judge.py:256-268, 533-608`) hardcode a
**reference-answer comparison**: the prompt is always framed as "Reference answer: {reference} /
Candidate output: {output}." The new request that triggered an L3 hit is a *question*, not a
reference *answer* — passing it as `reference` would produce an incoherent prompt (a "Reference
answer:" label wrapped around a question) and would silently misuse the one existing rubric this
codebase has for a judgment it was never designed to make. This is exactly the design question the
task instructions named explicitly, and the honest answer is that a new rubric is required, not a
bare reuse.

The concrete shape recommended (not committed — see Unresolved Questions on final function
placement): a new prompt template and a thin sibling function in `llm_judge.py`, e.g.
`build_cache_correctness_prompt(new_request: str, served_response: str) -> str`, framed as a
self-consistency check with no external reference: "Given this NEW REQUEST and this SERVED
RESPONSE (returned from a similarity-matched cache entry, not generated fresh for this exact
request), is the served response still a correct, appropriate answer to the new request?" —
preserving the existing rubric's own bias-mitigation discipline (CoT-forcing: reasoning before
verdict; a `QUOTE`-then-`VERDICT` grounding requirement, per
`docs/rfcs/2026-09-09-evals-quote-grounded-verdict.md`) so this new rubric doesn't regress below
the bar the existing one already clears. A corresponding `judge_cache_correctness()` function
would reuse — not reimplement — `parse_judge_response`, `PanelVote`, `reduce_panel_votes`, and
`quote_is_grounded` as-is: none of those four are coupled to the existing prompt's specific
wording, only to the shared `REASONING:`/`QUOTE:`/`VERDICT:` response contract every judge prompt
in this module already produces, so the new rubric inherits panel support (`--llm-judge-panel`'s
existing 2-judge Bedrock Sonnet-5+Haiku-4.5 pair, fail-closed tie-break) for free.

No new `Score.scorer_type` value is needed. `ScorerType` (`evals/evals/models.py:126`) is already
`Literal["deterministic", "llm_judge", "llm_judge_panel"]` — a cache-correctness verdict genuinely
*is* an `llm_judge`/`llm_judge_panel` call mechanically (same call/panel/reduce machinery, same
`Score` shape), just with a different prompt behind it. The existing `rubric_axis` field
(populated today by `--judge-axes`, per `docs/rfcs/2026-09-05-evals-multi-axis-judging.md`) is the
already-existing seam for exactly this: a new value, e.g. `"cache_correctness"`, tags the verdict
without widening any enum.

### 6. Minimum-viable output: `evals cache-correctness-report`, mirroring `cost-report`

A new command, structurally identical to `cost_report_cmd` (`evals/evals/cli.py:1600-1672`):
lists/reads/decodes the same way, filters to `event.cache_layer == "L3" and
event.l3_sample_response_content != ""` instead of `cost-report`'s `agent_run_id` filter, then
calls `judge_cache_correctness()` (single `--llm-judge` or panel `--llm-judge-panel`, reusing the
identical two flags `run`/`rollout` already expose at `cli.py:1083-1099`) once per matched row.
Aggregates pass/fail counts into a wrong-answer rate reported as a Wilson interval via the
existing `evals.stats.wilson_interval(successes, total)` (`evals/evals/stats.py:26-45`) — never a
bare percentage, matching both `PRD.md:40`'s own explicit "the two numbers must be reported
together" framing and `PRD.md:42`'s sibling "never a bare percentage" requirement, and mirroring
every other rate this codebase already reports this way (`report --fail-under`,
`trend alert`'s pooled-CI addition). Each verdict is persisted as a real `Score`
(`scorer_type="llm_judge"` or `"llm_judge_panel"`, `rubric_axis="cache_correctness"`), reusing
`results_store.py`'s existing append-only JSONL mechanism — no new persistence format.

Optional `--record-trend PATH`, mirroring `report`/`audit-corpus`'s own existing flag
(`cli.py:2166-2167, 2543-2544`), would append a `TrendSnapshot` for a new series — requiring two
small, additive widenings of currently-closed `Literal` types in `evals/evals/models.py`:
`TrendSeriesName` (`models.py:352-358`, e.g. adding `"l3_cache_wrong_answer_rate"`) and
`TrendSnapshot.source_command` (`models.py:430`, e.g. adding `"cache-correctness-report"`). Doing
this buys `evals trend show`/`evals trend alert --threshold
l3_cache_wrong_answer_rate:above:VALUE` for free, reusing `trend_alert.py`'s already-real
static-threshold-plus-pooled-Wilson-CI alerting (`DECISIONS.md`'s `[2026-09-12]` Round 6 Phase 3
entry) rather than inventing a second alerting mechanism — no new metric pipeline, no new gateway
OTel instrument, since this statistic is computed hours-to-days later, offline, in a different
process and language than the one that could ever emit a live OTel counter for it.

### End-to-end shape

```
[gateway: real L3 hit] --(cacheInfo.Layer=="L3")--> [CorrectnessSampling gate: sampled?]
        │ no                                                    │ yes
        ▼                                                       ▼
cache_layer="L3" only                          cache_layer="L3" + l3_sample_request_content
(existing behavior,                            + l3_sample_response_content, all on the same
 unchanged)                                     GatewayDecisionEvent
                                                        │
                                          [Vector: always-keep, per updated exclude rule]
                                                        │
                                          [S3/GCS: gatewayevents_v1, unchanged layout]
                                                        │
                              [evals cache-correctness-report --source ... --llm-judge(-panel)]
                                                        │
                              [judge_cache_correctness(): new rubric, existing panel/reduce machinery]
                                                        │
                        [Score(scorer_type=llm_judge[_panel], rubric_axis="cache_correctness")]
                                                        │
                          [Wilson-bounded wrong-answer rate, optionally --record-trend]
```

## Drawbacks

- **A new, real privacy/exposure surface once turned on**, even though disabled by default. The
  entire point of the two new content fields is to carry prompt/completion-shaped text further
  than any existing field does — an operator who sets `SampleRate` too aggressively low (a high
  effective sampling fraction) turns what's designed as a small forensic sample into something
  closer to routine content logging, which is exactly the default posture
  `docs/operations/TELEMETRY.md`'s Privacy & Redaction section exists to prevent. This RFC
  mitigates but does not eliminate that risk — the mitigation is "disabled by default plus a
  narrow gate," not a technical guarantee against operator misconfiguration.
- **Two independent, stacked sampling decisions compound non-obviously.** The gateway-side
  `CorrectnessSampling.SampleRate` and the Vector shipper's existing `OUTCOME_OK` sample rate are
  now two separately-tuned knobs whose *effective combined* judged-sample fraction is their
  product, not either number alone — this RFC's own "always-keep" fix for content-bearing rows
  (Section 3) removes the shipper's rate from that product for THIS specific row type, but an
  operator reasoning about overall S3/GCS egress volume from L3 traffic still has to reason about
  both gates existing, not one.
- **A second judge rubric to maintain in parallel.** `_JUDGE_PROMPT_TEMPLATE`'s own bias
  mitigations (CoT-forcing, quote-grounding, and — per `docs/rfcs/2026-09-09-evals-judge-
  debiasing-position-swap.md` — position-swap debiasing) have each been added incrementally over
  time. A sibling prompt template for cache-correctness does not automatically inherit any future
  improvement to the existing one; each would need its own deliberate backport, a real, ongoing
  double-maintenance cost this RFC's "new sibling function" choice accepts explicitly (see
  Alternatives Considered for why widening `judge()` itself was still rejected despite this cost).
- **This is a measurement, not a mitigation, and is inherently lagging.** Because judging happens
  offline, sampled, hours-to-days after the fact, a real wrong-answer episode can exist and
  continue for a period before any scheduled judging pass would ever surface it in a report. This
  RFC does not reduce actual wrong-answer risk — that remains the entity/freshness hard-gate's job,
  entirely unaffected by this design — it only makes the *rate* visible after the fact.
- **Two closed `Literal` types widened.** `TrendSeriesName` and `TrendSnapshot.source_command`
  each gain one new value — small, additive, non-breaking (mirrors this same `Literal`'s own
  history of past additions, e.g. `llm_judge_panel` joining `ScorerType` in
  `docs/rfcs/2026-09-07-evals-judge-panel-interface.md`), but a real, permanent increase in the
  surface `evals.models` has to keep internally consistent going forward.

## Alternatives Considered

- **Do nothing — leave the metric permanently `not_yet`, write no RFC at all, wait indefinitely.**
  This is arguably the literal reading of Round 7 Phase 9's "none should be written speculatively"
  language. Rejected for the same reason `docs/rfcs/2026-09-11-gateway-mcp-outbound-credential-
  design.md` rejected its own identically-shaped "wait until implementation actually starts"
  alternative: the finding is fresh, de-risking the "how" now is cheap, and doing so removes the
  temptation for a future implementer under real pressure to reach for the nearest-at-hand
  shortcut (e.g., silently reusing the existing reference-guided rubric, which this RFC's own
  Detailed Design shows would be semantically wrong).
- **Judge every cache-hit layer (L1/L2/L3), not just L3.** Rejected: L1/L2 hits are the product of
  a deterministic equality/normalization check — a wrong answer from either is a bug in that match
  logic, testable directly and deterministically, not a statistical sampling-and-judging problem.
  This also falls outside `PRD.md:40`'s own literal scope ("from semantic hits"), and outside this
  RFC's own scoping discipline of building only what the cited finding actually asked for.
- **Compute the correctness signal inside the gateway itself — a live LLM-judge call on every real
  L3 hit, gating serving on the verdict.** Rejected on two independent grounds: (1) it would be a
  second, LLM-based hard-gate layered on top of the existing deterministic entity/freshness gate,
  adding real per-request latency and — since judging requires a real provider call — real cost to
  every cache hit, defeating a fraction of the very cost savings caching exists to produce; (2) it
  would require Cache to call a provider directly, or depend on `internal/adapter`, both explicitly
  forbidden by `AGENTS.md`'s Boundaries section ("Cache is provider-agnostic by design"). Measuring,
  not gating, is what `PRD.md:40` and this RFC both scope to.
- **Reuse the existing reference-guided `judge()`/`build_judge_prompt()` unchanged, passing the new
  request in as `reference`.** Rejected as semantically wrong — see Detailed Design's own "A new
  judge rubric" section for why a question is not a reference answer, and why this would silently
  misuse the one existing rubric for a judgment it was never designed to make.
- **Fold the correctness verdict into the existing `kelvran.cache.l3.gate_outcome` OTel counter as a
  fourth outcome value, instead of a separate `evals`-side pipeline.** Rejected: that counter is a
  live, per-request, gateway-process-emitted metric. A judge verdict is produced hours-to-days
  later, offline, in a different process and a different language (`evals`, Python), by a
  mechanism (an LLM provider SDK call) the gateway process has zero access to. Any attempt to
  populate that counter with a real verdict would collapse into the already-rejected "compute
  inside the gateway" alternative above.
- **Carry the new opt-in content in a separate side-channel message/stream, rather than as
  additive fields directly on `GatewayDecisionEvent`.** A real option — considered and left open
  rather than decided here (see Unresolved Questions), since it trades off keeping every other
  `GatewayDecisionEvent` consumer (`cost-report`, `ingest`, `mapping.py`) free of ever seeing a
  content-bearing field shape against reusing the one already-shipped ingestion pipeline as-is
  with zero new transport.

## Unresolved Questions

- Whether `judge_cache_correctness()` should live as a genuinely new, sibling function in
  `llm_judge.py` (this RFC's recommended shape) or as a parameterization of `judge()` itself (e.g.
  an optional pre-built-prompt override that bypasses `build_judge_prompt` when given) — sketched
  here in the sibling-function shape for the reasons given in Detailed Design/Drawbacks, but not
  committed to a final signature, mirroring how `docs/rfcs/2026-09-11-gateway-mcp-outbound-
  credential-design.md` left its own `OutboundCredential` shape as a sketch, not an interface.
- The real `SampleRate` value for the new gateway-side gate. No real L3-hit production volume
  exists yet to size this against — this RFC does not guess a number, matching `docs/rfcs/2026-09-
  11-gateway-redis-backed-cache-design.md`'s own "decided at implementation time, not this design
  pass" precedent for an analogous not-yet-measurable parameter. A future implementer would likely
  start from the Vector shipper's own existing `rate: 10` default as a reasonable anchor, but that
  is a guess, not evidence.
- Whether the two new opt-in content fields belong directly on `GatewayDecisionEvent` (this RFC's
  proposal, chosen for zero new transport) or in a separate, narrower side-channel — see
  Alternatives Considered. Left open.
- Whether a confirmed "wrong answer" verdict on a sampled L3 hit should trigger anything automated
  (a targeted cache-entry invalidation, an operator alert beyond `trend alert`'s own static
  threshold) is explicitly out of scope here. This RFC produces a measurement, never an action,
  mirroring `audit_corpus.py`/`corpus_staleness.py`'s own report-only, human-review precedent for a
  comparably uncertain, false-positive-prone signal.
- Whether a confirmed-wrong L3 hit should ever feed back into the regression corpus (via `evals
  promote`, closing the loop into `evals/ARCHITECTURE.md`'s own documented "GAP convention" for
  tracking a known, currently-unfixed defect) is a plausible, distinct follow-on this RFC does not
  decide.
- Whether the eventual real trigger (Status section) should be phrased as a bare volume threshold
  (e.g., "N real L3 hits per day") or should also require a live multi-tenant/multi-deployment
  traffic mix (so a sample isn't dominated by one workload's own quirks) is not decided here —
  left for whoever scopes the follow-up implementation RFC once the trigger is closer to firing.

## Verification

None — this RFC is design-only, per its own Status line. No code changes accompany it. A future
implementation pass would need, at minimum: the additive `api/gatewayevents/v1/gatewayevents.proto`
fields (15-17) and regenerated bindings on both sides (confirmed via `buf breaking`, matching the
two prior additive-field RFCs' own verification shape); the new `CacheL3CorrectnessSamplingConfig`
and its gateway-side sampling/capture logic plus tests; the updated Vector-shipper `exclude` rule;
the new `llm_judge.py` rubric, prompt template, and function, plus tests mirroring
`llm_judge_test.py`'s existing scripted-`call_model` pattern; the new `evals cache-correctness-
report` CLI command plus CLI-integration tests mirroring `cost_report_cmd`'s own; and its own RFC
status update from "Design-only" to "Accepted, implemented" — none of which exist today.
