# 0001 — One monorepo, two deployables

- **Status**: accepted
- **Date**: 2026-09-02
- **Deciders**: project founder

## Context and Problem Statement

Kelvran combines three capabilities (Gateway, Cache, Evals). Should the project ship as one combined deployable, three fully independent projects/repos, or something in between? The answer needs to hold up not just for a solo builder today but through team growth.

## Decision Drivers

- Go and Python cannot share a runtime — a hard technical constraint, not a preference.
- Every "premature split" case study surveyed (Segment's 140+ microservices, InVision, Istio's control plane, Amazon Prime Video's monitoring pipeline) describes a team that split early, paid a real multi-month-to-multi-year cost, and reversed it.
- Every comparable AI-infra product surveyed (LiteLLM, Portkey, TensorZero, Bifrost, Kong AI Gateway, Helicone) keeps its gateway and cache fused in one process, with zero exceptions.
- The one company surveyed that unified gateway+cache+observability+evals+optimization into one team/codebase/business (TensorZero) is the one company in the survey that shut down.
- Conway's Law: service boundaries should mirror team boundaries. At 1-10 people there are no team boundaries to mirror.

## Considered Options

1. One deployable, one repo — technically impossible (Go can't embed Python).
2. Three fully separate projects/repos (Gateway, Cache, Evals each independent).
3. One monorepo, two deployables (`gateway` = Go, containing Gateway+Cache; `evals` = Python).
4. Two repos (one per deployable), no shared monorepo.

## Decision Outcome

**Option 3: one monorepo, two deployables.** The deployable split (`gateway`, `evals`) is forced by the Go/Python runtime boundary — not a choice. The repo count (one) is a separate choice, and monorepo wins because the thing that must be versioned atomically across that language boundary (the OTel/proto contract in `api/`) is exactly the kind of cross-cutting artifact that drifts when split across repos — Portkey's own two-codebases-for-one-gateway split ran for roughly two years before a public "one gateway, one codebase" remerge.

## Consequences

**Positive**: one CI pipeline enforces the shared contract via `buf breaking`; no risk of the two deployables' auth/identity/cost-accounting concepts drifting into incompatible shapes; onboarding a new contributor means cloning one thing.

**Negative**: `evals` sits in the same repo as `gateway` even though it's genuinely idle until `gateway` has real production traffic to sample — accepted as the right tradeoff, since standing up the schema/skeleton early costs little and the alternative (a second repo) risks exactly the drift this decision exists to prevent.

## Revisit Triggers

Split into polyrepo only when a **distinct team** (not a distinct engineer) owns a component end-to-end with independent release cadence and on-call, *and* cross-repo contract tooling (`buf breaking` running across repos, not just within one) is mature enough to hold schema drift to the same standard it holds today in-repo. Not a trigger: team size alone, or "it would be cleaner." See the full scale-based decision table in the parent workspace's `ai-infra-research/decision-single-vs-separate.md` §2.
