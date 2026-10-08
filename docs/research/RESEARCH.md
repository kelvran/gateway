# Research

A running, checkbox-driven list of open questions that need investigating before they can become a `docs/rfcs/` proposal or a `DECISIONS.md`/ADR entry. **This file never holds findings themselves** — a deep, point-in-time research report belongs in `docs/upgrade-research/` (dated `<topic>-YYYY-MM-DD.md` reports; `docs/upgrade-research/README.md` explains how a newer report or a dated `DECISIONS.md` entry supersedes an older one), or becomes the motivating context inside an RFC once it's ready to propose something concrete. The pre-repo research behind Kelvran's own architecture, naming, and documentation set lives in the parent workspace's `Not-Humans-World/ai-infra-research/`, outside this repository. This file is the funnel, not the archive.

## How to Use This File

- **Add** a question here the moment it's identified as blocking a future decision, even if nobody's investigating it yet.
- **Promote** a question once it has enough of an answer to act on — link the RFC or ADR it became, then move it to "Resolved" below.
- **Strike through**, don't delete, a question that turned out not to matter — leave a one-line note why.

## Open Questions

- [ ] Exact virtual-key/budget hierarchical-scope data model (org → team → user → key → session) — Gateway's Phase 1 build (`docs/rfcs/2026-09-02-virtual-keys-budgets.md`) shipped flat, key-level scope and explicitly deferred the hierarchy; it remains target-only in `gateway/ARCHITECTURE.md`, with no RFC yet — `DECISIONS.md`'s `[2026-09-07]` entry and `docs/upgrade-research/gateway-hierarchical-budgets-2026-09-07.md` reaffirm the deferral until a real multi-tenant demand signal appears.
- [ ] Contract-testing approach for the `api/` boundary: is `buf breaking` + a golden-fixture round-trip test sufficient long-term, or does a full Pact Broker-style consumer-driven contract test become worth the setup cost once there are more than two consumers of the contract?
- [ ] Chaos-engineering tooling: Toxiproxy is the near-term choice (see `docs/testing/TESTING.md`); revisit Chaos Mesh only if/when the deployment target is actually Kubernetes in production.
- [ ] Cascaded, confidence-gated judge panel (a cheap judge first, escalating to a stronger model only on low confidence) as a third skeptic-panel axis beyond independent refutation — recorded in `PRD.md`'s "Open Questions Carried Forward" (added 2026-09-25) for a future RFC to weigh alongside the settled protocol; not designed.

## Resolved

- [x] Freshness/risk model for gating L3 cache hits — settled for the lexical L3-lite that shipped, by `docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md` (entity/number/date hard-gate plus a freshness/risk model, checked in `checkLexicalCache`/`freshnessRiskModel` in `gateway/internal/gateway/dataplane/dataplane.go` with `Fingerprint` in `gateway/internal/gateway/dataplane/entities.go`; the MinHash/Jaccard primitives are `gateway/internal/cache/lexical.go`). The embedding-based L3 variant has a design-only, deferred RFC that keeps the same hard-gate: `docs/rfcs/2026-09-20-gateway-cache-l3-write-read-split.md`.
- [x] Skeptic-panel protocol — independent refutation, settled by `docs/rfcs/2026-09-07-evals-judge-panel-interface.md`; the quorum rule (strict majority, fail-closed on a tie) by `docs/rfcs/2026-09-08-evals-judge-panel-reducer.md` (`reduce_panel_votes` in `evals/evals/judge/llm_judge.py`). `PRD.md`'s "Open Questions Carried Forward" still records a third axis — a cascaded, confidence-gated panel — for a future RFC to weigh; that is a new question, not this one reopened.
