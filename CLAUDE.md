# CLAUDE.md

@AGENTS.md

Do not duplicate `AGENTS.md` here. Everything a generic coding agent (Codex, Cursor, Aider) would also need to know lives there. This file holds only what's true specifically because the runtime is Claude Code.

## Claude-Code-runtime specifics

- **Slash commands / skills**: none configured yet — pre-scaffolding. Add here once any project-specific skill or slash command exists.
- **Subagents**: none configured yet. When they are, list each with its auto-trigger condition (e.g. "security-touching diffs in `gateway/internal/identity/` or `gateway/internal/cache/` → run a security-review subagent before merge").
- **MCP servers**: none configured for this repo yet.
- **Hooks**: none configured yet. A likely future candidate, given `THREAT_MODEL.md`'s known threat classes: a pre-commit hook that flags diffs touching `gateway/internal/cache/` or `gateway/internal/identity/` for mandatory security review, since those are exactly the two subsystems `THREAT_MODEL.md` flags as highest-priority (cross-tenant isolation, cache poisoning/hijacking).
- **`settings.json` interactions**: none yet.

If you're extending this file and the thing you're adding would also matter to a non-Claude coding agent, it belongs in `AGENTS.md` instead — move it there rather than growing this file.
