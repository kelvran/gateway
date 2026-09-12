# Research: Production-readiness checklist and observability practices for a first pilot customer

**Date:** 2026-09-12
**Scope:** What "ready for a first pilot customer" concretely looks like for Kelvran's gateway — the actual unblocking trigger for most of Kelvran's remaining backlog. Kelvran has real OTel tracing/metrics (`gen_ai.*` spans, `kelvran.cache.*`/`kelvran.llm.spend_usd`/`kelvran.ratelimit.fail_open` counters), a real `THREAT_MODEL.md` and `SECURITY.md`, but has never operated with real production traffic and has no incident-response runbook, on-call practice, or concrete SLO commitments yet — all correctly deferred pending real traffic per `DECISIONS.md`. Already shipped and not re-litigated: the OTel signal set itself (`docs/operations/TELEMETRY.md`), `THREAT_MODEL.md`'s STRIDE analysis, real health-probing/circuit-breaker/fallback-chain resilience mechanisms.

**Note on recovery:** this file was reconstructed directly from the deep-research workflow's full structured JSON result. The subagent explicitly declined to author the report file itself under this session's operating constraints; the underlying research (103 agents, 21 sources, 93 claims extracted, 25 adversarially verified) completed normally.

## Executive Summary

Across the primary sources (Portkey's own status page, Google's SRE book) and practitioner/vendor content (a getdx production-readiness checklist, an independent analysis of LiteLLM's "production" guide, Helicone's own AWS-lockout incident postmortem, and an SRE runbook-writing series), the consistent picture for a first-pilot LLM gateway is that minimal ops maturity is the market norm, not a gap: comparable early-stage gateway products (LiteLLM, Portkey) run with no committed SLO/SLA, no on-call rotation, and no incident-response runbook, substituting a public status page that reports trailing historical uptime plus precise, dated incident entries instead of a forward guarantee. The realistic minimum-viable observability/alerting bar — achievable now with Kelvran's existing OTel signals, no big platform buy needed — is metrics-per-key-operation + dashboards + threshold alerts + structured/queryable logs, plus a strict "every paging alert has a runbook or the alert gets deleted" discipline tracked via a runbook-coverage-% metric. Runbook *content* should stay deliberately provisional pre-incident: real incidents are needed to properly stress-test and correct a runbook in ways scheduled review cannot, and Helicone's own postmortem (a false-positive Bedrock-key flag triggering a 4-day AWS account lock) is a concrete illustration both of the incident shape a first real one can take and of a validated architecture pattern — keep the customer-facing data plane decoupled from control-plane/observability infra. No comparable early-stage product publishes a concrete numeric SLO; the credible way to handle Kelvran's own "no traffic to calibrate against" problem is incident-transparency now (a dated/timed status-page log), deferring any actual SLO number until real pilot traffic exists. Enterprise-scale practices (Google's mandatory load-testing gate, six-dimension PRR, starting readiness review alongside design) are useful directional inspiration for shifting non-functional design left, but are explicitly not the bar a first-pilot-stage startup should replicate wholesale.

