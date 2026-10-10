# User Guide

**This guide was split into the documentation set at [`docs/README.md`](../README.md) on 2026-10-08.** The eleven headings below are kept so that existing references to "§3", "§4" and so on still land; each now holds one pointer to the page that owns the topic. Nothing here is a second copy of that content.

## 1. Before You Start

The ten-minute path (one deployment, one virtual key, one request) is [`tutorials/quickstart.md`](../tutorials/quickstart.md). The deployment topologies (Docker Compose, deb/rpm/apk with systemd, Kubernetes, ECS/Fargate) are under [`how-to/deploy/`](../how-to/deploy/docker-compose.md) and in [`operations/DEPLOY.md`](../operations/DEPLOY.md). The companion CLI — `kelvran init` writes the first config and key, `kelvran doctor` reports what `-validate` cannot see, `kelvran keys` creates, lists, rotates and deletes virtual keys, `kelvran status` and `kelvran spend` read a running gateway, `kelvran connect` points a coding tool at it (all on `main` since 2026-10-10, not in `gateway/v0.17.0`) — is [`reference/kelvran-cli.md`](../reference/kelvran-cli.md).

## 2. Provider Credentials

Giving a deployment its credential, by environment variable or by hot-reloaded file, is [`how-to/provider-credentials.md`](../how-to/provider-credentials.md); rotation is [`how-to/rotate-credentials.md`](../how-to/rotate-credentials.md). [`operations/PROVIDERS.md`](../operations/PROVIDERS.md) is the data-flow and residency inventory.

## 3. Virtual Keys and Budgets

The first key with a budget and a rate limit, step by step: [`tutorials/first-virtual-key-and-budget.md`](../tutorials/first-virtual-key-and-budget.md). Every key option, persistence and the live admin API: [`how-to/virtual-keys-and-budgets.md`](../how-to/virtual-keys-and-budgets.md) and [`reference/admin-api.md`](../reference/admin-api.md).

## 4. Calling Kelvran (Client Integration)

Point an existing client at the gateway: [`how-to/clients/openai-python.md`](../how-to/clients/openai-python.md), [`how-to/clients/openai-node.md`](../how-to/clients/openai-node.md), [`how-to/clients/langchain.md`](../how-to/clients/langchain.md), [`how-to/clients/llamaindex.md`](../how-to/clients/llamaindex.md), [`how-to/clients/vercel-ai-sdk.md`](../how-to/clients/vercel-ai-sdk.md), [`how-to/clients/curl.md`](../how-to/clients/curl.md). What of the OpenAI surface is honoured, ignored or extended: [`reference/compatibility.md`](../reference/compatibility.md). Why there is no first-party SDK: [`explanation/why-no-sdk.md`](../explanation/why-no-sdk.md).

## 5. Routing & Failover Configuration

[`how-to/routing-and-failover.md`](../how-to/routing-and-failover.md); every key with its default in [`reference/config.md`](../reference/config.md).

## 6. Cache Configuration

[`how-to/caching.md`](../how-to/caching.md); why the third layer is lexical and hard-gated, and never a bare similarity threshold: [`explanation/cache-gate.md`](../explanation/cache-gate.md).

## 7. MCP/A2A Tool Brokering

Designed, not shipped: [`explanation/mcp-a2a-status.md`](../explanation/mcp-a2a-status.md).

## 8. Observability

Spans, metrics and log events: [`reference/metrics-and-logs.md`](../reference/metrics-and-logs.md); dashboards, SLOs and alerting: [`operations/TELEMETRY.md`](../operations/TELEMETRY.md).

## 9. Running Your First Eval Suite

[`tutorials/first-eval-suite.md`](../tutorials/first-eval-suite.md).

## 10. Upgrading

[`how-to/upgrade.md`](../how-to/upgrade.md); the policy behind it is [`VERSIONING.md`](../VERSIONING.md) and the breaking-change table is [`UPGRADE.md`](../../UPGRADE.md).

## 11. Troubleshooting

[`how-to/troubleshooting.md`](../how-to/troubleshooting.md), derived from [`operations/FAILURE-MODES.md`](../operations/FAILURE-MODES.md). For anything not covered, [`SUPPORT.md`](../../SUPPORT.md).
