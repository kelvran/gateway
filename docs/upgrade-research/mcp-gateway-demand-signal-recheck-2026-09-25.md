# MCP Gateway Demand-Signal Recheck — Closing the "Is Anyone Actually Asking For This" Half of the Trigger (2026-09-25)

**Date:** 2026-09-25
**Scope:** `gateway/internal/mcp` (still zero code). Specifically re-testing the one sub-question the prior research pass (`docs/upgrade-research/mcp-a2a-ecosystem-maturity-check-2026-09-24.md`, Finding 5) explicitly could not resolve either way: is there real, dated, primary-source evidence of an operator or developer asking for MCP/A2A tool-brokering to happen **at the gateway/proxy layer itself** — as distinct from the already-well-served demand for MCP support *inside* an agent framework — on a comparable self-hosted LLM gateway (LiteLLM, Portkey, Kong AI Gateway, Envoy AI Gateway) or a major agent-orchestration framework (LangGraph, CrewAI, OpenAI Agents SDK, Claude Agent SDK, Microsoft AutoGen/Agent Framework)? Grounded against both `docs/rfcs/2026-09-11-gateway-mcp-outbound-credential-design.md`'s and `docs/rfcs/2026-09-20-gateway-mcp-inbound-design.md`'s own stated trigger conditions ("wait for MCP's own spec stabilizing AND a first real integration" / "no real demand signal yet"). Neither RFC's status changes as a result of this document — this is evidence-gathering only.
**Method:** Adversarial multi-source research (3-vote verification per claim) against GitHub issues, discussions, and pull requests on the named gateways and frameworks. 25 claims were put to adversarial vote; 14 survived, all unanimous 3-0, and 11 were refuted (six at 0-3, five at 1-2). Every surviving claim below is grounded in a primary source — an actual, dated GitHub issue/PR/comment thread fetched live via `gh`/the GitHub API, never a secondary blog post's unsourced claim. This document adds the Kelvran-specific "why it matters here / does it close the trigger" synthesis layer the raw adversarial verification deliberately does not do.

---

## Executive summary

