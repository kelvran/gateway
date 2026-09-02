# RFC Template

Copy this file to `docs/rfcs/YYYY-MM-DD-short-title.md` when proposing a major design change (a new subsystem, a breaking change to the shared Gateway↔Evals contract, or anything that would otherwise need to be re-argued from scratch every time it comes up). Smaller changes don't need an RFC — an issue with an `RFC`-sized label, or a one-line entry in `DECISIONS.md`, is enough.

Never delete an RFC file once created. Change its `Status` field instead — the history of what was proposed and why it was or wasn't accepted is itself valuable.

---

- **Status**: proposed | accepted | rejected | superseded-by-NNNN
- **Date**: YYYY-MM-DD
- **Author(s)**:

## Summary

One paragraph: what is being proposed, in plain language.

## Motivation

Why does this need to change? What's broken, missing, or blocking without it? Link to the production evidence, user report, or research finding that motivated this, if one exists.

## Detailed Design

The actual proposal. Specific enough that an engineer could start implementing from it: affected components (Gateway / Cache / Evals / the shared contract), new or changed interfaces, data model changes, migration path if this changes something already shipped.

## Drawbacks

What does this cost — complexity, performance, a new dependency, a breaking change, an ongoing maintenance burden? Be honest here; a drawbacks section with nothing in it usually means it wasn't looked at hard enough.

## Alternatives Considered

What else was considered, and why was this approach chosen over them? Include the option of doing nothing.

## Unresolved Questions

What does this RFC deliberately leave open — for the reviewer to weigh in on, or for a follow-up RFC to resolve later?
