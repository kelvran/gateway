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
│   └── README.md               dev-script + Makefile-target index; the targets it lists are real, not stubs (see its own header)
├── docs/                      user documentation set (rebuilt 2026-10-08) + the engineering record
│   ├── README.md               landing page: routes a reader to the tutorials/how-to/reference/explanation page for their job
│   ├── llms.txt                machine-readable index of the same set, for coding agents and LLM clients
│   ├── VERSIONING.md           what a version number promises, support window, surface retirement (policy in force from 2026-10-08)
│   ├── tutorials/              quickstart, first-virtual-key-and-budget, first-eval-suite
│   ├── how-to/                 one page per operator task: provider-credentials, virtual-keys-and-budgets, routing-and-failover,
│   │                            caching, streaming, structured-output, prompt-management, admin-api-rbac, backup-and-restore,
│   │                            rotate-credentials, upgrade, troubleshooting, configure-from-a-coding-agent
│   │   ├── clients/             openai-python, openai-node, curl — pointing a client you already use at the gateway
│   │   └── deploy/              docker-compose, kubernetes-kustomize, ecs-fargate, systemd-package
│   ├── reference/              data-plane-api, admin-api, config, metrics-and-logs, error-codes, release-artifacts, compatibility,
│   │                            glossary, container-image (the one reference page committed before this rebuild, 7686a0c8)
│   ├── explanation/            architecture, security-model, versioning, why-no-sdk, cache-gate, design-decisions, mcp-a2a-status
│   ├── decisions/              ADRs (MADR format) for the foundational, hard-to-reverse calls
│   ├── rfcs/                   dated design proposals (YYYY-MM-DD-[<component>-]<title>.md, 2026-09-02 onward; the component segment consistently from 2026-09-05), never rewritten —
│   │                            history, not current behaviour (rfcs/README.md); TEMPLATE.md for new ones
│   ├── research/                running, checkbox-driven pre-RFC question list — feeds rfcs/
│   ├── plans/                   task-by-task execution plans from 2026-09-02..04, each naming the RFC it implements — what was planned,
│   │                            not what shipped (plans/README.md); TEMPLATE.md kept
│   ├── upgrade-research/       dated research reports (<topic>-YYYY-MM-DD.md, since 2026-09-06; one synthesis is date-first) nearly all ending in point-in-time
│   │                            verdicts (build_now / not_yet / never / deferred, variously spelled) — proposals and evidence, not descriptions of the system;
│   │                            excluded from scripts/check-doc-paths.sh because the reports cite paths they only proposed
│   ├── agents/                 AI-agent-facing memory/log files (not application logs)
│   │   ├── README.md            what these files are for; why LOGS.md is excluded from the docs-path CI check
│   │   ├── MEMORY.md            curated, ≤200-line, durable facts/gotchas
│   │   ├── LOGS.md              append-only chronological session log
│   │   ├── ETHOS.md             decision-priority framework + core operating principles
│   │   ├── AGENTS_LEARNING.md   mistake→root-cause→prevention-rule taxonomy + anti-patterns (§5)
│   │   └── archive/             rotation target once MEMORY.md overflows
│   ├── testing/                  full test-pyramid strategy (unit/integration/contract/e2e/load/chaos/fuzz)
│   ├── operations/
│   │   ├── DEPLOY.md              deployment models, config reference, compat matrix
│   │   ├── TELEMETRY.md           SLIs/SLOs, dashboards, alerting, privacy defaults
│   │   ├── PROVIDERS.md           provider/data-flow inventory (moved here from docs/ root)
│   │   ├── FAILURE-MODES.md       per-dependency table: what a failure does to requests, how the operator sees it (2026-10-08)
│   │   ├── BENCHMARKS.md          benchmark methodology + harness; no published numbers yet, only regression trends (2026-10-08)
│   │   ├── DATA-SUBJECT-REQUESTS.md  operator procedure for data-subject (erasure/access) requests (2026-09-18)
│   │   ├── PROCUREMENT.md         what an enterprise procurement review asks, answered for Kelvran (2026-09-23)
│   │   ├── benchmarks/            dated raw benchmark runs (e.g. 2026-10-08-dev-laptop/)
│   │   └── grafana/, *.json, vector-*.yaml   Grafana/Prometheus/Alertmanager provisioning, dashboard JSON, Vector sinks
│   │                              shipping gatewayevents to S3/GCS
│   ├── development/
│   │   └── BRANCHES.md            branch strategy, tag-cutting mechanics
│   │                              (a symbol-level code map, `CODE_MAP.md` in this directory, is still not created;
│   │                               gateway/evals have had real package boundaries since 2026-09 — today the package-level
│   │                               maps are gateway/ARCHITECTURE.md § Package Layout and evals/ARCHITECTURE.md § Package Layout)
│   └── users/
│       └── USER_GUIDE.md          formerly the operator how-to-run-and-configure guide; since 2026-10-08 an eleven-heading stub
│                                   whose headings each point at the tutorials/ how-to/ reference/ explanation/ pages that own the topic
│                                   (several also cite operations/ or a root file) — start at docs/README.md instead
├── STATUS.md                   live, continuously-updated project-status dashboard
├── SUPPORT.md                   where to get help; routes vulnerabilities to SECURITY.md
├── RELEASE_NOTES.md             user-facing narrative release notes, spans both deployables
├── Makefile                     real targets (.PHONY list, Makefile:1): help setup config-safe lint[-gateway|-evals|-proto]
│                                test[-gateway|-evals] verify gen-proto check-proto bench bench-ci (the last two on main since 2026-10-08, not in gateway/v0.17.0)
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
├── UPGRADE.md                    breaking-change/migration guide; first row landed (evals/v0.2.0 `--scores`), policy in docs/VERSIONING.md
├── DEPRECATED.md                 deprecation list (stub until the first one)
├── LICENSE
└── README.md                    pitch, comparison table, links to everything above
```

## Why not 3 repos, and why not a single deployable

Go and Python cannot share a runtime, so a single deployable is impossible outright regardless of preference. Three fully separate repos was considered and rejected — every comparable product surveyed (LiteLLM, Portkey, TensorZero, Bifrost, Kong AI Gateway, Helicone) keeps Gateway and Cache fused in one process, and splitting the shared `api/` contract across repos before there's a distinct team to own each side is exactly the failure mode (Portkey's ~2-year OSS/enterprise-fork drift) this layout is designed to avoid. Full reasoning: `DESIGN.md` §"Decision 1", `docs/decisions/0001-monorepo-two-deployables.md`.

## `docs/agents/` is not `docs/` for humans

Everything under `docs/agents/` is specifically for AI coding agents resuming work across sessions (durable memory + chronological log), and is deliberately kept out of the repo root so it's never mistaken for user-facing documentation. See `AGENTS.md`/`CLAUDE.md`/`docs/agents/MEMORY.md`/`docs/agents/LOGS.md`'s boundary table (in the parent workspace's `ai-infra-research/naming-and-docs-plan.md` §3) for how these four files divide the work without overlapping.
