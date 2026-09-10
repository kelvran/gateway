# Evals Drift Monitoring — Deep Research, Round 4 (2026-09-11)

Scope: `evals/`'s production drift-detection and monitoring lens specifically — not the
broader evals-harness roadmap already covered in `evals-next-upgrade-round3-2026-09-11.md`.
Grounded against `evals/ARCHITECTURE.md`, `docs/upgrade-research/evals-trace-transport-
2026-09-07.md`, and `evals-next-upgrade-round3-2026-09-11.md`'s Finding 3/4 (the drift-
auto-flag section), plus direct reads of `evals/evals/auto_flag.py`,
`evals/evals/ingestion/mapping.py`, `evals/evals/models.py` (`TrendSnapshot`/
`TrendSeriesName`), and `api/gatewayevents/v1/gatewayevents.proto` to confirm the real,
shipped state. Externally verified against 2026-current drift-detection literature (DDM/
ADWIN/Page-Hinkley concept-drift detectors, PSI) and production-observability vendor docs
(Datadog, Sentry, New Relic, Galileo), each claim adversarially 3-vote-verified before
inclusion here. Five research questions were posed; RQ3 (push-based alerting) returned no
surviving verified external evidence this round and is answered from Kelvran's own
internal architecture facts alone, not new research.

## Context (Kelvran's own real, current state, re-confirmed this pass)

`evals flag-candidates` (`evals/evals/auto_flag.py`) is v1-scope, outcome-based rules only
— one `FlagRule` (`gateway-failure-outcome`) matching `EvalCase.task_spec["outcome"]`
against four enum names (`OUTCOME_UPSTREAM_ERROR`, `OUTCOME_RATE_LIMITED`,
`OUTCOME_GUARDRAIL_BLOCKED`, `OUTCOME_DEPLOYMENT_CAPACITY`), deliberately reading
`task_spec["outcome"]` rather than `Run.status` (which collapses all nine non-OK
`GatewayDecisionEvent.Outcome` values into one `"error"` string). The module's own
docstring names its v1 limitation explicitly: `occurred_at`, `rate_limit_fail_open`,
`fallback_happened`, `fallback_from_deployment`, and `budget_spent_usd` are decoded during
ingestion but discarded before persistence, so no rule can use timing, fallback, or spend
signals yet — this is the already-identified, explicitly-deferred follow-up this round
takes as given, not something to re-litigate. Independently confirmed against
`api/gatewayevents/v1/gatewayevents.proto` directly: there is no latency field and no
per-request-cost field anywhere on the wire at all — `budget_spent_usd` is cumulative
spend *before* this request, not this request's own cost — so no `mapping.py` change,
however thorough, can ever produce a latency- or per-request-cost-based rule; that would
require a new proto field and the `buf breaking` procedure, a cross-language contract
change out of scope here. `evals/evals/models.py`'s `TrendSnapshot`/`TrendSeriesName`
confirms four persisted trend series (`judge_accuracy_kappa`, `quote_grounding_rate`,
`audit_corpus_defect_rate`, `cost_usd`), each with a `recorded_at: datetime` — the time
the CLI invocation ran and appended the snapshot, not a per-case event timestamp; this is
a materially different clock than the per-case `occurred_at` still being discarded, a
distinction that matters for RQ2 below.

## Executive Summary

The single highest-value, best-evidenced move this round is Finding 2: DDM (Drift
Detection Method) is a genuinely low-cost, purely outcome-driven sequential drift test —
running mean/std with 2σ/3σ thresholds and a 30-instance minimum — that needs nothing
beyond a timestamped binary-outcome series, making it buildable the moment `occurred_at`
is persisted (the same already-deferred follow-up named in the prior round), with no new
dependency and no proto change. Root-cause correlation (RQ4, Finding 1) has real
industry precedent, but the mature, automated version (Datadog's tag-driven Faulty
Deployment Detection) depends on a persisted deployment/config-version identifier Kelvran
does not have on the wire — a genuine "needs more infrastructure" gap — while a
timestamp-adjacency correlation against Kelvran's own git history (the New Relic/Sentry-
style "visual overlay, not causal inference" approach the industry itself actually ships)
is buildable the moment `occurred_at` lands, with no proto change at all. The next rule
dimension question (RQ1, Finding 3) landed on weak evidence: only a single vendor blog
survived verification naming fallback-strategy monitoring as a reliability-monitoring
practice, and the stronger, more concrete postmortem-style evidence for why fallback/retry
signals catch what outcome-only rules miss was explicitly checked and refuted — so
fallback is the only *available* next dimension (latency is proto-blocked outright), not
a strongly evidenced highest-value one. Push-based alerting (RQ3, Finding 4) has no
surviving external evidence either way this round; the verdict below rests entirely on
Kelvran's own documented state (a human-run CLI with no scheduled invocation yet), not new
research.

