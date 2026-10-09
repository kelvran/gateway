# Kelvran documentation

Kelvran is a self-hosted, OpenAI-compatible LLM gateway written in Go: one `POST /v1/chat/completions`, `POST /v1/embeddings` and `GET /v1/models` surface in front of Amazon Bedrock, Anthropic, OpenAI, Google Gemini and any OpenAI-compatible endpoint, with virtual keys and budgets, weighted routing and classified failover, a three-layer response cache whose lexical tier is gated by entity and freshness checks, guardrails, an admin API on a separate listener, and OpenTelemetry on every request. The repository's [README](../README.md) holds the feature table and the quickstart; this page routes you to the page that does your job.

## Choose how to start

| You want to… | Go to |
|---|---|
| Try it in ten minutes: one deployment, one virtual key, one request | [Quickstart](tutorials/quickstart.md) |
| Point a client you already use at the gateway | [OpenAI Python SDK](how-to/clients/openai-python.md) · [OpenAI Node SDK](how-to/clients/openai-node.md) · [LangChain](how-to/clients/langchain.md) · [LlamaIndex](how-to/clients/llamaindex.md) · [Vercel AI SDK](how-to/clients/vercel-ai-sdk.md) · [curl](how-to/clients/curl.md) · what of the OpenAI surface is honoured: [Compatibility](reference/compatibility.md) |
| Use it from Cursor, Continue or Claude Code | Those clients speak the OpenAI shape; point them at `https://<gateway>/v1` with a virtual key as the API key (see [Compatibility](reference/compatibility.md)). Dedicated pages follow; Claude Code's native Anthropic-Messages mode is not served today ([MCP/A2A and Anthropic-Messages status](explanation/mcp-a2a-status.md)). |
| Let a coding agent configure Kelvran for you | [Configure Kelvran from a coding agent](how-to/configure-from-a-coding-agent.md) and the machine-readable index [`llms.txt`](llms.txt) |
| Give a key a budget and see it enforced | [First virtual key and budget](tutorials/first-virtual-key-and-budget.md) |
| Score an agent's output | [First eval suite](tutorials/first-eval-suite.md) |

## Configure and operate

- **Deploy**: [Docker Compose](how-to/deploy/docker-compose.md) · [Kubernetes with Kustomize](how-to/deploy/kubernetes-kustomize.md) · [ECS Fargate](how-to/deploy/ecs-fargate.md) · [deb/rpm/apk with systemd](how-to/deploy/systemd-package.md) · the container image: [reference](reference/container-image.md)
- **Credentials**: [Provider credentials](how-to/provider-credentials.md) · [Rotate credentials](how-to/rotate-credentials.md) · data flows and residency per provider: [`operations/PROVIDERS.md`](operations/PROVIDERS.md)
- **Keys and budgets**: [Virtual keys and budgets](how-to/virtual-keys-and-budgets.md) · [Admin API and role tiers](how-to/admin-api-rbac.md)
- **Routing**: [Routing and failover](how-to/routing-and-failover.md)
- **Cache**: [Caching](how-to/caching.md)
- **Requests**: [Streaming](how-to/streaming.md) · [Structured output](how-to/structured-output.md) · [Prompt management](how-to/prompt-management.md)
- **Observability**: [Metrics and logs](reference/metrics-and-logs.md) · dashboards, SLOs and alerting: [`operations/TELEMETRY.md`](operations/TELEMETRY.md)
- **State**: [Backup and restore](how-to/backup-and-restore.md)
- **Upgrade**: [Upgrade](how-to/upgrade.md) · breaking changes: [`UPGRADE.md`](../UPGRADE.md)
- **When something breaks**: [Troubleshooting](how-to/troubleshooting.md) · what each dependency's failure does to requests: [`operations/FAILURE-MODES.md`](operations/FAILURE-MODES.md)
- **Performance**: method, scenarios and the numbers on record: [`operations/BENCHMARKS.md`](operations/BENCHMARKS.md)
- **Compliance**: [`operations/DATA-SUBJECT-REQUESTS.md`](operations/DATA-SUBJECT-REQUESTS.md) · [`operations/PROCUREMENT.md`](operations/PROCUREMENT.md)

## Reference

[Data-plane API](reference/data-plane-api.md) · [Admin API](reference/admin-api.md) · [Configuration](reference/config.md) · [Error codes](reference/error-codes.md) · [Metrics and logs](reference/metrics-and-logs.md) · [Compatibility](reference/compatibility.md) · [Release artifacts](reference/release-artifacts.md) · [Container image](reference/container-image.md) · [Glossary](reference/glossary.md)

The commented [`gateway/config.example.yaml`](../gateway/config.example.yaml) is the authoritative list of configuration keys; [Configuration](reference/config.md) is the same list as a table with defaults and validation rules.

## Understand

[Architecture](explanation/architecture.md) · [Security model](explanation/security-model.md) · [Versioning](explanation/versioning.md) · [Why there is no SDK](explanation/why-no-sdk.md) · [The cache gate](explanation/cache-gate.md) · [Design decisions](explanation/design-decisions.md) · [MCP/A2A status](explanation/mcp-a2a-status.md)

Comparisons, from each project's own documentation on 2026-10-09 and with no benchmark figures: [Kelvran vs LiteLLM](explanation/compare/litellm.md) · [Kelvran vs Portkey Gateway](explanation/compare/portkey.md) · [Kelvran vs Bifrost](explanation/compare/bifrost.md)

Policies: [`VERSIONING.md`](VERSIONING.md) (what a version number promises) · [`SECURITY.md`](../SECURITY.md) (reporting a vulnerability, privately) · [`SUPPORT.md`](../SUPPORT.md) · [`CONTRIBUTING.md`](../CONTRIBUTING.md) · [`THREAT_MODEL.md`](../THREAT_MODEL.md).

## Project internals, not user documentation

These directories are the engineering record. They explain intent and history, not current behaviour; the code and the changelogs outrank them.

- [`rfcs/`](rfcs/README.md): dated design proposals, never rewritten.
- [`plans/`](plans/README.md): execution plans from the project's first days.
- [`upgrade-research/`](upgrade-research/README.md): dated research reports with point-in-time verdicts.
- [`decisions/`](decisions/0001-monorepo-two-deployables.md): architecture decision records; the running log is [`DECISIONS.md`](../DECISIONS.md).
- [`agents/`](agents/README.md): working memory for AI coding agents.
- [`research/RESEARCH.md`](research/RESEARCH.md), [`development/BRANCHES.md`](development/BRANCHES.md), [`testing/TESTING.md`](testing/TESTING.md): research notes, branch strategy, test strategy.

The former [`users/USER_GUIDE.md`](users/USER_GUIDE.md) is a stub whose headings point into the pages above.
