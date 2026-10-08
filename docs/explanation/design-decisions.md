# Design decisions

This page explains why Kelvran is shaped the way it is, where each decision is recorded, and what was considered and turned down. It is for operators, contributors and reviewers who are about to question a foundational choice and want to find the existing reasoning before re-arguing it. It does not list configuration or API surface; for that, see the [configuration reference](../reference/config.md) and the [architecture explanation](./architecture.md).

## Why decisions are recorded in layers

Kelvran keeps four kinds of decision record, each with a different weight and a different editing rule. The split exists because one log cannot serve both a hard-to-reverse architectural call and a one-line tooling choice without one of them drowning the other.

- **ADRs** in `docs/decisions/` are for rare, foundational, hard-to-reverse calls. They use the MADR shape: context, drivers, options, outcome, consequences, revisit triggers. There are exactly three, all accepted on 2026-09-02 by the project founder.
- **[DECISIONS.md](../../DECISIONS.md)** is the continuous layer for everything smaller. One line per decision, in the format `[YYYY-MM-DD] decision — one-sentence why`. A line is appended after work is implemented, not during planning. Past lines are never edited; a change is a new dated line that supersedes the old one. If a small decision keeps recurring, the rule is to promote it to an ADR rather than let it grow in place.
- **RFCs, plans and research reports** under `docs/rfcs/`, `docs/plans/` and `docs/upgrade-research/` hold the design work behind a decision. An RFC file is never deleted once created; its `Status` field changes instead (see [the RFC template](../rfcs/TEMPLATE.md)). Most research reports tag each finding `build_now` or `not_yet` with a named trigger (the directory's README also admits `never` and `deferred`, rarely used); a sizeable minority reach their conclusions in prose without that vocabulary.
- **Dated notes inside [gateway/ARCHITECTURE.md](../../gateway/ARCHITECTURE.md)** record ideas that were researched and declined, so that "we never built X" can be distinguished from "we looked at X and chose not to".

Above all of these sits [DESIGN.md](../../DESIGN.md), a whole-system sketch dated 2026-09-02 and frozen. It captures the reasoning behind the three ADRs before any code existed. Wherever DESIGN.md and the current architecture documents disagree, the architecture documents win; DESIGN.md says so in its own header.

## The three architecture decision records

### One monorepo, two deployables

[ADR 0001](../decisions/0001-monorepo-two-deployables.md) settles how many repositories and how many deployables Kelvran has. The deployable count is not a choice: Go and Python cannot share a runtime, so the Go gateway (with its embedded cache) and the Python evals service must ship separately. The repository count is a choice, and the ADR picks one monorepo.

The reasoning is about drift. The one artifact that must change atomically across the language boundary is the contract in `api/`. The ADR cites Portkey's roughly two-year split of one gateway across two codebases, later publicly remerged, as the failure it is designed to avoid. It also records that every premature-split case study it surveyed (Segment, InVision, Istio's control plane, Prime Video's monitoring pipeline) was later reversed, and that every AI-infra peer it surveyed keeps gateway and cache in one process.

The options rejected were a single deployable (impossible), three independent projects, and two repositories with no shared root. The accepted cost is that `evals/` sits in the repository while it is still idle, waiting for production traffic to sample.

The revisit trigger is narrow: a distinct team, not a distinct engineer, owning a component with its own release cadence and on-call, and cross-repository `buf breaking` tooling mature enough to hold schema drift to today's in-repo standard. Team size alone is explicitly not a trigger.

### Cache is embedded in the gateway, not a standalone service

[ADR 0002](../decisions/0002-cache-embedded-in-gateway.md) answers whether the multi-layer response cache should be its own service. It is not. A standalone cache would need its own front door, tenant resolution and upstream router, which the ADR estimates as a 60 to 70 percent duplicate of the gateway. All seven peers it surveyed embed caching in-process.

The chosen option keeps a seam. The request pipeline depends on two interfaces in `gateway/internal/cache/` and on no concrete type: `Cache` in `port.go` for L1 and L2, and `LexicalCache` in `lexical.go` for L3; the in-process implementations of all three layers are wired in `gateway/cmd/gateway/main.go`. `Cache` today has three methods, `Get`, `Put` and `Delete`, each taking a tenant ID; `Delete` arrived on 2026-09-14 to service a single-entry erasure request, and the tenant ID arrived on 2026-09-20 to give L1 and L2 a per-tenant eviction boundary. The ADR's own text still says `Get`, `Put`; read the code for the current shape. The dormant `grpcclient` and `grpcserver` adapters implement only `Cache`, so the ADR's one-line extraction would today cover L1 and L2 but not L3.

The ADR carries a correction dated 2026-09-12. Its original text implied a cache `.proto` contract existed for the dormant gRPC adapters. None ever has. The only protobuf in the repository is `api/gatewayevents/v1/gatewayevents.proto`, which is unrelated to the cache. The dormant seam is typed Go only, and the ADR names the missing wire contract as a disclosed gap in its "extraction is a one-line change" framing.

Four evidence-based triggers would justify extraction: a dedicated owner measurably blocked by the gateway's release cadence; production telemetry showing the cache's resource profile diverging from the gateway's; a second caller that needs the cache without routing through this gateway; or a profiled CPU bottleneck, for which the ADR says to try an in-process Rust FFI island first. "It would be architecturally cleaner" is named as not a trigger. The product rule that no L3 change may weaken the entity and freshness hard-gate is explained in [the cache gate page](./cache-gate.md).

### Go for the gateway and cache, Python for evals

[ADR 0003](../decisions/0003-go-python-split.md) rejects a single language for uniformity's sake. The gateway is an I/O-bound streaming proxy, a workload where Go has a long record and where the ADR documents Python's overhead using LiteLLM's own figures and its later move of the hot path toward Rust. Evals depends on a statistics and LLM-judge ecosystem that exists in Python and has no real Go or Rust equivalent. All-Python, all-Go and all-Rust were each considered and declined.

The boundary between the two is a versioned protobuf contract in `api/`, generated into both languages and checked by `buf breaking` in CI. It is never shared code. Within each language, `gateway/.go-arch-lint.yml` and `evals/.importlinter` enforce the dependency direction. The accepted cost is two toolchains and two sets of lint configuration.

The ADR allows one refinement: a narrow, profiled Rust FFI island inside the cache for similarity or tokenization math, behind a stable interface, and never touching auth, routing, quota or spend logic. The ADR grounds that hard rule in a community Rust port of LiteLLM's auth and rate-limiting that surfaced eleven open security findings, including auth bypass and spend-limit bypass.

ADR 0003 also carries a correction dated 2026-10-08. The toolchain and library specifics in its outcome paragraph recorded the 2026-09-02 intent and drifted from the code; the decision itself is unchanged. Today the Go floor is `go 1.26.8` in `gateway/go.mod`, upstream calls and the SSE relay are hand-rolled over `net/http`, evals orchestration is `asyncio` only, and Ray and the numeric libraries named in the original text are not dependencies. Ray was evaluated and declined on 2026-09-07 in [the sandbox-pool RFC](../rfcs/2026-09-07-evals-sandbox-pool-deferred-decision.md).

## What DESIGN.md got right and where it moved

DESIGN.md's reasoning for the three ADRs still stands, and its shared-contract concept matches what was built: no shared database, no shared library, one `api/` directory. Two details read differently now.

The first is the cache's third layer. The original sketch labelled it "L3 semantic". What shipped on 2026-09-03 is lexical near-duplicate matching: MinHash signatures over word shingles with a Jaccard similarity floor, implemented in `gateway/internal/cache/lexical.go` and governed by [the L3-lite RFC](../rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md). No embedding index exists in the cache package. DESIGN.md now carries a dated correction (2026-10-08) under its diagram saying so; "frozen" means the sketch is never rewritten, not that it cannot take the same dated forward-corrections that ADR 0002, ADR 0003 and `AGENTS.md` carry.

The second is the OTel protobuf schema that DESIGN.md places under `api/` next to `api/gatewayevents/`. No such directory exists. `api/README.md` lists its otel entry as a deliberate placeholder: the gateway already exports real OTLP spans, and a custom schema would duplicate a wire format that already exists.

DESIGN.md's three open questions were taken up later, two settled and one only partly. Semantic-cache risk gating got a first pass as the hard-gate in the L3-lite RFC; DESIGN.md's own 2026-10-08 correction records that the per-content-type Jaccard thresholds and the embedding-based layer itself remain open. The skeptic-panel protocol was settled in two RFCs: [the judge-panel interface RFC](../rfcs/2026-09-07-evals-judge-panel-interface.md) chose independent refutation over pairwise comparison, and [the judge-panel reducer RFC](../rfcs/2026-09-08-evals-judge-panel-reducer.md) built the strict-majority, fail-closed quorum rule. The hierarchical virtual-key scope model is covered below under what does not exist.

## The decisions log in practice

As of 2026-10-08, [DECISIONS.md](../../DECISIONS.md) holds 284 entries running from 2026-09-02 to 2026-10-08. The first three restate the ADRs. The rest show what the log is for. A few themes recur.

**Project mechanics were settled early and held.** The name Kelvran was confirmed clean across package registries on 2026-09-02. Changelogs are one file per released version under each deployable, an explicit override of the single-running-file convention. Branching is trunk-based, `main` only; the founder reopened that question three times (2026-09-02 twice, 2026-09-03) and it was reaffirmed each time, which is the kind of history the never-edit rule preserves (see [BRANCHES.md](../development/BRANCHES.md)). The first release was `gateway/v0.1.0` and `evals/v0.1.0` on 2026-09-03, never `0.0.1` and never `1.0.0`, following semver's own guidance; [the versioning explanation](./versioning.md) covers what followed.

**Contract tooling stayed small on purpose.** Contract testing for `api/` is `buf breaking` plus a golden-fixture round trip, not a Pact broker (2026-09-02). `go-arch-lint` and `import-linter` were wired into CI on 2026-09-05, closing gaps the architecture documents had already named.

**Several features were researched and then not built.** Hierarchical budgets (org, team, user, key, session) had their deferral reaffirmed a second time on 2026-09-07. Pre-call TPM reservation was dropped as a candidate the same day. Rate-limit matching on path, header or provider was left RFC-only on 2026-09-07 in [its own RFC](../rfcs/2026-09-07-gateway-ratelimit-provider-header-path-dimensions.md); a later note in gateway/ARCHITECTURE.md records that the RFC's path trigger did fire when embeddings shipped, and that the concern was already covered by the existing per-model override. Blanket `temperature=0` pinning for the evals judge was decided against on 2026-09-09, and a scoped nightly variant was decided against the same day after a check invalidated its premise. Real Bedrock judge pricing could not be found on the live pricing page on 2026-09-09, and the log records the choice not to fabricate a substitute. Model cascading was documented as a pattern and marked `not_yet` on 2026-09-14.

**Some decisions set a shape without building anything.** If an admin UI is ever built, it is server-rendered Go with `html/template` or `templ` plus htmx, never a new SPA toolchain (2026-09-14). MCP outbound credential brokering was closed as a design-only RFC on 2026-09-12 ([RFC](../rfcs/2026-09-11-gateway-mcp-outbound-credential-design.md)); [the MCP and A2A status page](./mcp-a2a-status.md) explains where that stands.

**Recent entries explain the last release and what has landed on `main` since it.** `gateway/v0.17.0` was cut on 2026-10-07 after a read-only review of changelog against commits. The 2026-10-08 entries all describe work that is on `main` but not in `v0.17.0`: the README is rewritten without dated correction markers, the release pipeline runs GoReleaser in snapshot mode with the workflow publishing, the gateway on `main` now issues the `id`, `object` and `created` envelope of every chat completion it delivers, and the benchmark harness is in-repo Go with open-loop load and no published numbers (see [BENCHMARKS.md](../operations/BENCHMARKS.md)). The pipeline, the envelope and the harness are listed in `gateway/changelog/unreleased.md` until the next release.

The log is long by design. Read it with `grep` for a date or a keyword rather than top to bottom.

## RFCs, plans and research reports

As of 2026-10-08 there are 87 dated RFCs in `docs/rfcs/` (2026-09-02 through 2026-10-08), 22 dated plans in `docs/plans/`, and 156 dated research reports in `docs/upgrade-research/`. The template's status vocabulary is `proposed`, `accepted`, `rejected` and `superseded-by-NNNN`, but only 28 of the 87 RFCs use that vocabulary in the template's inline field; 58 carry a prose status instead, 55 of them under a `## Status` heading, such as "design-only, explicitly deferred pending its own named trigger", and one (the 2026-10-08 streaming-overhead RFC) has none. [The RFC README](../rfcs/README.md) adds the rule that matters most to a reader of this page: an RFC records intent at the time it was written, never what the gateway does today; where an RFC and the code disagree, the code is right, and the changelogs say what shipped in which version. [The MCP inbound RFC](../rfcs/2026-09-20-gateway-mcp-inbound-design.md) is the clearest example of a prose status: it scopes the "how" in full while committing to no "when", and says in its status that no MCP package exists under `gateway/internal/`. [The Redis-backed cache RFC](../rfcs/2026-09-11-gateway-redis-backed-cache-design.md) was the first RFC to carry that exact status wording (two 2026-09-07 RFCs had already recorded deferred or design-only decisions in other words); it credits the precedent to the distributed concurrency-limiter research report of 2026-09-09 (`docs/upgrade-research/gateway-distributed-concurrency-limiter-2026-09-09.md`), and the later design-only RFCs cite it in turn.

Research reports that reach a verdict end with a `build_now` versus `not_yet` table; others reach their conclusions in prose and have no such table: as of 2026-10-08, 74 of the 156 dated reports contain neither the literal token `build_now` nor `not_yet` (counted with `grep -L build_now *.md | xargs grep -L not_yet` over the dated files; looser spellings such as "build now" or "not yet" appear in prose and would shrink that number). `build_now` means validated, concrete, low risk and usually no new code; `not_yet` means a named trigger has not fired. `docs/upgrade-research/client-sdk-strategy-2026-09-13.md` is a representative case: documenting how to point the OpenAI and Anthropic SDKs at Kelvran is `build_now`, and a first-party multi-language SDK is `not_yet` because no external caller has asked for one. [Why there is no SDK](./why-no-sdk.md) explains that outcome.

## Researched and declined

Dated notes in [gateway/ARCHITECTURE.md](../../gateway/ARCHITECTURE.md), all added on 2026-09-23, record ideas that were examined against external evidence and declined rather than left unmentioned.

- A WASM plugin or filter-chain model for guardrails. The newer evidence hardened the earlier "not yet": Kong removed WASM support from its gateway in version 3.11 after two years in beta (`docs/upgrade-research/wasm-plugin-extensibility-2026-09-22.md`).
- Carbon-aware routing. Two of the five adapters, OpenAI and Anthropic, publish no energy or carbon data for their models, so the feature would fly blind on the adapters most likely to carry traffic (`docs/upgrade-research/carbon-aware-sustainable-routing-2026-09-22.md`).
- Edge-native rate limiting. Per-PoP limiting at a CDN edge was found less globally accurate than the gateway's own Redis and GCRA cross-replica design (`docs/upgrade-research/edge-native-ai-gateway-deployment-2026-09-22.md`).
- A Kelvran-native cross-request agent memory store. It would require the gateway to track cross-request state it deliberately tracks nowhere else; provider-hosted memory already passes through as an opaque request (`docs/upgrade-research/agent-memory-context-management-2026-09-22.md`).
- A standardized AI-gateway wire protocol as an adoption candidate for the canonical request schema. The one GA standard surveyed does not apply to Kelvran's architecture, the one that would is pre-alpha and unimplemented by any peer, and Kong, Envoy AI Gateway and LiteLLM have each gone their own way; the verdict is to monitor lightly and revisit in 6 to 12 months, not a permanent no (`docs/upgrade-research/ai-gateway-api-standardization-2026-09-22.md`).
- Moving token delivery from Server-Sent Events to WebTransport: re-confirmed SSE as the right transport for this layer, since every major provider streams over it and WebTransport reaching browser Baseline status in March 2026 changes nothing for a server-to-server gateway (per `docs/upgrade-research/streaming-transport-protocol-evolution-2026-09-22.md`).

## Not available today

Readers often expect one of the following to exist because a design document mentions it.

- No ADR beyond 0001 to 0003. Later calls that feel foundational, such as the admin RBAC tiers, L3-lite and the Redis-backed stores, live only in RFCs and DECISIONS.md.
- No hierarchical virtual-key scope resolution (org, team, user, key, session). DESIGN.md sketched it; [the virtual-keys RFC](../rfcs/2026-09-02-virtual-keys-budgets.md) deferred it on 2026-09-02, and DECISIONS.md reaffirmed the deferral on 2026-09-07 and again on 2026-09-15.
- No OTel protobuf under `api/`. The otel entry in `api/README.md` is a deliberate placeholder, and no such directory exists.
- No Rust FFI island. ADR 0003 permits one only on profiled evidence, and no `.go` file in `gateway/` imports `"C"`.
- No cache gRPC wire contract. The `grpcserver` and `grpcclient` adapters are dormant typed Go only.
- No MCP or A2A brokering code. Both halves exist as design-only RFCs.

## Before you re-decide

The repository's working rules in `AGENTS.md` make the records above binding on process. Check DECISIONS.md and `docs/decisions/` before re-deciding something already settled. Ask first before changing anything in `api/`, before adding a third language or runtime anywhere (ADR 0003 rejects doing so preemptively), and before extracting the cache into its own service (ADR 0002 lists the required triggers).

The three ADRs each point to a fuller scale-stage analysis in a research workspace outside this repository. That material is not part of Kelvran and is not linkable from these pages; the ADRs contain the reasoning needed to apply them.

## Related pages

- [Architecture](./architecture.md) for how the two deployables fit together today.
- [Cache gate](./cache-gate.md) for the hard-gate rule that ADR 0002's cache carries.
- [Versioning](./versioning.md) and [docs/VERSIONING.md](../VERSIONING.md) for the release and tag conventions.
- [Security model](./security-model.md) and [THREAT_MODEL.md](../../THREAT_MODEL.md) for the threat findings behind the cache gate.
- [PRD.md](../../PRD.md) and [REPO_LAYOUT.md](../../REPO_LAYOUT.md) for scope boundaries and where files live.
