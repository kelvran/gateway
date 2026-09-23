# Procurement

Added 2026-09-23, per `docs/upgrade-research/enterprise-procurement-buyer-criteria-2026-09-22.md`'s
Findings 1, 2, and 3. This document exists for one narrow purpose: three questions a real 2026
enterprise procurement or security-review process reflexively asks any AI infrastructure vendor,
answered here explicitly rather than left silent for a reviewer to flag as "unknown/unaddressed."
It does not restate `SUPPORT.md`'s support-model disclosures or `SECURITY.md`'s "Compliance
Evidence for Operators" section — read those first; this document assumes both and does not
contradict either.

## Adopting Kelvran without a vendor contract

`SUPPORT.md` already states this plainly: "Commercial/Enterprise Support: Not offered... best-effort,
no SLA, at the current solo-maintainer stage." There is no legal entity behind Kelvran to sign an
MSA, DPA, BAA, or SLA with — which means the standard enterprise third-party-risk-management (TPRM)
workflow, built around sending a vendor-risk questionnaire to a company and contracting against its
answers, does not have a vendor to route to.

That does not mean there is no viable adoption path — only that it is a different one. Two
independently-published analyses of open-source procurement (Kusari's "Third-Party Risk in Open
Source" and The New Stack's "Open Source Risk in the Procurement Process") both describe the same
real, converged pattern: an organization evaluating OSS with no vendor relationship behind it
routes the decision through an **internal security review** instead — the adopting organization's
own security team audits the code, architecture, and published security posture directly, rather
than contracting against a vendor's own attestations. For a solo-maintainer project at Kelvran's
current stage, that internal-review path is the real, viable route, and this document names it so a
procurement or security team doesn't have to independently rediscover that Kelvran simply doesn't
fit the default vendor-risk workflow.

The artifacts an internal reviewer would use for that review already exist and are listed in
`SECURITY.md`'s "Compliance Evidence for Operators" section — `THREAT_MODEL.md`, this repository's
disclosure process and severity taxonomy, `SECURITY-INSIGHTS.yml`, the signed/SBOM'd/provenance'd
container image, and `docs/operations/PROVIDERS.md`. None of those are a substitute for a vendor's
own SOC 2 report; they are what an internal-review-track reviewer actually reads instead.

**What this section is not**: it is not a recommendation that Kelvran incorporate as a company, seek
a fiscal sponsor, or begin selling support contracts to close this gap. Whether Kelvran ever gets a
legal entity behind it is a business and governance decision several steps beyond what a docs-only
change can or should make. This section only makes the internal-review alternative visible; it does
not resolve the underlying absence of a vendor entity.

## Why Kelvran doesn't need a BAA

A HIPAA Business Associate Agreement (BAA) is the contract HHS requires between a covered entity and
any vendor that creates, receives, maintains, or transmits protected health information (PHI) *on
the covered entity's behalf*. HHS's own guidance draws that line at **persistent** access to PHI —
storing or processing it — not merely transient access; a pure conduit that never has an opportunity
to access the data in any meaningful way is not a business associate, even though data may pass
through it.

That leaves two architectural shapes a system can have with respect to a customer's PHI:

1. **Vendor-hosted/managed**: the vendor's own infrastructure stores or processes the customer's
   data. A business-associate relationship exists, and a BAA is required.
2. **Self-hosted, entirely inside the customer's own environment**: the software runs on
   infrastructure the customer alone controls; the software's publisher never receives, stores, or
   has access to the customer's data on any system it operates. No business-associate relationship
   exists, because there is no separate party with access to govern.

Kelvran is the second shape. Per `PRD.md`'s own non-goal, self-hosting is the only deployment model
— there is no Kelvran-operated service, and no infrastructure this project controls ever receives or
persists an operator's data. Kelvran's cache and logs do persist data, but only on the *operator's
own* infrastructure, under the operator's own control, never on a system the Kelvran project runs.
Two real, self-hosted products with the same shape — Philterd (self-hosted PII redaction) and
Twingate (self-hosted zero-trust networking) — both publish this identical reasoning for declining
to sign a BAA on their own software's behalf, for the same underlying reason.

**The one real caveat**: this reasoning covers Kelvran-the-software only. It says nothing about an
operator's own, separate BAA obligations with each upstream LLM provider Kelvran is configured to
route to. Providers such as OpenAI, Anthropic, or AWS Bedrock may require — and in some cases offer
directly — their own BAA with the covered entity operating Kelvran. That relationship is between the
operator and the provider; Kelvran is not a party to it, and using Kelvran does not create, satisfy,
or remove the need for it. Confirming and arranging those provider-level BAAs remains the operator's
own responsibility. As elsewhere in this repository's compliance documentation: this is a disclosed
architectural analysis, not legal advice — confirm it applies to your own deployment and
circumstances before relying on it.

## FedRAMP applicability

fedramp.gov's own scope rules define FedRAMP as governing cloud computing products or services that
a cloud service provider offers *to* a federal agency under a shared-responsibility model — the
provider operates a shared service, and FedRAMP authorizes that service for government use.
fedramp.gov explicitly excludes the opposite case: information systems used only for a single
agency's own operations, hosted on cloud infrastructure the agency itself configures, operates, and
supports, with no shared-responsibility model and no shared service being offered.

Kelvran is that excluded case. It is self-hosted, open-source software: whichever organization
deploys it — including a federal agency, if one chooses to — configures, operates, and supports it
entirely within its own environment. There is no Kelvran-operated service, control plane, or shared
infrastructure anywhere in the picture for FedRAMP to authorize.

FedRAMP therefore does not apply to Kelvran. This is not "not yet achieved" — it is stated
affirmatively, per fedramp.gov's own scope rule, as structurally out of scope, so that a federal
buyer's checklist item finds an explicit answer here rather than an unaddressed silence.
