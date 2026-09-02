# Plan Template

Copy this file to `docs/plans/YYYY-MM-DD-<title>.md` when it's time to implement something that already has a settled design — this is the "how, task by task" layer, one step past `docs/rfcs/` ("should we, and roughly how"). No `-design`/`-implementation` filename suffix; one file per plan.

> **For agentic executors:** work through this task-by-task, checking off each step as it's done. Don't skip ahead — a later task may depend on an earlier one's actual output, not just its description.

---

**Goal:** One or two sentences — what does this plan actually build.

**Architecture:** The shape of the solution, in a few sentences — which existing components it touches, what's new.

**Tech Stack:** Concrete tools/libraries this plan uses, if narrower than what `AGENTS.md`/`*/ARCHITECTURE.md` already establish.

**Spec:** Pointer to the `docs/rfcs/NNNN-*.md` this plan implements — or, for work too small to have warranted a full RFC, the `DECISIONS.md` line that authorized it. Every plan traces back to one of these; a plan with no spec pointer is scope creep. If a plan conflicts with something in the linked spec, resolve against the spec, not by guessing.

**Global Constraints:** Anything the linked spec establishes that every task below must respect (e.g. "must not add a new language," "must preserve the existing `cache.Cache` interface signature"). Inherited verbatim — don't re-argue them here.

---

## Phase 1: `<name>`

### Task 1: `<name>`

**Files:**
- Create: `path/to/new_file.go`
- Modify: `path/to/existing_file.go`
- Test: `path/to/new_file_test.go`

**Interfaces:**
- Consumes: `<what this task reads/depends on>`
- Produces: `<what this task exposes for later tasks>`

**Steps:**
- [ ] Step 1 — concrete, with real code where it clarifies intent, not a placeholder comment.
- [ ] Step 2 — ...

---

*(Repeat Phase/Task/Step blocks as needed. A plan for a genuinely small change may be a single Phase with one Task — don't pad it.)*

## Scope Gate

Plans are written only for architecturally-scoped work (the kind that would otherwise need a `docs/rfcs/` entry). A bounded change — a bug fix, a small refactor, an obvious addition — gets a one-line `DECISIONS.md` entry when it ships, not a plan file. If you're not sure which this is, it's probably the smaller one.