## Findings

### Finding 1 — Root-cause correlation has real industry precedent, but the automated version needs a persisted change-identifier Kelvran's wire format doesn't have; the timestamp-adjacency version the industry actually ships is buildable now, once `occurred_at` lands (confidence: high) — **not yet: version-tag auto-correlation (needs a proto change); build now (once `occurred_at` persists): timestamp-adjacency correlation against git history**

Four sources converge on what "root-cause correlation" actually looks like in mature 2026
production tooling, and none of them do automated causal inference. New Relic's Change
Tracking explicitly targets the exact question Tombstone's "What Changed?" answers —
"What changed before this outage?", "Did our deployment cause this regression?" — via "a
timeline of all modifications leading up to incidents." But its own mechanism, confirmed
directly from the same docs and cross-checked against three years of New Relic's own
release notes (Jan 2023 through Mar 2026, with no upgrade in that window), is a **visual/
statistical overlay**: interactive change markers laid on performance charts plus
before/after comparative metrics and anomaly detection — correlation by timestamp
adjacency an operator eyeballs, not an automated causal-inference engine. (New Relic does
have a separate AI root-cause tool, Autopilot, but it is presented as a distinct product,
not folded into Change Tracking's correlation mechanism.) Sentry's suspect-commit feature
is the same shape from the code side: it attributes an issue to the most recent commit
touching the failing line, but only within a deliberately bounded ≤1-year recency window —
time-limited attribution, not open-ended causal search. Datadog's Automatic Faulty
Deployment Detection is the one genuinely more-automated example found: it is Watchdog-
powered and requires no separately built correlation pipeline, but it is enabled *only* by
tagging services with a reserved `version` tag — the mechanism is entirely dependent on a
persisted, per-request version/deployment identifier existing on the telemetry in the
first place.

That dependency is exactly where Kelvran's own wire format falls short today. Confirmed
directly against `api/gatewayevents/v1/gatewayevents.proto`: there is no deployment-
config-version or router-config-version field anywhere on `GatewayDecisionEvent`, and a
grep of `gateway/` found no `config_version`/`deployment_version` concept tracked anywhere
in the Go router at all — deployment config changes happen via YAML edits with no
persisted version identifier attached to the events those configs later produce. A
Datadog-style automated version-to-regression correlation is therefore genuinely blocked
on new infrastructure: either a new persisted config/deployment-version field on
`GatewayDecisionEvent` (a cross-language contract change requiring the `buf breaking`
procedure per `AGENTS.md`) or an equivalent versioning concept added to the Go router
first. **What is buildable now, without any proto change**, is the lighter-weight pattern
the *rest* of the survey actually confirmed as the real 2026 norm — New Relic/Sentry-style
timestamp-adjacency correlation, not causal inference: once `occurred_at` is persisted
per `EvalCase` (the already-named deferred follow-up), a drift signal's own timestamp
(e.g., when DDM's warning/drift threshold fires, per Finding 2) can be reported alongside
Kelvran's own git commit history for `gateway/` and its config files in the same window —
an operator-facing "here's what changed near this time" report, exactly the visual/
temporal-adjacency shape New Relic and Sentry both actually ship, not a fabricated claim
of automated attribution.

### Finding 2 — DDM is a real, low-cost, purely-outcome-driven sequential drift test that needs nothing beyond a timestamped binary-outcome series — distinct from, and cheaper than, PSI's batch-only reference-window approach (confidence: high) — **build now, once `occurred_at` persists: DDM over the ingested outcome stream; not PSI (batch-only, heavier-weight, not a sequential test)**

DDM (Drift Detection Method) is confirmed, from the primary source (the CRAN `datadriftR`
package documentation, which itself cites Gama et al.'s original 2004 paper and matches
the long-established scikit-multiflow/MOA reference implementations), to detect concept
drift purely from a live stream of binary outcome/error values using only a running mean
and standard deviation of the error rate — no latency, cost, or other field needed
anywhere in its public interface. Its defaults are exactly the shape the research question
asked for: `warning_level`/`out_control_level` are literal multipliers on the running
standard deviation (2σ for warning, 3σ for drift), and `min_num_instances` defaults to 30
— i.e., detection can start meaningfully with as few as 30 ingested cases, not thousands.
This is a direct, uncomplicated fit for Kelvran's actual data shape once the already-
identified deferred follow-up (persisting `occurred_at`) lands: feed each ingested
`drift_sample` case's binary outcome (e.g. `gateway-failure-outcome`-matched = 1, else =
0) through DDM's running-mean/std update in `occurred_at` order, and DDM's own 2σ/3σ
thresholds do the sequential comparison the research question asked about — a "recent
window vs. baseline" test implemented as a running statistic, not two frozen snapshots.

This is meaningfully cheaper and more directly applicable than PSI, which this round also
confirmed but in a way that argues against using it for this specific purpose. Arize's own
published PSI methodology (`(Actual% − Expected%) × ln(Actual% / Expected%)`, summed
across bins, reference-window vs. current-window) is, as documented on that same page,
**exclusively a batch-comparison technique** — freeze a reference window, then re-histogram
on a schedule tick against the same bin edges; nothing on that page describes a streaming,
sequential, or incremental/online computation method. Using PSI for Kelvran's outcome
stream would mean choosing and freezing an arbitrary reference window and re-running it as
a scheduled batch job — real work with a design decision (how big a reference window, how
often to re-run) that DDM's running-statistic approach doesn't require at all. Two
candidate streaming detectors that would have been even closer analogs — ADWIN and the
Page-Hinkley test — were checked this round and explicitly **failed verification** (1-2 and
0-3 votes respectively): the specific claims made about their fit (a single confidence-
delta parameter for ADWIN; "recommended for residuals/error rates specifically" for
Page-Hinkley) did not hold up to scrutiny, so DDM is the one sequential detector with
verified, cited support for this exact use case, not the only one considered.

**Build now, once `occurred_at` persists**: a small, pure-Python DDM implementation (no
numpy/scipy needed — it is running sums of a 0/1 stream, unlike the kappa power-calculator
gap already on record in this round's judge-calibration research, which genuinely needs
`kappaSize`-equivalent statistics) run over `drift_sample` cases ordered by `occurred_at`,
flagging a warning/drift state the same way `evals flag-candidates` already prints a
"run this promote command next" recommendation. **Not yet**: PSI or any other batch-
reference-window technique for this specific outcome-rate-over-time question — it is a
real, well-documented technique, just not the low-cost sequential one this question asked
for.

### Finding 3 — Fallback-based monitoring is the only next rule dimension with any surviving vendor evidence, but the evidence is thin (a single vendor blog) and the stronger postmortem-level case for it was explicitly refuted; latency is blocked outright regardless of `mapping.py` (confidence: low-medium) — **not yet: fallback-based rule; trigger = the already-named `mapping.py` extension; latency-based rule is never buildable against this proto, full stop**

Only one source survived verification on ranking rule dimensions: Galileo's own published
guidance names fallback strategies (routing to simpler models, cached responses, or
alternative processing) and a 4-way error taxonomy (infrastructure/system/model/
integration) as parts of production LLM reliability monitoring, with latency tracked as a
separate, earlier metric in the same piece. This is real — the fallback-strategy language
and the taxonomy both check out verbatim against the source — but it is Galileo's own
blog post describing Galileo's own recommended practice, not an independently-measured
ranking of which signal catches the most real incidents, and the verification pass flagged
that the taxonomy/fallback material and the latency material sit in separate, sequential
sections of the same article rather than one unified bundle, so treating this as "Galileo
ranks fallback above/below latency" would overreach the source.

Critically, a stronger and more concrete piece of evidence that would have supported
fallback/retry-based monitoring far more forcefully — an Arize case study quantifying a
specific hidden retry loop (a root span reporting "OK" status while masking 43 repeated
tool calls and 50 orchestrator iterations) as proof that outcome/status-based monitoring
alone misses real failures — was checked this round and explicitly **refuted** (0-3 and
1-2 votes on its two component claims). That is exactly the kind of postmortem-level
evidence this research question asked for, and it did not survive scrutiny, so it cannot
be used to argue fallback-based rules are more valuable than an outcome-only rule set.
What remains true independent of that refuted claim, from Kelvran's own already-confirmed
architecture facts (not new external research): latency-based rules are not merely
lower-priority than fallback-based ones — they are **categorically impossible** against
`api/gatewayevents/v1/gatewayevents.proto` as it exists today, since no latency field
exists on the wire at all (confirmed directly, see Context above), whereas
`fallback_happened`/`fallback_from_deployment` already exist on the proto and are simply
discarded before persistence — a `mapping.py` change, not a proto change, closes that
specific gap. **Verdict**: fallback-based rules are the correct next dimension to build
*because they are the only one available*, not because the evidence proves they catch
more real incidents than outcome-only rules — a materially weaker claim than the research
question hoped to find support for. **Not yet, named trigger**: the already-identified
`mapping.py` extension to persist `fallback_happened`/`fallback_from_deployment` (and,
while there, `rate_limit_fail_open`/`budget_spent_usd`) — once that lands, a second
`FlagRule` keyed on `fallback_happened=true` (optionally combined with the outcome
already being non-OK) is additive to `evals/evals/auto_flag.py` with no schema change to
`EvalCase`/`Run` beyond what the deferred follow-up already proposes.

### Finding 4 — Push-based alerting has no surviving external evidence this round in either direction; the "not yet" verdict rests entirely on Kelvran's own documented state, not new research (confidence: n/a — internal-facts-only) — **not yet: push-based alert channel; named trigger: `evals flag-candidates` running on any recurring schedule, not just human-invoked ad hoc**

No claim addressing push-based alerting, paging discipline, or when a rule-based signal
should escalate to a notification channel survived this round's 3-vote verification — four
distinct Google SRE-book claims on this exact topic were checked and every one was
refuted (0-3 across all four), so this question cannot be answered from newly-confirmed
external sources this pass. The verdict below is therefore drawn only from Kelvran's own
already-documented, non-externally-sourced facts: `evals flag-candidates` is, per its own
docstring and CLI design (confirmed directly from `evals/evals/auto_flag.py` and
`evals/evals/cli.py`), a human-run, one-shot command that prints a recommended `evals
promote` invocation for a person to act on — it has no scheduled/cron invocation anywhere
in this repo today, and (per the research target's own framing) zero real production
traffic feeds it yet. A push notification needs a recurring producer to be worth building
around; a command a human runs when they think to run it has no cadence for a push channel
to attach to. **Not yet, named trigger**: the point at which `evals flag-candidates` (or
Finding 2's DDM check) is wired into any recurring scheduled job — a cron, a CI nightly
step, or equivalent — is the concrete moment a push channel becomes worth building,
because only then does a "this fired since the last time a human looked" gap exist for a
notification to close. Building push alerting before that scheduling exists would be
notifying about a signal nothing yet produces on a cadence, which is premature regardless
of what the SRE literature says about paging discipline in general — this verdict does not
depend on those refuted claims either way.

## Caveats

- RQ3 (push-based alerting) has zero surviving externally-sourced evidence this round —
  all four candidate SRE-book claims were refuted 0-3. Finding 4's verdict is real and
  defensible from Kelvran's own documented facts, but readers should not mistake it for an
  externally-validated research conclusion the way Findings 1-3 are.
- Finding 3's ranking is explicitly weaker than the research question asked for: it
  reduces to "fallback is the only *available* next dimension" (a fact about what the
  proto permits) rather than "fallback is the *most valuable* next dimension" (a claim
  about incident-catching power), because the one piece of evidence that would have
  supported the stronger claim was checked and refuted this round.
- Finding 1's "buildable now" half (timestamp-adjacency correlation against git history)
  is this report's own synthesis of confirmed source material (New Relic's/Sentry's actual
  mechanism) applied to Kelvran's specific repo structure — it is a reasonable, well-
  grounded inference, not itself a claim any of the four cited vendor docs make about
  Kelvran.
- Two vendors named in the original research prompt as comparison points (Arize AI,
  WhyLabs) had claims checked this round; the WhyLabs core-mechanism claim was refuted
  0-3, and Arize's PSI-related claims survived but only for the batch-methodology
  description (Finding 2), not for anything supporting a ranking of rule dimensions
  (Finding 3) or root-cause correlation (Finding 1) — their absence from those findings is
  a real gap in this pass's search coverage, not evidence they lack a relevant published
  position.
- Several claims hit search-tool rate limits (Google 429, DuckDuckGo CAPTCHA, Tavily/Exa
  caps) during verification, consistent with prior rounds' documented experience with the
  same tools — this affected independent third-party corroboration for a few claims, which
  then rested on primary-source-only confirmation (still verified directly against the
  live page, just without a second independent source).

## Recommendation for Kelvran

1. **Build now, once `occurred_at` persists (the already-named deferred follow-up)**: a
   small, pure-Python DDM (Drift Detection Method) pass over `drift_sample` `EvalCase`s in
   `occurred_at` order, using the already-available binary outcome signal, flagging a
   warning (2σ) or drift (3σ) state once at least 30 cases exist (Finding 2). No new
   dependency, no proto change — the single highest-value, best-evidenced item this round.
2. **Build now, once `occurred_at` persists, additive, no proto change**: a lightweight
   "what changed nearby" report correlating a drift signal's timestamp against Kelvran's
   own git commit history for `gateway/` and its configs — the same timestamp-adjacency,
   not-causal-inference shape New Relic/Sentry actually ship (Finding 1's second half).
3. **Not yet, named trigger = the already-named `mapping.py` extension**: a
   `fallback_happened`-keyed `FlagRule`, once `fallback_happened`/`fallback_from_deployment`
   are persisted — the only next rule dimension the proto permits, but not shown this
   round to be the most valuable one (Finding 3).
4. **Not yet, genuinely blocked on new infrastructure**: a Datadog-style automated
   version-tag correlation for root-cause attribution — needs a persisted deployment/
   config-version identifier that does not exist anywhere in Kelvran's Go router or its
   `GatewayDecisionEvent` proto today; trigger is a deliberate decision to add that field
   (a `buf breaking`-gated cross-language contract change), not evaluated in this pass
   (Finding 1's first half).
5. **Not yet, named trigger**: a push-based alert channel — trigger is `evals
   flag-candidates` (or Finding 2's DDM check) running on any recurring schedule rather
   than purely human-invoked; building this before a scheduled producer exists notifies
   about nothing (Finding 4).
6. **Never buildable against the current proto, not merely deprioritized**: a
   latency-based or per-request-cost-based `FlagRule` — no such field exists on
   `GatewayDecisionEvent` at all; this requires a proto change and the `buf breaking`
   procedure before any `mapping.py`/`auto_flag.py` work could even begin (Finding 3).

## Open Questions

- Once `occurred_at` is persisted and DDM (Finding 2) is running, what should the actual
  operator-facing surface be — folded into `evals flag-candidates`'s existing printed
  recommendation, a new `evals drift-check` command, or a fifth `TrendSnapshot` series
  alongside the four `evals/evals/models.py` already tracks? This report does not resolve
  that interface question, only the underlying statistical method.
- Is there a lighter-weight way to get a deployment/config-version identifier onto
  `GatewayDecisionEvent` (Finding 1) than a full new proto field — e.g., could Kelvran
  derive an approximate "config epoch" from existing `occurred_at` timestamps joined
  against git commit timestamps for `gateway/config/`, without a wire-format change at
  all, as a cheaper first cut before considering the proto change?
- This round found no surviving evidence on WhyLabs' or a genuinely independent Arize
  case-study's specific incident-postmortem data for ranking rule dimensions (RQ1) — is
  there a real 2026 postmortem or incident report (distinct from the refuted Arize
  retry-loop blog claim) that would give RQ1 a stronger evidentiary basis than "the only
  available dimension, not the most valuable one"?
- Finding 4's "not yet" on push alerting is entirely internal-facts-based this round —
  should a future pass specifically re-search push-alerting/paging-discipline literature
  with different sources than the Google SRE book (which failed verification four separate
  times this round), or is that itself a signal the underlying claims were simply
  over-specific rather than that better sources exist?
