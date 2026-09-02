# Repo Layout

Literal map of this repository. For *why* it's shaped this way (one monorepo, two deployables, not three separate projects), see `DESIGN.md` and `docs/decisions/0001-monorepo-two-deployables.md`.

```
kelvran/
├── gateway/                  Go binary: Gateway + embedded Cache — see gateway/ARCHITECTURE.md
│   └── changelog/             one .md file per released version + unreleased.md
├── evals/                    Python service: agent-eval rollouts, judging, statistics — see evals/ARCHITECTURE.md
│   └── changelog/             one .md file per released version + unreleased.md
├── api/                       the ONLY cross-language contract surface — see api/README.md
│   ├── otel/                   OTel semantic-convention schema (proto)
│   └── gatewayevents/          cost/usage/decision event schema (proto)
├── scripts/
│   └── README.md               dev-script index: stubbed-now vs. deferred, and why
├── docs/
│   ├── decisions/              ADRs (MADR format) for the foundational, hard-to-reverse calls
│   ├── rfcs/                   template + future major design proposals (empty until one is written)
│   ├── research/                running, checkbox-driven pre-RFC question list — feeds rfcs/
│   ├── plans/                   superpowers-style task-by-task implementation-plan template
│   ├── agents/                 AI-agent-facing memory/log files (not application logs)
│   │   ├── MEMORY.md            curated, ≤200-line, durable facts/gotchas
│   │   ├── LOGS.md              append-only chronological session log
│   │   ├── ETHOS.md             decision-priority framework + core operating principles
│   │   ├── AGENTS_LEARNING.md   mistake→root-cause→prevention-rule taxonomy + anti-patterns (§5)
│   │   └── archive/             rotation target once MEMORY.md overflows
│   ├── testing/                  full test-pyramid strategy (unit/integration/contract/e2e/load/chaos/fuzz)
│   ├── operations/
│   │   ├── DEPLOY.md              deployment models, config reference, compat matrix
│   │   ├── TELEMETRY.md           SLIs/SLOs, dashboards, alerting, privacy defaults
│   │   └── PROVIDERS.md           provider/data-flow inventory (moved here from docs/ root)
│   ├── development/
│   │   └── BRANCHES.md            branch strategy, tag-cutting mechanics
│   │                              (a symbol-level docs/development/CODE_MAP.md belongs here once
│   │                               gateway/evals have real package boundaries — not created yet;
│   │                               this directory-only view is the pre-code substitute)
│   └── users/
│       └── USER_GUIDE.md          operator how-to-run-and-configure guide
├── STATUS.md                   live, continuously-updated project-status dashboard
├── SUPPORT.md                   where to get help; routes vulnerabilities to SECURITY.md
├── RELEASE_NOTES.md             user-facing narrative release notes, spans both deployables
├── Makefile                     placeholder entry points (help/setup/lint/test/verify)
├── PRD.md                     one-time, dated: what to build and why
├── DESIGN.md                   one-time, dated: whole-system design sketch + the 3 foundational decisions' rationale
├── ARCHITECTURE.md             root, thin index — current-state component map and request flow
├── DECISIONS.md                 continuous, terse decision log (the small stuff ADRs don't need)
├── THREAT_MODEL.md              STRIDE-per-component + OWASP LLM Top 10 crosswalk
├── SECURITY.md                  disclosure policy, severity taxonomy, known threat classes
├── SECURITY-INSIGHTS.yml        OpenSSF machine-readable security metadata
├── AGENTS.md                    tool-neutral instructions for any AI coding agent (Codex/Cursor/Aider/Claude)
├── CLAUDE.md                    thin shim: imports AGENTS.md, adds Claude-Code-runtime-only specifics
├── CONTRIBUTING.md              dev setup, PR conventions, design-review gate
├── CODE_OF_CONDUCT.md
├── CODEOWNERS
├── RELEASE.md                   release runbook, contract-version bump procedure
├── UPGRADE.md                    breaking-change/migration guide (stub until the first one)
├── DEPRECATED.md                 deprecation list (stub until the first one)
├── LICENSE
└── README.md                    pitch, comparison table, links to everything above
```

## Why not 3 repos, and why not a single deployable

Go and Python cannot share a runtime, so a single deployable is impossible outright regardless of preference. Three fully separate repos was considered and rejected — every comparable product surveyed (LiteLLM, Portkey, TensorZero, Bifrost, Kong AI Gateway, Helicone) keeps Gateway and Cache fused in one process, and splitting the shared `api/` contract across repos before there's a distinct team to own each side is exactly the failure mode (Portkey's ~2-year OSS/enterprise-fork drift) this layout is designed to avoid. Full reasoning: `DESIGN.md` §"Decision 1", `docs/decisions/0001-monorepo-two-deployables.md`.

## `docs/agents/` is not `docs/` for humans

Everything under `docs/agents/` is specifically for AI coding agents resuming work across sessions (durable memory + chronological log), and is deliberately kept out of the repo root so it's never mistaken for user-facing documentation. See `AGENTS.md`/`CLAUDE.md`/`docs/agents/MEMORY.md`/`docs/agents/LOGS.md`'s boundary table (in the parent workspace's `ai-infra-research/naming-and-docs-plan.md` §3) for how these four files divide the work without overlapping.
