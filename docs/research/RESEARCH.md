# Research

A running, checkbox-driven list of open questions that need investigating before they can become a `docs/rfcs/` proposal or a `DECISIONS.md`/ADR entry. **This file never holds findings themselves** — a deep, point-in-time research report belongs in the parent workspace's `Not-Humans-World/ai-infra-research/` (that's where the research behind Kelvran's own architecture, naming, and documentation set already lives), or becomes the motivating context inside an RFC once it's ready to propose something concrete. This file is the funnel, not the archive.

## How to Use This File

- **Add** a question here the moment it's identified as blocking a future decision, even if nobody's investigating it yet.
- **Promote** a question once it has enough of an answer to act on — link the RFC or ADR it became, then move it to "Resolved" below.
- **Strike through**, don't delete, a question that turned out not to matter — leave a one-line note why.

## Open Questions

- [ ] What's the exact freshness/risk model for gating semantic-cache (L3) hits, replacing a bare similarity threshold? (`PRD.md`'s "Open Questions Carried Forward", `DESIGN.md`'s "Open Design Questions" both flag this as an RFC candidate once L3 work starts — not designed yet.)
- [ ] Independent-refutation vs. pairwise-comparison judge panels for the Evals skeptic-panel upgrade (v2)? Research leans toward independent refutation per prior work, but the panel-size/quorum rule isn't settled.
- [ ] Exact virtual-key/budget hierarchical-scope data model (org → team → user → key → session) — sketched at a high level in `ARCHITECTURE.md`, not finalized; likely settled during Gateway's own Phase 1 build rather than needing a standalone RFC.
- [ ] Contract-testing approach for the `api/` boundary: is `buf breaking` + a golden-fixture round-trip test sufficient long-term, or does a full Pact Broker-style consumer-driven contract test become worth the setup cost once there are more than two consumers of the contract?
- [ ] Chaos-engineering tooling: Toxiproxy is the near-term choice (see `docs/testing/TESTING.md`); revisit Chaos Mesh only if/when the deployment target is actually Kubernetes in production.

## Resolved

*(None yet — nothing has graduated to an RFC or ADR since this file was created.)*
