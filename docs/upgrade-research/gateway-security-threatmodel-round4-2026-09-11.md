# Gateway Security/Threat-Modeling Research — Round 4 (2026-09-11)

Scope: `gateway/`'s security posture against 2026 LLM-gateway/API-security practice —
guardrail evasion resistance, supply-chain CVE automation, secrets lifecycle, admin
governance, multi-tenant blast-radius containment, tool-result prompt injection. Grounded
against `THREAT_MODEL.md`/`SECURITY.md` (reconciled with round-3 features 2026-09-10),
externally verified against OWASP LLM Top 10 (2025 Edition), NIST AI RMF/AI 600-1,
LiteLLM/Portkey/Kong AI Gateway's own published security features, the InjecAgent benchmark,
and current supply-chain-scanning tooling docs, each claim adversarially 3-vote-verified
before inclusion here.

## Executive Summary

Across the seven research questions, the single most concrete and actionable gap is that
Kelvran's Guardrails scan tool-call ARGUMENTS but not tool-call RESULTS re-injected into
later turns — a distinct attack surface formalized by the InjecAgent benchmark (24%
base-rate compromise for a ReAct-prompted GPT-4 agent) and treated as scanned-by-default in a
comparable production gateway (LiteLLM, which requires an explicit opt-out flag to stop it);
this is a BUILD-NOW item, not a deferred one. Supply-chain CVE automation is likewise a real,
cheaply-closable gap: ready-made, low-effort tooling already exists off-the-shelf
(`govulncheck` as a blocking reachability-scoped Go CI gate; `pip-audit`'s official GitHub
Action + pre-commit hook backed by the PyPI Advisory DB/OSV.dev) rather than requiring custom
scripting, so if not yet wired it should be wired at the next CI touch. The originally
hypothesized regex-evasion gap (paraphrase-based PII exfiltration, multi-turn slow-drip
disclosure) did NOT survive adversarial verification and should remain a documented v1 scope
limit rather than being escalated to urgent. Multi-tenant anomaly-triggered key suspension
and enterprise-grade RBAC/SSO/compliance-certification parity are real maturity gaps versus
named competitors (Kong, Portkey) but are "not yet" items gated behind concrete triggers (a
real containment-failure incident, or a first enterprise/compliance-driven customer
requirement) rather than urgent defects. **No CRITICAL vulnerability was identified in this
research round.** Secrets-rotation guidance and centralized admin audit-log/SIEM export
remain open questions with no surviving evidence either way.

## Findings

### Finding 1 — Tool-call RESULTS scanning gap: real, distinct, currently exploitable (HIGH, not CRITICAL)
**Confidence: high**

Kelvran's Guardrails cover tool-CALL arguments (post-call) but there is a real, distinct,
currently-relevant gap in scanning tool-call RESULTS fed back into a later turn. LiteLLM (a
comparable production LLM gateway) scans `role:tool` content by default across its unified
guardrail path (Presidio, Bedrock guardrails, `litellm_content_filter`, OpenAI Moderation,
Generic Guardrail API, custom `apply_guardrail` guardrails, Lakera v2) and ships a dedicated
flag whose sole purpose is to OPT OUT of that default scanning — implying scan-by-default is
the industry-standard posture, not the exception. The InjecAgent benchmark formalizes exactly
this threat model (malicious instructions embedded in tool outputs/observations, explicitly
distinguished from tool-call arguments) and measured a 24% attack-success rate against a
ReAct-prompted GPT-4 agent. The design-patterns literature (six named control-flow-isolation
patterns, e.g. Plan-Then-Execute, co-authored by Google/Microsoft/IBM researchers) confirms
this is architecturally hard: fixing the action plan before tool output is seen stops
control-flow hijacking from injected tool-result instructions, but does NOT stop
content-level tampering — so plan-fixing alone does not substitute for inspecting tool-result
content. Note: LiteLLM's tool-message-skip control does NOT extend to a large second tier of
named integrations (Aporia, DynamoAI, Javelin, Lasso, Pangea, Model Armor, Azure Content
Safety, Guardrails AI, AIM, Cato Networks) that run via direct hooks on the raw request
instead of the unified path — relevant if Kelvran's own Guardrails architecture resembles a
direct-hook design.

