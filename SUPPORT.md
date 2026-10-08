# Support

## Getting Help

No dedicated community channel exists yet (no Discord/Slack/discussion forum has been set up). Start with the documentation landing page, [`docs/README.md`](docs/README.md), and for a gateway that is failing right now, [`docs/how-to/troubleshooting.md`](docs/how-to/troubleshooting.md) — **corrected 2026-10-08**: this line pointed at `docs/users/USER_GUIDE.md`, which became an eleven-heading stub of pointers into that set on 2026-10-08. If your question isn't answered there, open a GitHub issue.

## Supported Versions

The latest minor of each deployable gets all fixes; the previous minor gets security fixes for 90 days. Policy, current versions and dates: [`docs/VERSIONING.md`](docs/VERSIONING.md).

## Filing a Bug or Feature Request

Open a GitHub issue including: affected component (Gateway/Cache/Evals — same vocabulary as the "Please include" line of `SECURITY.md` § Reporting a Vulnerability; **corrected 2026-10-08**: until this date this line cited `SECURITY.md`'s severity taxonomy instead, whose table lists P0–P4 severities, not components, and MCP-A2A was removed from both lists because that subsystem has no shipped code — `gateway/internal/` has no `mcp` or `a2a` package — see [`docs/explanation/mcp-a2a-status.md`](docs/explanation/mcp-a2a-status.md)), affected version, and a minimal reproduction if it's a bug.

## Reporting a Security Vulnerability

**Do not file a public issue for a security vulnerability.** See `SECURITY.md` for the private reporting channel — this file intentionally doesn't restate that process, to avoid the two drifting out of sync.

## Response Time Expectations

Best-effort, no SLA, at the current solo-maintainer stage. `SECURITY.md`'s acknowledgement/resolution targets are the one place with numeric targets — **corrected 2026-09-20** (found stale by an end-to-end research round, `docs/upgrade-research/ga-readiness-tier1-2026-09-20.md`): those targets have been real and contractual since `gateway/v0.1.0`/`evals/v0.1.0` (tagged 2026-09-03; see `SECURITY.md`'s own `2026-09-05` correction), not aspirational — this line describing them as still-aspirational was itself never updated when that changed. This document still points there rather than inventing separate numbers.

## Commercial/Enterprise Support

Not offered. Stated as a current fact, not a permanent policy — if this changes, this section updates.
