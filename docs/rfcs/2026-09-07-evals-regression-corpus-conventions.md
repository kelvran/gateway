# RFC: Regression-tier eval corpus conventions — taxonomy, provenance, review gate, staleness

## Status

Accepted, 2026-09-07.

## Context

Four parallel `/deep-research` passes (`docs/upgrade-research/evals-regression-dataset-{methodology,sizing,bootstrapping,taxonomy-maintenance}-2026-09-07.md`) found that Kelvran's regression-tier machinery is real and sophisticated — `evals promote --tier {regression,drift_sample}`, `evals report --tier`/`--category-fail-under`/flaky-exclusion, Wilson-lower-bound gating — but the corpus itself is still a 4-5-case smoke-test fixture, and no convention exists yet for what to tag a real case with, who reviews it before promotion, or how to correct a case whose expected answer has gone stale. This RFC is process/convention only, not code — `EvalCase.tags: list[str]` and `evals promote` already support everything below with zero schema changes.

### What already exists (verified directly, not assumed)

- `EvalCase.tags: list[str] = Field(default_factory=list)` (`evals/evals/models.py`) — a plain, open-ended string bag, already used for the `category:safety` convention `--category-fail-under` reads (per `docs/rfcs/2026-09-07-evals-cigate-refinements.md`).
- `evals promote --tier {regression,drift_sample}` (`docs/rfcs/2026-09-05-evals-golden-regression-promotion.md`) already creates a **new** `EvalCase` (new `id`/`revision`) from a `Run`+`Score`+source case — it never mutates or re-revises the source. `--tier golden` is deliberately excluded from that command's `click.Choice` entirely — golden cases must be hand-authored/reviewed, never auto-promoted.
- No code change is needed for anything in this RFC — every convention below is a tagging/process discipline layered on the existing mechanism.

## Design

### 1. Starter `category:` taxonomy

Following the methodology research's Finding 1 (real taxonomies cross multiple orthogonal axes, not one flat list) and the taxonomy research's Finding 1/2 (HELM/DeepEval both mix dimensions; Promptfoo's `metadata`/`filterMetadata` is the closest real analog to a flexible tag bag): adopt these `category:` values, one tag per case, matching the existing `category:safety` naming convention exactly:

- `category:routing` — deployment selection, fallback-chain correctness, rate-limit enforcement
- `category:cache` — L1/L2/L3 hit/miss correctness, tenant isolation, fingerprinting
- `category:cost` — Decimal cost-accounting correctness, budget-threshold behavior
- `category:guardrail` — PII/secrets/prompt-injection detection accuracy
- `category:streaming` — SSE/streaming correctness
- `category:judge` — judge-scoring accuracy itself (a meta-eval category, scoring the evaluator, not the gateway)

This list is additive — a new category is just a new tag value, no schema change, no registry to update elsewhere.

### 2. `provenance:` tag axis, orthogonal to `category:`

A second tag on every case, from day one, even though the first real batch will be almost entirely one value:

- `provenance:dogfood` — mined from this project's own development history (bug-to-eval conversions)
- `provenance:hand-written` — authored directly, not derived from an existing bug or run
- `provenance:synthetic` — LLM-generated input, human-reviewed before promotion

Tagging provenance now (per the methodology research's Finding 1) means these buckets can be scored and gated separately later, once real production traffic exists and an `evals ingest`-sourced `provenance:production` bucket becomes real — without retroactively tagging every earlier case.

### 3. Human-review checkpoint before `evals promote --tier regression`

**Decision:** a person confirms a candidate `Run`+`Score` is genuinely correct-enough before running `evals promote`, for every promotion — no automatic promotion path. This mirrors the strongest precedent the methodology/taxonomy research found (LangChain's evaluator-flags-candidate → human-annotation-queue → SME-decides workflow, the closest real analog found anywhere to a "should this become a regression case" decision), and mirrors how `golden`-tier cases are already, by design, excluded from auto-promotion in this same command.

**Alternative considered and rejected:** trust `evals promote`'s existing requirement (a real failing `Score` when `--scores` is given, per the original promotion RFC) as sufficient gating on its own. Rejected because a failing `Score` proves the *original* run's outcome was captured faithfully — it says nothing about whether that outcome is the *right* one to lock in as a permanent regression bar. The methodology research's Finding 2 confirms this is a real distinction other platforms draw (DeepEval's `expected_output` is optional and explicitly human-approved when present, not auto-derived from a run).

### 4. Stale-case correction: `deprecated` + `supersedes:<old-case-id>` tags

**The problem:** `evals promote` is append-only by design — it never mutates a source case. If a case's expected answer becomes wrong because Kelvran's own behavior legitimately changed (not a real regression), there is no existing mechanism to correct it. The taxonomy research (Finding 8) confirms **no real platform documents this exact scenario** for an append-only, never-edit-in-place system — Braintrust's updates are merge-upserts against a stable ID (the opposite pattern); LangChain's golden-dataset workflow is silent on correcting an existing stale entry.

**Decision (original design for a genuine gap, not borrowed practice — stated as such):** tag the stale case `deprecated` (never delete or edit it — preserving history, matching the append-only mechanic's own spirit) and promote a new corrected case via the normal `evals promote` path, adding a `supersedes:<old-case-id>` tag on the new case. `report_cmd`'s existing gate logic needs zero changes: a `deprecated`-tagged case should simply never be included in a fresh `--suite` file going forward (an authoring-time convention — drop `deprecated` cases when building/editing the suite file used for `evals run`/`evals rollout`, not a new runtime filter).

### 5. Review cadence: reactive triggers, not a calendar

Per the taxonomy research's Finding 4 (a specific tiered monthly/quarterly/annual cadence claim was explicitly refuted as documented practice; only reactive triggers survived verification): re-review affected regression cases whenever a scorer/prompt/model/routing change ships, a real bug is found, or CI gate behavior looks surprising. No fixed calendar review is adopted.

## Alternatives considered and rejected (summary)

- **A new `provenance` field on `EvalCase`/`Score`** instead of a tag — rejected for the same reason `category:` isn't a dedicated field: `tags` is already the open-ended, filterable mechanism `--category-fail-under` reads, and a second dedicated field would duplicate that mechanism for no benefit.
- **Automated category-imbalance rebalancing** — rejected for this pass; the taxonomy research found only qualitative "stratify + oversample informative strata + reweight" guidance with no verified numeric target anywhere. Track category distribution via `report_cmd`'s existing per-category output; rebalance by judgment, not by an automated mechanism this research found no real precedent for.

## Verification

Doc-only RFC — no tests. Verified against the currently-shipped `EvalCase`/`Score` schema (`evals/evals/models.py`) and `evals promote`'s real behavior (`evals/evals/cli.py`) directly, not assumed. Confirms no contradiction with `docs/rfcs/2026-09-05-evals-golden-regression-promotion.md`'s existing promotion mechanics or `docs/rfcs/2026-09-07-evals-cigate-refinements.md`'s existing gate logic.
