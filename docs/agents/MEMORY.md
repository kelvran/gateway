# Memory

Curated, durable facts and gotchas any AI agent should know without re-deriving them. **Hard cap: 200 lines.** When this file approaches the cap, rotate the oldest/least-load-bearing entries into `docs/agents/archive/archive-YYYY-MM.md` and keep this file lean — a memory file nobody can scan defeats its own purpose (a real documented failure elsewhere: a 501-line memory file got silently truncated by tooling for days before anyone noticed).

**One-writer rule**: only the active session appends in real time. Rotation is a separate, deliberate act (reorganizing, never originating new content).

This is **not** the place for: session-by-session history (→ `docs/agents/LOGS.md`), settled architectural decisions with full rationale (→ `docs/decisions/`, pointer only from here), or anything already enforced by CI/lint (delete it from here if it becomes redundant with a build check).

Every entry is date-stamped. Never edit a past entry's meaning — if something changes, add a new entry that supersedes it and say so explicitly.

---

- `[2026-09-02]` This project is pre-scaffolding as of this date — no Go/Python source code exists yet, only the documentation set. If you're picking up work and expect to find source files, check `docs/agents/LOGS.md` for when scaffolding actually started; don't assume it has.
- `[2026-09-02]` The project name is **Kelvran**. Two independent naming research rounds ran before this was locked in — do not re-run naming research; see `DECISIONS.md` and `ai-infra-research/naming-and-docs-plan.md` (parent workspace) if the rationale is ever needed again.
- `[2026-09-02]` Cache is never a standalone service — it's an internal Go package inside `gateway`, reached only through the `cache.Cache` interface. If a task seems to call for a new Cache microservice, stop and re-read `docs/decisions/0002-cache-embedded-in-gateway.md`'s extraction triggers first; none of the usual "it'd be cleaner" reasons qualify.
- `[2026-09-02]` Semantic caching (L3) must never ship with only a similarity threshold — the entity/date hard-gate and freshness/risk model are non-negotiable requirements from `PRD.md`, driven by real 2026 attack research (CacheAttack, KeyPooling) cited in `THREAT_MODEL.md`. Any PR touching `gateway/internal/cache/internal/` that weakens this should be treated as a security regression, not a style question.
- `[2026-09-02]` `evals` and `gateway` communicate ONLY through the versioned `api/` protobuf contract. Never add a direct source-code dependency in either direction, and never let them share a database.
- `[2026-09-02]` Pointers, not duplicates: full architectural rationale → `DESIGN.md` + `docs/decisions/`; session history → `docs/agents/LOGS.md`; current system state → `ARCHITECTURE.md`.
- `[2026-09-04]` Supersedes the `[2026-09-02]` "this project is pre-scaffolding" entry above: real source code has existed since 2026-09-02's later scaffolding pass, and `gateway/v0.1.0`/`evals/v0.1.0` are tagged, released, and live as of 2026-09-03. Always check `STATUS.md` for the current snapshot rather than assuming pre-scaffolding — this exact stale-status framing was independently found repeated verbatim across `README.md`, `docs/users/USER_GUIDE.md`, `RELEASE_NOTES.md`, `UPGRADE.md`, `DEPRECATED.md`, `CONTRIBUTING.md`, and `docs/operations/DEPLOY.md`, all corrected in this pass.
