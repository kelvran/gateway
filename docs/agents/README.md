# docs/agents — working memory for AI coding agents, not user documentation

These files are for an AI coding agent resuming work on the repository: `ETHOS.md` (how agents are expected to behave here), `AGENTS_LEARNING.md` (patterns learned, including the repository's recurring doc-vs-code staleness), `MEMORY.md` (curated durable facts, hard-capped), and `LOGS.md` (an append-only, session-by-session record in a fixed entry format). `AGENTS.md` at the repository root is the entry point that tells an agent to read them.

If you are a person looking for history, `LOGS.md` is long and narrative; prefer [`DECISIONS.md`](../../DECISIONS.md) for why, and `gateway/changelog/` and `evals/changelog/` for what shipped when. `LOGS.md` is excluded from the CI check that cited `docs/…` paths resolve because it is never edited after the fact: a path that later moved stays as it was written.

For what the gateway does today, start at [`docs/README.md`](../README.md).