**Verdict: BUILD NOW.** Named trigger: before or alongside shipping any agentic/tool-use
feature where tool results are fed back into a later LLM turn — especially when tool results
can carry externally-sourced/untrusted content (web-fetch, email, document retrieval, RAG).
**Not CRITICAL** under this repo's own Response Protocol (no live exploit was demonstrated
against Kelvran's own code — the finding is comparative/architectural), but **HIGH
priority**: the single most concrete, benchmarked, currently-exploitable-in-the-wild class of
gap surfaced across all seven questions, and extending the existing Guardrails hook (same
regex/PII/injection heuristics) to also cover `role:tool`-equivalent content is a
comparatively low-effort reuse of infrastructure Kelvran already has, not a new subsystem.

### Finding 2 — Supply-chain CVE automation: cheap, ready-made, likely unwired
**Confidence: high**

Supply-chain CVE automation has a real, cheaply-closable gap between "manual point-in-time
check" and "automated CI wiring" — but the fix is off-the-shelf, not a design problem. A
concrete, currently-verified example: `langwatch/langwatch` wires `govulncheck` as a BLOCKING
Go dependency gate directly in CI (`govulncheck@v1.1.4` scoped to reachability across
services/pkg/cmd/tools, on push-to-main/PR/merge_group/workflow_dispatch/weekly cron). The
official Go vulnerability database (vuln.go.dev) aggregates NVD + GitHub Advisory DB + direct
maintainer reports and is manually curated by the Go Security team using the OSV schema.
For Python, `pip-audit` has an official GitHub Action (`pypa/gh-action-pip-audit`) and
documented pre-commit hook support, sourcing from the Python Packaging Advisory Database
(default) or OSV.dev — a ready-made path, not custom scripting. GitHub Dependabot version
updates are opt-in and require an explicit `dependabot.yml`; Dependabot security updates
(CVE-triggered PRs) and version updates (routine freshness PRs) are two independently
configurable mechanisms, not one — "Dependabot is on" does not by itself imply CVE-triggered
patching is wired.

**Verdict: BUILD NOW.** Named trigger: at the next CI/CD pipeline touch, or before the next
dependency bump — this is a low-cost, ready-made addition (one GitHub Action/YAML block per
language), not a research or architecture decision. **Not CRITICAL** (no evidence any
specific current Kelvran dependency has an active exploited CVE was produced in this research
pass — only that the automation infrastructure to catch one continuously is cheap and
unused/unverified), but worth prioritizing precisely because it is cheap relative to its risk
reduction.

### Finding 3 — RQ1's original regex-evasion framing did NOT survive verification: remains an acceptable v1 scope limit
**Confidence: medium**

The originally-hypothesized framing — that Kelvran's regex/checksum Guardrails have a real,
urgent, currently-checkable gap specifically against paraphrase-based PII exfiltration or
multi-turn "slow-drip" disclosure, comparable to a live 2025/2026 OWASP LLM02/LLM03 ranking —
did NOT survive adversarial verification. Refuted: OWASP LLM02 as the current top-10 ranking
(1-2); OWASP LLM03 (Supply Chain) as a standalone carried-over category (1-2); the specific
mechanism that paraphrasing evades pattern-based/regex filters more effectively than explicit
steering instructions (0-3); the multi-turn "trigger re-injection" amplification mechanism
(1-2). One adjacent claim did survive (Back-Reveal paper, 3-0): even ML-based defenses
(reranker ensembles, NeMo Guardrails, LLM Guard) failed against a specific exfiltration
attack — but the paper's own stated reason is architectural (inspecting retrieval/prompt
content, not the tool-call payload carrying the exfiltrated data), which restates Finding 1's
tool-results gap, not new evidence that regex-vs-ML detection quality is the actual weak link.

