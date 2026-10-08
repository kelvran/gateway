# RFCs — engineering record, not user documentation

This directory holds dated design proposals, `YYYY-MM-DD-<component>-<title>.md`, written before or while a change was built. They explain what was intended and why at that moment. They are never deleted or rewritten: a superseded or rejected RFC stays here with its Status changed.

What an RFC does **not** tell you is what the gateway does today. For that, read the code, `gateway/changelog/` and `evals/changelog/` (what shipped in which version), and the user documentation at [`docs/README.md`](../README.md). Where an RFC and the code disagree, the code is right and the RFC is history. Some RFCs describe designs that were never built (the two MCP design RFCs mark themselves design-only; others are decided in [`DECISIONS.md`](../../DECISIONS.md) or in the newest report under [`docs/upgrade-research/`](../upgrade-research/README.md)).

Only some RFCs carry the template's `- **Status**:` line; the absence of one means nothing about whether the work shipped. Check the changelog.

Writing a new one: copy [`TEMPLATE.md`](TEMPLATE.md).