Unlike the 2026-09-24 pass — which searched spec text and vendor product documentation and found zero evidence either way — this pass searched GitHub issue trackers directly and found real, dated, primary-source evidence that the demand class the research question asked about does exist: on LiteLLM (issue #7934, opened 2025-01-23 by a self-identified ML Ops Team member, seconded independently by a second operator who later became a code contributor) and on Envoy AI Gateway (issue #589, opened 2025-04-26), real developers explicitly asked for MCP tool-brokering to happen at the gateway/proxy layer — not inside an agent framework — and both requests were subsequently built and shipped. A third dimension of evidence, specific to A2A rather than MCP, is stronger still: LiteLLM's shipped A2A Agent Gateway has real, unaffiliated operators filing production friction reports against it (issue #21409), which is evidence of active use, not just a request. Set against that: on Portkey and on Kuadrant's own dedicated `mcp-gateway` project, the pattern reverses — their shipped MCP-gateway features trace to internally-initiated maintainer work with no linked external issue, and every agent-framework thread checked (CrewAI, OpenAI Agents SDK, LangGraph) stayed strictly framework-scoped with zero mention of relocating brokering to a gateway layer. Net: the honest answer to "is anyone asking for this" is no longer a flat "no evidence found" — it is "yes, on two comparable OSS gateways, from a small number of named developers, with one gateway's A2A equivalent already showing real production friction reports" — but this is demand for the *class of capability* observed in the wider ecosystem, not demand for it *on Kelvran specifically*, which remains structurally unanswerable until Kelvran has real external users.

---

## Findings, ranked

### 1. Real, dated, primary-source operator demand for gateway-layer MCP tool-brokering exists on LiteLLM — from at least two independent voices, predating and then followed by a shipped feature (High confidence)

**What:** GitHub issue [BerriAI/litellm#7934](https://github.com/BerriAI/litellm/issues/7934) ("[Feature] MCP bridge support"), opened 2025-01-23 by James4Ever0 (self-identified ML Ops Team), explicitly asks that "LiteLLM will read a MCP server config and act as a middle man between the MCP server, the LLM server and the LLM client" — modeled on the standalone SecretiveShell/MCP-Bridge gateway-layer proxy pattern. A commenter (forpr1093, 2025-03-21) explicitly disambiguates the ask from framework-side MCP support: the requester wants "LiteLLM to act as a bridge between the clients and the MCP Server... rather than having LiteLLM function as an MCP Server itself." A second operator, wagnerjt (later a prolific code contributor on this exact feature — PRs #10699, #10707, #10634, #10643, #11968), independently states the value proposition in his own words: "a MCP-bridge solution would be really great for a single ease of use point for the various deployed MCP servers" — i.e., gateway-layer consolidation of many deployed MCP servers behind one endpoint, not per-framework integration. Maintainer ishaan-jaff added the feature to LiteLLM's roadmap (2025-03-11) and it shipped at the proxy layer per PR #9642 ("v1.65.1-nightly... LiteLLM Proxy to act as the MCP bridge with multiple servers over SSE," 2025-04-01), preceded by an SDK-level bridge (PR #9436, 2025-03-22).

**Why it matters for this recheck:** This directly reverses, for one comparable gateway, the 2026-09-24 pass's "zero dated, primary-source evidence" conclusion. The demand is unambiguously gateway/proxy-layer (a commenter in the thread itself draws the exact framework-vs-gateway distinction the research question asked about), it is dated and primary-source (a real, live GitHub issue, not a secondary summary), and it predates the shipped feature rather than being written after the fact to rationalize it.

**Consistent with the RFCs' own stated triggers?** Directly answers the "is anyone actually asking for this... on a gateway specifically" half of the outbound RFC's demand framing — for LiteLLM, yes. Does not, and cannot, answer it for Kelvran itself, which has no external users to ask.

**Grounding:**
- Issue body and forpr1093's disambiguating comment, verified verbatim via `gh issue view 7934 --repo BerriAI/litellm` [github.com/BerriAI/litellm/issues/7934 — confirmed 3-0].
- wagnerjt's 2025-03-21T13:09:15Z comment, verified verbatim, plus his subsequent PR history confirming the "code contributor" follow-through [github.com/BerriAI/litellm/issues/7934 — confirmed 3-0].
- **Caveat, not a refutation:** a separate, more narrowly-worded framing of the same disambiguation point was independently put to vote and refuted (0-3) — the underlying fact (a commenter distinguishes gateway-layer brokering from framework/SDK-side MCP support) survived in the wording above but a stricter formulation of it did not. Two further claims asserting a *specific* implementation PR (#9426) as the shipping mechanism, and asserting that PR's description shows zero motivating issue, were also refuted (0-3 each) — the general "this shipped at the proxy layer" fact is solid (via #9642/#9436, confirmed above), the specific PR-number framing is not.

**Concrete next step:** No RFC change needed today — this is evidence to file, not a build trigger. Worth citing alongside the existing 2026-09-24 addendum in `docs/rfcs/2026-09-11-gateway-mcp-outbound-credential-design.md` the next time that RFC's demand framing is revisited, so this specific citation doesn't need re-research.

**Effort:** N/A — evidentiary finding, not an implementation step.

---

### 2. A second, independent comparable OSS gateway (Envoy AI Gateway) received the same kind of dated, primary-source, gateway-layer MCP proposal — and it also shipped (High confidence)

**What:** [theagentrouter/agent-router#589](https://github.com/theagentrouter/agent-router/issues/589) ("[Proposal] Support for MCP protocol"), opened 2025-04-26 by yanavlasov against what was then Envoy AI Gateway (`envoyproxy/ai-gateway`, confirmed via GitHub's own redirect to be the same project under its current name/org), lists concrete gateway-layer capabilities the proposal is explicitly for: routing policy with retries, request authentication, per-RPC-method authorization, RPC access logging, per-RPC rate limiting, and JSON-RPC-to-gRPC/OpenAPI transcoding — i.e., the proxy itself doing MCP brokering, not an agent framework. The proposal was not abandoned: it closed `completed` on 2025-10-03, cross-referenced to a related issue (#775, "Add Support for MCP / Agent Gateway," explicitly proposing both MCP and A2A at the gateway, filed by an AMD/vllm-project contributor) and to merged PR #1260 ("feat: adds complete MCP proxy implementation," full MCP 2025-06-18 spec + OAuth).

**Why it matters for this recheck:** A second, independent data point in the same shape as Finding 1 — a real, dated, primary-source proposal explicitly scoped to the gateway/proxy layer, not a framework, that was actually built. Two independent gateways receiving and shipping the same class of request is a stronger signal than either alone.

**Consistent with the RFCs' own stated triggers?** Same answer as Finding 1, for a second gateway: yes, this class of demand is real and observable in the comparable-gateway ecosystem; still not evidence specific to Kelvran.

**Grounding:**
- Issue #589 body, verified verbatim via GitHub's API, labeled `enhancement`/`area-mcp` [github.com/theagentrouter/agent-router/issues/589 — confirmed 3-0].
- Closure state, cross-reference to #775, and merged PR #1260, independently confirmed against the live repo [github.com/theagentrouter/agent-router — confirmed 3-0].
- **Caveat:** the requester (yanavlasov) has no listed company/bio, and the implementers are maintainers/community members, not self-identified production operators citing an acute deployment pain point — this is stronger evidence of "a developer specifically asking for X at the gateway layer" than of "a paying customer's operational demand." A separate claim asserting an *additional* independent third-party developer seconded this request the same day was put to vote and refuted (0-3) — that specific corroboration did not survive, though the core proposal-and-shipped-outcome above did.

**Concrete next step:** N/A — evidentiary finding.

**Effort:** N/A.

---

### 3. Two further independent gateway/proxy products (Pomerium, Higress) also built MCP-at-gateway support in the same window — supply-side ecosystem corroboration, not itself an independent demand signal beyond Findings 1–2 (Medium confidence for demand-relevance; High confidence for the underlying shipped-product facts)

**What:** Pomerium — a real, production, Envoy-based identity-aware access proxy — was confirmed "actively working on" MCP gateway support as of 2025-06-27 by a Pomerium employee (`envoyproxy/envoy#39174`, commenter desimone), cross-linked to Pomerium's own tracking issue (`pomerium/pomerium#5672`, opened 2025-06-25 by core contributor wasaga), which closed `completed` 2025-12-30 with MCP routes now a real, shipped feature. Separately, Higress (an Envoy-based API gateway) had already built and shipped a production MCP Server Wasm plugin plus a live public marketplace (`mcp.higress.ai`, independently confirmed live via a Wayback Machine snapshot dated 2025-04-17) by 2025-05-09 — predating any native Envoy MCP filter.

**Why it matters for this recheck:** Both data points are genuinely useful — they show the "build MCP brokering at the gateway layer" pattern is not confined to LiteLLM and Envoy AI Gateway, it recurs independently across at least four gateway-adjacent products. But neither is itself a *demand* signal in the way Findings 1–2 are: Pomerium's issue #5672 was filed by a core Pomerium contributor (not an external customer), and the Higress evidence describes what shipped, not who asked for it. These are supply-side/ecosystem-trend facts, the same category as the AWS/Microsoft/Cloudflare precedent the 2026-09-24 pass already found on the *outbound-credential* design question — useful corroboration that the pattern is real and recurring, but not new evidence on the specific "is anyone asking" question this document exists to answer.

**Consistent with the RFCs' own stated triggers?** Reinforces "a first real integration [pattern] emerged" more than it reinforces "real demand signal" — the same distinction the 2026-09-24 pass drew about AWS/Microsoft/Cloudflare.

**Grounding:**
- desimone's 2025-06-27T17:37:45Z comment (confirmed Pomerium employee via GitHub profile) and pomerium/pomerium#5672's full lifecycle (opened by a 331-contribution core contributor, closed `completed`, later cross-referenced by a follow-on shipped-feature issue) [github.com/envoyproxy/envoy/issues/39174, github.com/pomerium/pomerium — confirmed 3-0].
- johnlanni's 2025-05-09T08:10:11Z comment describing the Higress MCP Wasm plugin and marketplace, independently corroborated by Higress's own tagged GitHub releases (v2.1.0 through v2.1.3, all dated before or on 2025-05-09) and a Wayback Machine snapshot of the live marketplace three weeks prior [github.com/envoyproxy/envoy/issues/39174, github.com/alibaba/higress — confirmed 3-0].

**Concrete next step:** N/A — evidentiary/corroborating finding.

**Effort:** N/A.

---

### 4. Where checked most closely, the opposite pattern also holds: Portkey's and Kuadrant's own MCP-gateway features trace to internally-driven work with zero linked external customer issue (High confidence)

**What:** Portkey's "MCP Gateway" PR #1343 (opened 2025-09-15 by internal collaborator roh26it) and its earlier PR #1285 have no `Closes #X`/`Fixes #X` reference, no quoted customer feedback, and no support-ticket citation anywhere in the description or ~79-commit history — despite two real, pre-existing open feature-request issues (#926, #1151) sitting unreferenced in the same repo, both self-filed by a Portkey team member (vrushankportkey), not an external requester. Every GitHub issue in Portkey-AI/gateway that mentions "MCP" filed after the feature shipped (eight checked, Jul–Sep 2026) is a third-party vendor asking to be *listed in Portkey's public MCP-server directory* — a marketing/discoverability ask, not a request for gateway-layer brokering functionality. Separately, Kuadrant's own dedicated `mcp-gateway` project — itself a purpose-built "MCP at the gateway" product, which is in one sense further ecosystem corroboration like Finding 3 — traces issue #253 (the one PR #1531 implements) to maintainer/core-contributor maleck13, framed as the broker's own credential-handling shortcoming, refined in an internal sprint-grooming comment ("not enough priority for 0.7") and linked to an internal Red Hat Jira ticket — zero external voices anywhere in the thread.

**Why it matters for this recheck:** This is the honest counter-evidence the research brief explicitly asked not to paper over. Two of the four comparable self-hosted gateways checked show the *opposite* of Findings 1–2: a shipped MCP-gateway feature with no traceable external demand behind it at all, and a post-launch issue stream that, on inspection, is unrelated to gateway-layer brokering demand.

**Consistent with the RFCs' own stated triggers?** Directly supports treating "no demand signal" as still partly true — just not universally true across every comparable gateway, per Findings 1–2.

**Grounding:**
- PR #1343/#1285 bodies and full commit history, all ~79 commits inspected for issue references or quoted feedback (none found); issues #926/#1151 confirmed self-filed by a Portkey employee via author lookup [github.com/Portkey-AI/gateway — confirmed 3-0].
- All eight cited post-launch "MCP" issues (#1820, #1813, #1812, #1811, #1810, #1790, #1749, #1744) read directly via `gh`, each confirmed to be a directory-listing request, none proposing gateway-layer functionality [github.com/Portkey-AI/gateway — confirmed 3-0].
- Kuadrant/mcp-gateway issue #253's body, its three comments (all from confirmed Kuadrant/Red Hat team members), the linked internal Jira ticket, and maleck13's `CONTRIBUTOR`-tagged but 127-PR-deep insider status on the same repo, all confirmed via the GitHub API [github.com/Kuadrant/mcp-gateway — confirmed 3-0].
- **Caveat:** a claim framing Portkey's PR #1285 specifically (rather than #1343) as the internally-initiated origin point was put to vote and refuted at 1-2 — close, but not confirmed; the #1343/commit-history evidence above is what survived. A similarly-close claim about Kuadrant's internal deprioritization comments was also refuted 1-2 for the same reason — the broader "internally-driven, zero external voice" finding above is what survived at 3-0, a more specific sub-framing of the same point did not.

**Concrete next step:** N/A — evidentiary finding.

**Effort:** N/A.

---

### 5. On the agent-framework side, demand — where it exists at all — stays entirely in-framework; no thread among CrewAI, OpenAI Agents SDK, or LangGraph asks for MCP tool-brokering to move to a gateway/proxy layer (High confidence)

**What:** CrewAI's `#1813` (opened 2024-12-28, 44 comments through 2026-03-17) scopes its entire MCP "Vision" as agents taking an MCP server address directly ("the agent can discover tools/resources hosted within that MCP server... use the tools as and when required") — an in-framework adapter (`mcpadapt`, later native `ToolCollection.from_mcp`), not a gateway sitting outside the framework. A full-text search of all 44 comments for gateway/proxy/broker/hub/centralized/LiteLLM/Kong/Envoy (word-boundary-checked to avoid false positives from GitHub URLs) returned zero genuine hits. Separately, OpenAI Agents SDK issue `#2074` — initially a promising-looking "MCP server session not recognized when using MCP servers along with LiteLLM proxy" bug — was root-caused by the reporter (2025-11-26) to a client-side LiteLLM-adapter tool-definition serialization defect, explicitly framing "tool calling happening on the LLM/proxy instead of agent runtime" as the bug being fixed, i.e. the reporter wanted execution to stay in the agent runtime, not move to a gateway. A LangGraph issue about an OpenAI-compatible gateway integration (`#8957`, opened 2026-09-17) contains zero mention of MCP or A2A at all — a pure API-surface-mismatch bug report, a negative data point consistent with "still no evidence found" for LangGraph specifically.

**Why it matters for this recheck:** This is the clean control case the research question needs: three real, dated, checked-in-full threads on the framework side of the framework/gateway distinction, and all three land squarely on the framework side, corroborating that the distinction the research question drew (framework-level MCP demand vs. gateway-level MCP demand) is a real, observable line in practice, not an artificial one.

**Consistent with the RFCs' own stated triggers?** Neutral-to-negative on the demand question — no evidence here moves the "is anyone asking for this on a gateway" needle either way; it simply confirms framework-level demand (well documented, already served) is a distinct pool from gateway-level demand.

**Grounding:**
- CrewAI #1813 body, all 44 comments (paginated), and the shipped `mcpadapt`/`ToolCollection` resolution, all confirmed via the GitHub API [github.com/crewAIInc/crewAI/issues/1813 — confirmed 3-0, twice independently for the scoping claim and the full-thread-search claim].
- OpenAI Agents SDK #2074's closing comment, verified verbatim and timestamp-matched to the issue's own `closed_at`/`closed_by` fields [github.com/openai/openai-agents-python/issues/2074 — confirmed 3-0].
- LangGraph #8957's body and its one comment, confirmed to contain zero MCP/A2A mentions [github.com/langchain-ai/langgraph/issues/8957 — confirmed 3-0].
- **Caveat:** a separately-worded claim characterizing #2074 specifically as "a framework-side MCP client bug" (rather than the LiteLLM-adapter framing above) was refuted 1-2 — the two framings describe closely related but not identical root causes; the LiteLLM-adapter framing is what survived. A separate claim about a different CrewAI thread (`#4875`, on hardening MCP tool calls) was also refuted 1-2 and is not relied on here.

**Concrete next step:** N/A — evidentiary finding.

**Effort:** N/A.

---

### 6. The A2A half of the compound question has a stronger, different-in-kind signal: real operators are actively using a shipped gateway-layer A2A broker in production, not just requesting the concept (High confidence)

**What:** LiteLLM's proxy already ships an "A2A Agent Gateway" — agents registered in `config.yaml`, routed through `POST /a2a/{agent_id}`, JSON-RPC 2.0, with access control and cost tracking (`docs.litellm.ai/docs/a2a`, confirmed live in LiteLLM's own README). Issue `#21409` (opened 2026-02-17, still open as of this research date) is not a request to build this — it is a friction report from an operator already routing production traffic through it: "Without this, the LiteLLM Proxy cannot successfully route requests to protected A2A agents." At least two additional, unaffiliated commenters (`authorAssociation: NONE`) independently corroborate active or attempted production use of the same gateway-layer A2A-brokering feature.

**Why it matters for this recheck:** This closes the A2A half of the compound MCP/A2A question in the affirmative, and with a stronger evidentiary type than Findings 1–2: this is not a pre-launch feature request that might never get real usage, it is a post-launch, active-use friction report with independent corroboration — the clearest evidence anywhere in this recheck that real operators value gateway-layer tool/agent brokering enough to hit its edges in production and ask for it to be extended.

**Consistent with the RFCs' own stated triggers?** A genuinely useful, if scope-bound, data point: it is A2A-specific, not MCP-specific, so it closes the A2A half of the compound research question without itself establishing gateway-layer MCP demand. Neither RFC's own "not_yet" framing changes as a result — this is additional evidence to weigh, not a trigger being met.

**Grounding:**
- Issue #21409 body, verified verbatim via `gh issue view` [github.com/BerriAI/litellm/issues/21409 — confirmed 3-0].
- LiteLLM's own README linking the A2A docs, and the docs page itself describing the concrete, shipped routing mechanism [docs.litellm.ai/docs/a2a — confirmed 3-0].
- **Caveat:** a separately-worded claim naming one specific corroborating commenter (arnaujc91) as an "independent third-party user confirming the same need on their own deployment" was put to vote and refuted at 1-2 — the broader "at least two unaffiliated commenters corroborate active use" framing above is what survived at 3-0; the more specific single-commenter framing did not.

**Concrete next step:** N/A — evidentiary finding, and A2A-specific rather than MCP-specific.

**Effort:** N/A.

---

## Top takeaways

1. **The demand half of the trigger is no longer a flat "no evidence found" — it is a qualified "yes, on two of four comparable gateways, from a small number of named developers."** LiteLLM (Finding 1) and Envoy AI Gateway (Finding 2) both received real, dated, primary-source proposals explicitly scoped to gateway-layer MCP brokering — not framework-level MCP support — and both shipped. This is the first time this research thread has found genuine demand-side evidence rather than only supply-side (vendor-built) evidence.

2. **The same recheck also found real counter-evidence, and it should be weighed, not discarded.** Portkey and Kuadrant (Finding 4) both shipped MCP-gateway features with zero traceable external customer ask behind them, and every agent-framework thread checked (Finding 5) stayed strictly framework-scoped. The honest picture is a mixed ecosystem, not a uniform "yes" or "no."

3. **A2A has a stronger, later-stage signal than MCP does.** Finding 6's evidence is a post-launch production-friction report with independent corroboration — a materially stronger form of demand evidence than a pre-launch feature request, even a well-corroborated one like Findings 1–2. This asymmetry is itself worth naming, not just the raw fact that both protocols have some evidence.

4. **None of this evidence is, or could be, Kelvran-specific.** The research question's literal ask — "is anyone actually asking for this on Kelvran specifically" — remains structurally unanswerable, because Kelvran has no external users yet to ask. What this recheck answers is the narrower, useful proxy question: does this class of demand exist anywhere in the observable, comparable-gateway ecosystem at all? The answer is now yes, on a modest scale, where the 2026-09-24 pass could only say "not found."

---

## Caveats

- **This recheck used a different method than the 2026-09-24 pass, which explains the different result — it is not a contradiction to be resolved, it is two passes answering two different sub-questions.** The 2026-09-24 pass searched the MCP spec and vendor product documentation; this pass searched GitHub issue trackers directly on the named comparable gateways and frameworks. The demand evidence in Findings 1, 2, and 6 was always sitting in public GitHub issues — it simply wasn't the kind of source the prior pass's method was built to find.
- **The strongest MCP-specific evidence (Findings 1–2) is "a developer specifically asked for X at the gateway layer," not "a paying enterprise customer cited acute deployment pain in dollars."** Both requesters are individual contributors/community members, not named companies with quantified production stakes. This is real, dated, primary-source demand evidence — but it is a smaller, earlier-stage signal than an enterprise procurement conversation would be.
- **No surviving evidence exists for Kong AI Gateway, Claude Agent SDK, or Microsoft AutoGen/Agent Framework specifically**, despite all three being named explicitly in the research brief. This is an absence of evidence, not evidence of absence — consistent with the same pattern flagged for Portkey/TensorZero/Bifrost/Helicone in the sibling `docs/upgrade-research/gateway-2026-09-06.md` report's own caveats.
- **11 of the 25 claims originally extracted were refuted during adversarial verification** — six at 0-3, five at close 1-2 votes. Several of the refuted claims were near-duplicate, more specific framings of a confirmed claim's same underlying fact (e.g., a stricter wording of the LiteLLM disambiguation point in Finding 1, or a single-commenter framing of the A2A corroboration in Finding 6) — the broader fact survived at 3-0 even where a narrower or more specific version of it did not. This pattern (general claim confirmed, an overly specific restatement of it refuted) recurred often enough in this pass to be worth naming explicitly, rather than silently dropping the refuted claims.
- **Time-sensitivity:** the LiteLLM (Finding 1) and Envoy AI Gateway (Finding 2) feature-request-to-shipped timelines both date to early-to-mid 2025 — over a year old relative to this research date, though the Portkey/Kuadrant counter-evidence (Finding 4) and the LiteLLM A2A friction reports (Finding 6) are current through 2026-02 and 2026-09 respectively, so the overall picture is not stale.
- **This document does not change either RFC's `not_yet` status.** Per the existing 2026-09-24 addendum already recorded in `docs/rfcs/2026-09-11-gateway-mcp-outbound-credential-design.md`, the design half of the trigger has separately moved (AWS/Microsoft/Cloudflare precedent); this document only speaks to the demand half, and even a qualified "yes, elsewhere" is not the same as "yes, on Kelvran" — no RFC status change is implied or recommended by this research pass alone.

## Open questions

- Given that GitHub-issue-mining (this pass's method) surfaced real evidence a spec/vendor-doc sweep (the 2026-09-24 pass's method) did not, would the same issue-mining method applied to Kong AI Gateway's own tracker, or to LLM-gateway-adjacent community forums (Discord servers, r/LocalLLaMA, HN threads) not yet searched, surface further evidence for the three still-unanswered comparables (Kong, Claude Agent SDK, Microsoft AutoGen/Agent Framework)?
- Does the asymmetry between MCP's evidence (pre-launch feature requests, Findings 1–2) and A2A's evidence (post-launch production friction, Finding 6) reflect a real difference in operator urgency between the two protocols, or is it simply an artifact of LiteLLM's A2A gateway having shipped earlier and therefore having had more time to accumulate usage-stage feedback?
- Is the Portkey/Kuadrant pattern (Finding 4 — shipped features with no linked external demand) typical of how most gateway vendors build MCP/A2A support in this market generally, or are Portkey and Kuadrant specifically atypical compared to LiteLLM and Envoy AI Gateway (Findings 1–2)? A larger sample across more gateways would be needed to tell.
- Once Kelvran has any real external users or design partners, should demand-signal-gathering for MCP/A2A gateway-layer brokering be a standing, proactive question asked of them, rather than something inferred only from the wider ecosystem's own GitHub issue trackers?