**build_now:** metrics/dashboards/alerts/structured logging (largely already in place via Kelvran's OTel signals); a strict alert-to-runbook mapping rule; a runbook-coverage-% tracking habit; verifying data-plane/control-plane decoupling; a public status page (trailing uptime + dated/timed incident log) as the SLA substitute.
**not_yet** (trigger: first pilot customer signs / first real incident occurs): any numeric SLO/SLA commitment; a validated (not just drafted) incident-response runbook for specific failure modes; formal on-call rotation/paging tooling (PagerDuty-class); postmortem process specifics.

## Findings

### Finding 1 — Minimal ops maturity is the market-normal baseline for this product category, not a disclosed deficiency

**Confidence: high.** Sources: [LiteLLM in production](https://perun.au/insights/litellm-production/), [Portkey status page](https://status.portkey.ai/).

Comparable early-stage LLM gateway/proxy vendors (LiteLLM's own production-operations content, Portkey's live status page) currently run with far less operational-maturity infrastructure than an enterprise SRE org would — no SLO/SLA target, no on-call rotation, no paging/alerting runbook, no postmortem process. A consultancy's dedicated "LiteLLM in production" piece names Prometheus metrics and log-grep diagnostics as its only production-diagnostic tooling and contains zero mentions of SLO/SLA, on-call rotation, paging, PagerDuty, incident severity, or postmortems. Portkey's own public status page shows no SLA/SLO language anywhere — only trailing 90-day historical uptime percentages.

### Finding 2 — The real SLA substitute is a status page with incident transparency, not a numeric commitment

**Confidence: high.** Source: [Portkey status page](https://status.portkey.ai/).

Portkey's status page reports only trailing historical uptime plus real, dated, minute-precise incident entries — not a forward-looking numeric commitment. Two named incidents ("JWKS URLs Inaccessible", Aug 13 2026, 40 min; "Control Plane Slowness", Aug 18 2026, 2h10m) are logged with specific dates and minute-level durations rather than vague status text. This directly answers "how do comparable products handle no-traffic-to-calibrate": they don't publish a number, they publish incident transparency instead.

### Finding 3 — The minimum-viable observability bar is four items, framed by one question

**Confidence: medium.** Source: [getdx production-readiness checklist](https://getdx.com/blog/production-readiness-checklist).

For a Series A-B-stage company, production-readiness work should prioritize automation, observability, and rollback/blast-radius containment over comprehensive documentation or enterprise sign-off gates. getdx's checklist has a dedicated "High-growth startups (Series A-B)" section stating verbatim: "Focus on automation and lightweight processes. Prioritize observability and rollback capabilities over comprehensive documentation... Focus on blast radius," and a separate monitoring section listing exactly four items (metrics/dashboards/alerts/structured logging) framed by "How will we know this broke, and how will we know it's working?"

### Finding 4 — Enterprise-scale PRR practice is directional inspiration only, not a checklist to replicate

**Confidence: high.** Sources: [Google SRE book — reliable product launches](https://sre.google/sre-book/reliable-product-launches), [USENIX on PRRs](https://www.usenix.org/publications/loginonline/production-readiness-reviews-surprisingly-versatile-practice), [continuous PRR](https://josvisser.substack.com/p/the-continuous-production-readiness).

Google's SRE book treats load testing as a required gate for most launches and defines a six-dimension Production Readiness Review (architecture/dependencies, instrumentation/monitoring, emergency response, capacity planning, change management, availability/latency/efficiency performance), recommending readiness review start in parallel with the design doc because non-functional capabilities are hard to retrofit. Several attempts to find a "lightweight" or "startup-scaled" version of this same framework were checked and refuted — no verified scaled-down template exists in the sourced literature, so this should be cited only as directional inspiration, not replicated wholesale for a first pilot customer.

### Finding 5 — A real comparable-vendor incident validates a specific architecture pattern: decouple data plane from control plane

**Confidence: medium.** Source: [Helicone AWS incident postmortem](https://www.helicone.ai/blog/aws-account-incident).

A real production incident at a comparable gateway/observability vendor (Helicone) — an AWS Bedrock key false-positively flagged as compromised, triggering a full-account lock that blocked ECS-based control-plane/observability tasks for ~4 days — is a concrete illustration of the failure shape a first real incident can take. It validates a specific architecture insight: the customer-facing proxy/data-plane stayed fully operational throughout because it was architecturally decoupled from the locked control-plane stack. Rated medium confidence as a single self-reported primary source with no independent corroboration found — treat as an illustrative case study, not an industry statistic. This is a build_now action item: verify now that Kelvran's own gateway data plane doesn't depend on non-critical control-plane/observability infra to keep serving traffic.

### Finding 6 — Runbook discipline realistic to enforce now vs. content that must wait for a real incident

**Confidence: medium.** Source: [SRE runbook-writing series](https://chroniclesofasre.substack.com/p/writing-runbooks-engineers-actually).

Every paging alert should be mapped 1:1 to a runbook (write one or delete the alert), and "runbook coverage" (% of paging alerts with a current, reviewed runbook) is a trackable operational-health metric. But real incidents are necessary to properly stress-test and refine runbook content in ways scheduled review cannot replicate — runbook *accuracy* should be treated as provisional until validated by an actual incident. The governance rule and coverage metric are build_now once alerts exist; full runbook accuracy is effectively not_yet, gated on "first real incident occurs."

### Finding 7 — Composite minimum-viable on-call/alerting setup, achievable now with existing signals

**Confidence: medium.** Sources: getdx, LiteLLM-in-production, SRE runbook series (as above).

The minimum viable on-call/alerting setup that doesn't require investing in a big observability platform is achievable by composing: metrics-per-key-operation + dashboards + threshold alerts + structured/queryable logs (already largely covered by Kelvran's existing `gen_ai.*`/`kelvran.*` OTel signals), a strict alert-to-runbook mapping rule, and a runbook-coverage-% tracking habit. Notably, this is roughly the same ceiling that a real comparable vendor (LiteLLM's own production-ops guide) currently operates at, with no PagerDuty-class tooling or formal on-call rotation.

## Caveats

Several plausible-sounding, closely-related claims were researched but did NOT survive adversarial verification and should not be reused even though they read as directly on-point: Google's "lightweight Consultation alternative" to a full PRR; the "Simple PRR" six-category enterprise baseline as a scale-down reference; PRRs being scoped around "Observability/Reliability/Incident Handling/Scalability/Security/Disaster Recovery"; a vendor checklist treating on-call rotation + threshold paging as a standard pre-launch requirement and SLO/error-budget definition as "non-negotiable"; a rule of thumb to only write a runbook after an alert has fired more than once; the "48-hour postmortem, one page max" rule; "write playbooks before runbooks" sequencing advice and the "document only the thing that keeps breaking" illustrative example; a fixed 7-section pre-incident runbook template; and a runbook-vs-playbook word-count/scope split.

Beyond the refuted list: sourcing throughout leans blog/vendor-consultancy rather than peer-reviewed or large-sample data — only Google's SRE book and Portkey's own status page qualify as strong primary sources; treat any percentage/threshold figures as heuristics, not empirically validated targets. The Helicone and Portkey incident data points are self-reported by the vendors about their own outages with no independent corroboration found, so treat them as illustrative single-source case studies. Time-sensitivity: the Portkey and Helicone incidents are recent (mid-2026, close to the 2026-09-12 research date), which is good for currency but this is also a fast-moving space where competitive status-page/ops practices could shift.

## Open Questions

- What specific numeric SLO/SLA or error budget should Kelvran eventually commit to once real pilot traffic exists — no verified source (including comparable-product status pages) gave a concrete target; this genuinely stays open until real traffic data accrues.
- How many real incidents or alert-firings should elapse before a given runbook is treated as "validated" rather than provisional? A specific heuristic ("only write a runbook after an alert has fired more than once") was researched but did not survive verification, so this threshold remains unresolved.
- Should Kelvran stand up a Portkey-style public status page (trailing uptime + dated/timed incident log) before or only after the first pilot customer signs, given it functions as both an SLA substitute and a trust-building/credibility signal during pilot sales conversations?
- Does any verified, startup-scaled version of Google's six-dimension PRR framework exist in the published literature? Two candidate claims proposing a "lightweight Consultation alternative" and a "Simple PRR" enterprise-baseline checklist were both researched and refuted, so no such scaled-down template was confirmed — this gap remains open.

## Refuted Claims (excluded from findings above)

- Google's internal Launch Coordination Checklist requiring inclusion be justified by a prior real incident, not anticipatory speculation (0-3).
- Google SRE's "lightweight Consultation" alternative to a full PRR (0-3).
- The "Simple PRR" six-category enterprise baseline as a scale-down reference (1-2).
- PRRs scoped around Observability/Reliability/Incident Handling/Scalability/Security/Disaster Recovery (0-3).
- On-call rotation + threshold paging as a standard pre-launch requirement (0-3); SLO/error-budget definition as "non-negotiable" (1-2).
- Only write a runbook after an alert has fired more than once (0-3).
- 48-hour postmortem, one page max (0-3).
- Write playbooks before runbooks sequencing advice (1-2); "document only the thing that keeps breaking" example (1-2).
- Fixed 7-section pre-incident runbook template (1-2).
- Runbook-vs-playbook word-count/scope split (0-3).

## Sources Consulted

21 sources fetched across 5 search angles (scaled-down production readiness checklist; comparable infra gateway startups; pre-incident runbook scoping; SLO calibration with no historical traffic; minimum viable on-call/alerting on existing OTel data); 93 claims extracted, 25 adversarially verified (13 confirmed, 12 refuted, 0 unverified).
