# AGENTS_LEARNING.md — Kelvran Mistake & Pattern Taxonomy

This document captures recurring mistakes, their root causes, and the prevention rules that came out of them — a mistake→fix→rule loop, distinct from `docs/agents/LOGS.md` (raw chronological session history), `docs/agents/MEMORY.md` (curated durable facts), and `DECISIONS.md`/`docs/decisions/` (architectural decisions and their rationale). If something belongs in one of those three, it belongs there and not here — this file is specifically for behavior corrections that recur or are likely to recur.

Seeded empty, and still empty as of 2026-09-04 despite real code and real bugs having shipped since — this file is specifically for *recurring* mistakes (see the header above), and no single mistake found and fixed so far (several are logged individually in `DECISIONS.md`/`docs/agents/LOGS.md`) has repeated. No entries are fabricated to fill space.

---

## 1. Mistakes Observed

| Date | What happened | Ref |
|---|---|---|
| — | none yet | — |

## 2. Root Cause Analysis

None yet — populated per recurring category once a mistake repeats, not per single incident.

## 3. Mitigation & Fix Strategy

None yet.

## 4. Best Practices (Must Follow)

None yet — will accumulate from real experience. Do not front-load generic advice here; see `AGENTS.md` for the conventions already settled before any code existed.

## 5. Anti-Patterns (Must Avoid)

None yet. **This is Kelvran's only anti-pattern list** — do not create a separate `ANTI-PATTERNS.md`; an entry here that becomes enforced by CI/lint should be removed from this list per `AGENTS.md`'s own falsifiability rule, the same discipline that already governs `AGENTS.md`'s Boundaries section.

## 6. Architectural Patterns to Follow

Pointer-heavy, not a restatement: see `DESIGN.md` for the whole-system rationale and `docs/decisions/` for the three foundational ADRs. New architectural patterns land here only once they've actually been used more than once — a single instance is a design choice, not yet a pattern.

## 7. Failure Scenarios (Failure-First Thinking)

None logged yet from real operation. `THREAT_MODEL.md` already covers the failure scenarios anticipated *before* any code existed (STRIDE per component) — this section is for failure modes actually observed once the system runs, which is a different, evidence-based list.

## 8. Continuous Learning Rules

- A `docs/agents/LOGS.md` entry that describes a mistake gets promoted into this file's Evolution Log below — the log stays raw history, this file extracts the lesson.
- An Evolution Log entry whose Category is "Anti-Pattern" and recurs 3+ times gets promoted into §5 above as a standing rule, and cross-referenced from `AGENTS.md`'s own Gotchas section.
- An Evolution Log entry that turns out to be architecturally significant (changes a foundational, hard-to-reverse decision) gets promoted into a full ADR under `docs/decisions/`, not left here.

---

## Evolution Log

Append-only. Newest entry at the bottom. Never edit a past entry — if something in it turns out to be wrong, add a new entry that supersedes it and says so.

**Entry format:**
```
### [Learning Entry - YYYY-MM-DDTHH:MM:SSZ]

**Context:** <what task was being performed>
**Mistake:** <what went wrong>
**Root Cause:** <why it happened>
**Fix:** <what was done>
**Prevention Rule:** <rule to avoid this in future>
**Category:** Best Practice / Anti-Pattern / Bug Fix / Architecture
```

*(No entries yet.)*
