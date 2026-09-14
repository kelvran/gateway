# Admin Dashboard UI — Research (2026-09-14)

> **Recovery note:** the synthesizing subagent's own file-write step failed silently (a known recurring class); recovered by reading the workflow's own returned JSON result and reconstructing this report by hand.

**Scope:** Whether Kelvran needs a real operator-facing admin dashboard UI, and if so what shape — Kelvran has zero web UI today, only an HTTP admin API + Grafana infra metrics.

**Already shipped (not re-litigated):** a real HTTP admin API (config read, virtual-key CRUD, prompt CRUD+versioning) with a two-tier admin/viewer credential model and audit logging on every route; a real Grafana dashboard for infra-level OTel metrics; zero existing frontend code anywhere in this repo.

## Findings

1. **LiteLLM's real admin UI genuinely exceeds Kelvran's current posture — this is a verified gap, not overclaiming.** A real, actively-maintained Next.js/React SPA covers virtual-key create/edit/delete with SSO, per-key and per-model budget windows with live spend-to-date, and role-scoped usage views (Personal/Team/Organization/Global) — exactly the "friendlier than raw JSON" key-management and budget-burn UX Kelvran's admin API currently lacks.

2. **But even LiteLLM's own UI explicitly doesn't replace Grafana-style observability — validating, not undermining, Kelvran's existing API+Grafana split.** LiteLLM's docs state its Admin UI does not host custom dashboards, redirecting users to the management API or Prometheus/OTel for detailed per-user/per-team breakdowns. A business/admin UI and an infra-metrics dashboard are complementary, non-substitutable surfaces even for a vendor with a full-featured UI.

3. **Portkey is investing further in admin-UI-driven governance, not standing still.** It deprecated its original Virtual Keys feature, migrating it into a broader "Model Catalog" system advertising org-level credential management, per-model allow-lists, and fine-grained budgets/rate-limits exposed through the product UI — a second real competitor moving past simple key CRUD.

4. **Grafana's own plugin ecosystem could partially close the gap without a bespoke frontend, but the CRUD half is unproven.** The Business Forms plugin supports real CRUD (GET/POST/PUT/PATCH/DELETE) against arbitrary REST endpoints from inside a dashboard; the separate Business Table panel is positioned for business analytics, not infra observability — but Table's cells aren't verified to support write-back. A Grafana-native virtual-key UI would need Forms specifically, not Table, for the CRUD half. *(Medium confidence.)*

5. **If a bespoke UI is ever warranted, the lowest-effort, boundary-consistent 2026 stack is server-rendered Go (`html/template` or `templ`) + htmx — zero new toolchain, respecting `docs/decisions/0003-go-python-split.md`'s explicit rejection of adding a third language/runtime preemptively.** `templ` compiles Go-like HTML syntax into pure Go code with no virtual-DOM/reconciliation overhead; working reference stacks (Fiber + HTMX + Templ + GORM) exist as concrete 2026 tutorials for exactly this app class. *(Medium confidence — no verified evidence this stack is objectively superior to a small SPA for admin dashboards specifically, only that it's a legitimate, low-effort, boundary-respecting option.)*

6. **A third, less-conventional pattern exists but doesn't fit Kelvran's shape.** Cartapel (Rust-based) introspects a Postgres/MySQL/MariaDB schema directly and auto-generates a full CRUD panel, bypassing a REST API layer entirely — the opposite of what Kelvran would need (Kelvran's admin API already exists and is the thing to build a UI *on top of*, not replace). *(Low confidence, likely not applicable.)*

## Bottom line

**Closer to "not yet, with a documented low-effort fallback ready when the trigger fires" than to "build now."** None of the verified evidence establishes a Kelvran-specific urgency trigger (e.g., a second tenant/operator) — that judgment falls outside what competitive-landscape research can evidence; it needs a real roadmap decision. The honest recommendation: **`not_yet`** for the UI itself, **`build_now`** only as a documented decision (if/when built, use server-rendered Go+htmx, not a new SPA toolchain).

## Open Questions

- What does `gateway/internal/admin/admin.go`'s real route set look like today — does it already have everything an htmx-based UI would need (a paginated virtual-key list endpoint, etc.), or would new endpoints be needed first? *(Not independently re-verified in this pass — the synthesis operated on already-adjudicated claims, not a fresh code read.)*
- Is there an actual near-term second tenant/operator on Kelvran's roadmap — the one concrete go/no-go signal this research keeps circling back to — or is the pilot still single-operator with no onboarding plan?
- Would Kelvran's two-tier admin/viewer credential model and mandatory audit-logging-on-every-write even be satisfiable through a generic REST-CRUD panel (Grafana Forms), or does that requirement push firmly toward a bespoke UI regardless of effort level?
- Has anyone actually measured the operator time-cost of the current curl/raw-JSON workflow (minutes per new key, hand-typed-JSON error rate)? Without that baseline, "the API/curl workflow is painful at scale" remains an assumption, not a measured pain point.

## Caveats

This synthesis operates on already-adjudicated claims from the underlying research pass, not a fresh independent re-read of `gateway/internal/admin/admin.go` or `docs/operations/grafana/` — RQ3's mapping to Kelvran's real route set is inferred from the task's own stated background, not re-verified against source in this synthesis pass. RQ4 (is this premature for Kelvran's real scale) has no surviving claims that speak to Kelvran directly — none of the underlying claims are about Kelvran itself; this requires a judgment call from whoever owns the roadmap. Two claims (per-model budget spend-to-date; Business Table's positioning) landed at medium (2-1) rather than unanimous votes — treat as slightly softer.

## Synthesis: build_now vs not_yet

| Item | Verdict | Why |
|---|---|---|
| Bespoke admin UI itself | **not_yet** | No verified Kelvran-specific urgency trigger found |
| Decision to use server-rendered Go+htmx *if* ever built | **build_now** | Documented, boundary-respecting default, no new toolchain |
| Grafana Business Forms as a partial CRUD stopgap | **not_yet** | CRUD write-back unproven, and Kelvran's audit-logging requirement may not be satisfiable through a generic panel anyway |
| Cartapel-style schema-introspection tool | **not applicable** | Bypasses the REST API layer Kelvran already has and wants to build on |