**Verdict: NOT YET for the RQ1 framing as originally posed** — should remain a documented,
still-acceptable v1 scope limit, consistent with this repo's own established pattern of
treating documented scope limits (e.g. `--judge-debias`) as deliberate, not deficient. Named
trigger to revisit: if/when Kelvran adds RAG or retrieval-augmented tool results as a
first-class feature (reintroducing the Back-Reveal-style risk via Finding 1's mechanism), or
a real incident of paraphrase-based PII exfiltration against Kelvran specifically.

### Finding 4 — Multi-tenant blast-radius containment: at 2026 industry parity, not lagging
**Confidence: medium**

Kong AI Gateway, a comparable production LLM gateway, also implements only static
user/model/time-bound quotas for cost/abuse containment. An exhaustive keyword sweep of
Kong's own AI Gateway product page and its dedicated AI Rate Limiting Advanced docs page
found zero mentions of anomaly, suspend, abuse, compromised, circuit-breaker, throttle,
spike, auto-suspend, kill-switch, or noisy anywhere.

**Verdict: NOT YET** for automatic anomaly-triggered key suspension. No named competitor (at
least Kong, confirmed) has shipped this as a product feature, so this is a genuine
industry-wide gap rather than a Kelvran-specific deficiency. Named trigger: a real incident
where static budget/rate-limits alone failed to contain a compromised-key blast radius before
human intervention, or a documented case of a named competitor shipping anomaly-detection-based
auto-suspension as a standard feature — worth re-checking in 6-12 months.

### Finding 5 — Admin governance maturity gap vs. Portkey enterprise tier: real, but enterprise-demand-gated
**Confidence: medium**

Portkey's enterprise tier advertises two features beyond Kelvran's current two-tier
admin/read-only-viewer credential model (confirmed via direct grep of `THREAT_MODEL.md`,
`SECURITY.md`, gateway/ Go source, and docs/ turning up zero matches for rbac, sso, oidc, or
role-based): (a) enhanced RBAC plus SSO via OIDC, gated to Portkey's enterprise/custom-pricing
tier; and (b) formal, self-reported third-party-audited compliance certifications — SOC2,
ISO27001, GDPR, and HIPAA — backed by a public Trust Portal. NIST also published a dedicated
companion profile for generative AI risks (NIST AI 600-1, July 2024), distinct from and more
current/specific than the base AI RMF alone.

**Verdict: NOT YET** for RBAC+OIDC SSO and formal compliance certification — enterprise-tier,
customer/compliance-demand-driven features, not exploitable security weaknesses in Kelvran's
current constant-time two-tier credential model. Named trigger: the first enterprise
customer/prospect requiring SSO federation or a specific compliance certificate as a purchase
condition. **BUILD NOW** as a docs-only addition: a NIST AI 600-1 crosswalk section extending
`THREAT_MODEL.md`'s already-established OWASP-crosswalk pattern to a second framework — cheap,
no code change. Centralized audit-log/SIEM export (the original RQ4 framing) was NOT directly
evidenced by any surviving claim and remains a genuinely open question.

## Caveats

No CRITICAL vulnerability was identified in this research round — every finding is a
comparative/architectural gap against industry practice or academic threat models, not a live
exploitable bug discovered in Kelvran's own code. Several sub-questions returned no surviving
evidence either way — RQ3 (secrets rotation) and the centralized-audit-log-export half of RQ4
simply produced no claims that survived 3-vote adversarial verification, so absence of a
confirmed claim there is not proof of absence of a gap. The Portkey and Kong claims are drawn
from vendor marketing/product pages (2-1 votes, medium confidence), not independent audits.
The InjecAgent 24% GPT-4 figure is from a 2024 benchmark — a citable historical figure, not a
live 2026 measurement, though the structural threat-model distinction it demonstrates (tool
observations vs. tool-call arguments as separate attack surfaces) is definitional and doesn't
go stale. This synthesis did not independently re-run the Kelvran-repo-side negative greps
(RBAC/SSO/audit-log-export terminology) itself, and did not run an actual CVE scan against
Kelvran's own `go.mod`/requirements manifests to check point-in-time real exposure.

## Open Questions

- Secrets rotation in a genuine production deployment: is the lack of secrets-manager
  integration guidance a real docs gap or a real feature gap at current maturity?
- Centralized admin audit-log export to a SIEM: real, currently-checkable gap, or premature
  for Kelvran's current maturity? Portkey/Kong/LiteLLM's own audit-log-export capabilities
  were not checked in this pass.
- Point-in-time CVE exposure: automation tooling is cheap and ready-made, but neither
  `govulncheck` nor `pip-audit` was actually run against Kelvran's current manifests in this
  pass.
- Whether Portkey's or LiteLLM's own cost/abuse containment models include anomaly-triggered
  automatic key suspension (only Kong was checked and confirmed absent).
