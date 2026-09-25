# RFC: Gateway request-log store — design only, no code

## Status

Design-only, explicitly deferred pending its own named trigger. Named as "the true first step" by
`docs/upgrade-research/gateway-observability-devex-2026-09-24.md`'s Follow-up scoping section
(2026-09-24) — a dedicated scoping pass that investigated a Portkey-style Replay feature and a
LiteLLM-style bundled admin UI and found both blocked on the SAME missing prerequisite: a
persisted, queryable request-log store. Mirrors the exact "designed in full, deferred" precedent
already established for the Redis-backed cache (`docs/rfcs/2026-09-11-gateway-redis-backed-cache-design.md`)
and the MCP/A2A outbound-credential design (`docs/rfcs/2026-09-11-gateway-mcp-outbound-credential-design.md`):
scope the "how" now, without committing to the "when." Neither Replay nor a bundled admin UI is
scheduled as work — the 2026-09-24 scoping pass's own verdict was "do not scope an RFC for either
yet" — and this RFC does not change that; it scopes only their shared prerequisite.

## Date

2026-09-25

## Author(s)

Session agent (Claude Code), per the 2026-09-24 observability/DevEx scoping pass's own named
follow-on ("If either DevEx feature is ever pursued, revised order: (1) the shared request-log
store, metadata-only by default...").

## Summary

Kelvran has no persisted, queryable record of a single proxied chat request/response anywhere in
its own code today. `internal/admin/auditstore` durably logs *admin mutation* events (virtual-key
CRUD, config reads) — never a proxied `HandleChatCompletion` call
(`gateway/internal/admin/auditstore/auditstore.go`'s own package doc comment, lines 1-13). The
response cache (L1/L2/L3) and the idempotency store both hold response blobs keyed by an
irreversible SHA-256 hash of the request, with no list/browse API — neither can answer "show me
request #X" unless the caller already knows X's exact content well enough to recompute its own
key. `GatewayDecisionEvent`, the one contract this codebase already builds for offline/cross-request
analysis, carries outcome/cost/trace metadata only — confirmed directly against its real field list
(`gateway/internal/gateway/dataplane/dataplane.go:4186-4227`) — never prompt or completion content,
matching the observability scoping pass's own framing: "zero prompt/completion content, by explicit
design" (`docs/upgrade-research/gateway-observability-devex-2026-09-24.md`, Follow-up scoping
section). This RFC sketches — without implementing — a new store that closes this gap: what it
captures by default, what it captures only opt-in, how it's persisted, and what a first
read/query admin API surface over it would look like.

## Motivation

`docs/upgrade-research/gateway-observability-devex-2026-09-24.md`'s Follow-up scoping section
investigated what building two competitively-motivated features — a Portkey-style Replay and a
LiteLLM-style bundled admin UI — would actually require against Kelvran's real current code, and
found, independently for each: "neither feature can be built without a new, persisted, queryable
request-log store — one does not exist in any form today." That same section explicitly revises
its own recommended build order once the store is named: "(1) the shared request-log store,
metadata-only by default...(now the true first step...); (2) the bundled UI...; (3) Replay last,
gated on its cross-tenant-authorization question being resolved first." This RFC is exactly that
step-(1) scoping pass — design only, and deliberately silent on whether or when either downstream
feature itself gets built.

Scoping this now, while the finding is fresh, follows the same "cheap now, expensive
mid-implementation" reasoning the MCP outbound-credential RFC used for its own trigger
(`docs/rfcs/2026-09-11-gateway-mcp-outbound-credential-design.md`'s Motivation section): the two
nearest-at-hand existing precedents in this codebase — `auditstore`'s single-file sequential scan
and the two `boltstore` packages' pure load/save/delete-only shape — both look superficially
reusable, and both are the wrong shape for this store's own volume and query pattern, for reasons
detailed below. Discovering that mismatch after Replay or the bundled UI already have real call
sites depending on a wrong assumption would not be cheap; discovering it now, against nothing but
a design document, is.

## Detailed Design

### Schema: metadata (default) and raw content (opt-in) as two independently-retained layers

Default metadata capture reuses fields already computed on the request path today — no new
computation, only a new place to durably write what already exists in memory, for exactly the
duration of one request, at `dataplane.Pipeline.finalize`/`logRequest`:

- From the `GatewayDecisionEvent` proto Kelvran already builds on every request
  (`gateway/internal/gateway/dataplane/dataplane.go:4186-4227`): `TraceId`, `SpanId`, `OccurredAt`,
  `VirtualKeyId`, `RequestedModel`, `Outcome`, `RateLimitFailOpen`,
  `FallbackHappened`/`FallbackFromDeployment`/`FallbackReason`, `BudgetSpentUsd`,
  `BillingSubjectId`, `AgentRunId`, `CostUsd`, `CostIsEstimated`, `SavingsUsd`, `FinishReason`.
- From `logRequest`'s own additional computed fields, not yet on the proto
  (`gateway/internal/gateway/dataplane/dataplane.go:4537-4571`): `cache_hit`/`cache_layer`/
  `cache_age_ms`/`cache_similarity` (from `cacheProvenance`), `prompt_tokens`/`completion_tokens`/
  `total_tokens`, and the decimal-string `cost_usd`.
- Available in-process at the same call site but not yet threaded into either the proto or
  `logRequest`'s own fields: `dep.Name`/`dep.Provider` (the resolved deployment and upstream
  provider — both already used elsewhere in this same file for structured logging, e.g.
  `dataplane.go:3062-3063`, `:3452-3479`) and the response's own upstream-reported `Model`, which
  can legitimately differ from `RequestedModel` after a fallback. **Corrected from an earlier draft
  of this RFC, which misattributed this field to the `GatewayDecisionEvent` proto — verified by
  reading the proto message end-to-end**: `ErrorType` belongs on this same "available but not yet
  threaded" list, not the reused-for-free list above — it's set on `ChatCompletionResult`
  (`dataplane.go:4124`, the separate, OTel-span-only result struct passed to
  `telemetry.RecordChatCompletionResult`), a different struct literal from the
  `GatewayDecisionEvent{}` built ~60 lines later at `dataplane.go:4186`, which has no `error_type`
  field at all today. A future implementation should thread `dep.Name`/`dep.Provider`/response
  `Model`/`ErrorType` into this store's write path directly from `finalize`'s existing in-scope
  values, not recompute them, and not assume they arrive "for free" on the proto as written.

None of the above is prompt or completion content. The first bullet above is genuinely free (zero
new capture); the second and third need a small amount of new wiring (reading an already-computed,
in-scope value and threading it to a new write call) but no new computation — still the reuse half
by design, just not uniformly zero-effort across every field.

The opt-in layer is different in kind, not just in size: raw `req.Messages` (including system/tool
messages), `resp.Choices[].content`, the `ResponseFormat` schema, and any multimodal
`Message.Parts` content. This is real prompt/completion content — the first place in this
codebase such content would be durably persisted outside the response cache's own
irreversible-hash-keyed blob (whose key can be recomputed by someone who already knows the exact
request, but whose value cannot be listed or browsed by anyone who doesn't). `docs/operations/TELEMETRY.md`'s
existing stance is explicit and this RFC adopts it verbatim rather than inventing new privacy
language: "Prompt/completion content in trace events is opt-in, not default-on — this is a
deliberate privacy stance, not an oversight" (`docs/operations/TELEMETRY.md:52-54`). The opt-in
flag itself should follow the exact existing `bool`, false-by-default, doc-commented convention
`VirtualKeyConfig.CacheScopeToEndUser` already establishes: its own doc comment's phrasing — "an
opt-in, false-by-default flag folding..." (`gateway/internal/gateway/controlplane/config.go:350-354`)
— is the template to reuse verbatim for a new field in this shape, e.g. `CaptureRequestContent
bool`.

Structurally, the two layers get separate buckets/tables, not a nullable column on the metadata
row — per the same 2026-09-24 scoping pass's own explicit call for "an opt-in (off-by-default,
separately-retained) content store distinct from metadata capture." This is not a style
preference: the two layers need independently configurable retention (a metadata row is safe to
keep far longer than a content row that may contain PII, per the Drawbacks section below), and a
single table with a nullable content column would couple both retention policies to one shared
row's lifecycle, defeating the entire point of gating content capture separately in the first
place.

### Storage backend: bbolt for v1, not auditstore's JSONL-append and not (yet) Postgres

Neither existing precedent fits this store's own access pattern as-is:

- `auditstore.Query` (`gateway/internal/admin/auditstore/auditstore.go:154-183`) is a full-file
  sequential scan plus an in-memory `Filter.matches` check on every single call. **Corrected from
  an earlier draft of this RFC, which claimed reads and writes share one `sync.Mutex` — verified
  by reading the package line by line**: only `Store.Append` (the writer, `auditstore.go:86-87`)
  actually locks `s.mu`; `Store.Query` (`:102-104`) delegates to the package-level `Query` function
  (`:154`), which opens the file directly via `os.Open` with no locking at all. Reads and writes
  are today NOT mutually exclusive in the real code — a real, previously-undiscovered minor
  correctness gap in `auditstore` itself (a concurrent `Append` could in principle interleave with
  an in-progress `Query`'s own read), out of scope for this RFC to fix, but worth naming rather
  than silently inheriting the same unguarded-read shape into a brand-new store without at least
  disclosing it. Locking aside, the scan-cost conclusion is unchanged: that access pattern is the
  right trade for `auditstore`'s own actual volume — admin mutations, a handful per day on a real
  deployment — but proxied-request volume is orders of magnitude higher; a sequential scan over
  every logged chat completion since the process's first cold start, on every single list/filter
  call, degrades linearly and does not stay fine at that scale, whether or not it's lock-guarded.
- `identity/boltstore.Store` and `prompt/boltstore.Store`
  (`gateway/internal/identity/boltstore/boltstore.go`, `gateway/internal/prompt/boltstore/boltstore.go`)
  each give exactly `Load`/`Save`(/`Delete`) over one bbolt bucket, keyed by an application ID with
  the whole value JSON-encoded (`identity/boltstore.go:76-116`, `prompt/boltstore.go:74-114`) —
  real point lookups, zero secondary index, zero TTL, driven only by explicit admin write actions
  (a key rotation, a prompt version upsert). Neither package's own doc comment claims more than
  that scope, and neither is wrong to be scoped that way for what it is for — but a list/browse
  view filtered by `virtual_key_id`, model, outcome, or a time range needs an index bbolt's own
  single-bucket-by-primary-key shape does not give for free.

This RFC recommends bbolt for v1 anyway, not a rewrite of either precedent's own shape: a primary
bucket (`trace_id` -> the JSON-encoded metadata row, mirroring `identity/boltstore`'s and
`prompt/boltstore`'s exact "JSON value, no bespoke field layout" convention) plus 2-3 hand-rolled
secondary-index buckets — a `virtual_key_id:trace_id -> ∅` bucket and a time-bucketed
(`YYYY-MM-DDTHH:trace_id -> ∅`) bucket, each range-scanned by key prefix via bbolt's own
`Cursor.Seek`. This is genuinely new code neither existing boltstore package needed before — the
first time this repo's own bbolt usage needs a secondary index at all — but it stays inside this
project's "proven single-instance-durability pattern," per `prompt/boltstore.go`'s own doc comment
(quoted below), with zero new deploy dependency and zero new external service.

`prompt/boltstore.go`'s own doc comment already names the honest alternative and defers it
explicitly: "Mirrors gateway/internal/identity/boltstore and gateway/internal/budget/boltstore
file-for-file -- this repo's own proven single-instance-durability pattern -- rather than waiting
on the Postgres control-plane store gateway/ARCHITECTURE.md's Tech Stack table names as the
eventual real target: that decision has no trigger of its own yet"
(`gateway/internal/prompt/boltstore/boltstore.go:3-8`), citing `gateway/ARCHITECTURE.md:763`
("Control-plane config store | Postgres (`pgx`/`sqlc`) — still the target for real control-plane
state"). This RFC borrows that same "proven pattern now, named-trigger deferral" shape, but
**corrected from an earlier draft, which copied `prompt/boltstore.go`'s own Postgres citation
uncritically**: this store is a request-log/event stream, not control-plane config — the closer
analog in the same Tech Stack table is the very next row, `gateway/ARCHITECTURE.md:765`
("Observability sink | ClickHouse (`clickhouse-go`) remains the intended future target for a
durable, queryable event store at scale"). ClickHouse, not Postgres, is the architecturally
correct long-term fit this RFC should name: real `WHERE`/index support, real pagination cursors,
real aggregate counts at genuine event-stream scale, none of which bbolt's hand-rolled index
buckets give for free — but, exactly like the Postgres control-plane decision, that decision has
no trigger of its own yet either. Naming the trigger explicitly, mirroring the Redis-cache RFC's
own "trigger, not preemptive build" framing
(`docs/rfcs/2026-09-11-gateway-redis-backed-cache-design.md`'s Status section): a real operator
running multi-field `AND` filters, needing real pagination cursors instead of an index-bucket
walk, or outgrowing manual secondary-index maintenance, is the revisit condition — not a
speculative build ahead of any of those being real.

A plain JSONL-append file (`auditstore`'s own shape) is rejected outright for this store, not
merely deferred like Postgres: `auditstore.Query`'s full-file scan is a correctness-preserving,
performance-losing trade at admin-mutation volume, but at proxied-request volume it stops being a
performance trade and starts being a real availability risk — a list/filter call over months of
accumulated chat-completion rows would tie up the read path for a duration that only grows, with
no bound. bbolt's B+tree gives real point lookups and prefix range scans without that failure
mode, at the same "no new deploy dependency" cost.

### The genuinely new part: retention and size-bounding

Neither existing precedent has any retention primitive to borrow. `auditstore.Append` never
deletes — `SECURITY.md`'s own Data Retention table already discloses this plainly for the admin
audit log specifically: "Indefinite by design ... | Yes — `admin.enable_audit_log: false` disables
it entirely | **None** at the record level (only whole-log disablement)" (`SECURITY.md:99`). The
two `boltstore` packages are pure CRUD with no time dimension at all — an entry lives until an
explicit `Delete` call, never a background sweep. A request-log store, writing at
proxied-traffic volume rather than admin-mutation or explicit-write volume, cannot inherit either
"never delete" posture without becoming a real disk-exhaustion risk — exactly the failure class
`AGENTS.md`'s own Gotchas list already flags for a different concrete incident (the
rootless-Docker entry's own header notes this file's promotion bar is "recurs 3+ times"),
generalized here to a new one before it recurs for real.

Two genuinely new mechanisms this store needs, neither borrowed from an existing package:

1. **An operator-configured retention policy** — a max-age duration and/or a max-total-size bound,
   mirroring `VirtualKeyConfig.BudgetResetIntervalSeconds`'s existing "operator-configured duration,
   parsed from config, positive-means-enabled, zero preserves the original never-expiring behavior"
   shape (`gateway/internal/gateway/controlplane/config.go:271-277`) rather than inventing a new
   config idiom for this one field.
2. **Real enforcement.** bbolt has no native per-key TTL (unlike Redis's `SET ... EX`, which the
   Redis-cache RFC leans on directly for its own retention story, per that RFC's Concrete Redis
   Operations section). Enforcing an age- or size-bound over a bbolt-backed store needs either a
   background sweep goroutine keyed off the time-bucketed secondary index above (oldest-first,
   walking forward from the earliest time bucket until back under the size/age bound) or an
   eviction check folded into the write path itself. Both are real, new code — nothing in this
   codebase does this today for any bbolt-backed store.

### Read/query API: two new admin routes, the exact existing tier convention

`internal/admin/admin.go` already establishes the pattern this store's own read surface should
copy without deviation. `GET /admin/audit` is wired, when a store is configured, as
`requireEitherBearerToken(creds, queryAuditLogHandler(auditStore))` (`admin.go:207-209`) — the
same Admin-or-Viewer tier as every other read-mostly admin route (`GET /admin/virtual_keys`,
`GET /admin/prompts`, etc.), never the narrower `CostViewer` tier (reserved for the one
cost-specific route, `GET /admin/virtual_keys/{name}/spend`, via its own dedicated
`requireAnyBearerToken` call, `admin.go:220-225`) and never gated behind `Operator` (reserved for
reversible, single-resource writes, per `docs/rfcs/2026-09-20-gateway-admin-rbac-risk-tiering.md`'s
own tier taxonomy — this is a pure read, so `Operator`'s write-scoped rationale does not apply at
all). `queryAuditLogHandler` itself (`admin.go:486-523`) accepts `msg`/`field`/`value`/`since`/`until`
query parameters against `auditstore.Filter` — the closest existing shape to mirror for a new
filterable list route.

A future implementation should add:

- **`GET /admin/requests`** (list, filterable by `virtual_key_id`, `model`, `outcome`, and a
  `since`/`until` time range) — the same `requireEitherBearerToken(creds, ...)` wrap as
  `GET /admin/audit`, querying the metadata store's secondary-index buckets described above.
- **`GET /admin/requests/{trace_id}`** (point lookup) — the same tier, a direct bbolt point read
  against the primary bucket.

Both routes must use the same `auditLogger`-wrapped read-audit-the-read discipline every existing
GET route already has (`admin.go`'s `auditLogger` type, `:167-171`, and its `Info` method,
`:173-187`, used across every handler in the file) — a request-log store is, if anything, a MORE
sensitive read surface than the existing admin audit log, and should not be the one GET route this
codebase exempts from that logging convention. Whether the same two routes also serve the opt-in
content layer (e.g. via `?include_content=true`), or whether that needs its own separately-gated
route entirely, is left to a future implementation RFC — resolving it requires deciding the
cross-tenant replay-authorization question this RFC deliberately leaves open below, and a
content-inclusive read route is exactly the kind of capability that question needs answered first.

## Drawbacks

This store's opt-in content-capture layer is, explicitly, the first place in this codebase that
would durably persist real prompt/completion content outside the response cache's own
irreversible-hash-keyed, non-listable blob. `THREAT_MODEL.md`'s Gateway Information Disclosure row
already frames guardrail scanning as in-flight only — pre- and post-call PII/content scanning,
never a persistence step (`THREAT_MODEL.md:14`) — and the same 2026-09-24 scoping pass that
motivated this RFC confirms the consequence directly against the proto:
`GatewayDecisionEvent`/OTel spans carry "outcome/cost/trace metadata only, zero prompt/completion
content, by explicit design" (`docs/upgrade-research/gateway-observability-devex-2026-09-24.md`,
Follow-up scoping section). Building the opt-in layer this RFC scopes is a materially new posture,
not an extension of an existing one, and needs to be named as such rather than glossed over as
"just another optional flag." `docs/operations/TELEMETRY.md`'s own "opt-in, not default-on"
framing (quoted above) is the right posture to inherit, but inheriting the framing does not
eliminate the real retention/PII risk once an operator does opt in — a retention window long
enough to be useful for Replay is, definitionally, a window during which real PII sits durably on
disk in a form the existing guardrail scan (which only ever inspects content in-flight, never at
rest) has no ongoing visibility into.

A second, more mundane drawback: this is a genuinely new subsystem — a new bbolt file, a new pair
of admin routes, a new background retention-sweep goroutine — on top of every existing store this
codebase already has to reason about (`identity`'s and `prompt`'s boltstores, `budget`'s
persistence layer, `auditstore`'s JSONL file, the three-layer response cache). Each is small in
isolation, matching this project's own "many small files" convention, but the aggregate
operational surface (one more file to back up, one more retention policy to document in
`SECURITY.md`'s Data Retention table, one more thing `docs/operations/DEPLOY.md`'s
disaster-recovery guidance needs to know about) is real and should not be undercounted just
because no single piece of it is individually large.

Third: bbolt's hand-rolled secondary-index buckets (the `virtual_key_id:trace_id` and
time-bucketed buckets described above) are real, new, untested code — the first time this
codebase's bbolt usage needs anything beyond a primary-key-only bucket. A bug in index maintenance
(an index entry surviving a metadata-row deletion, or vice versa) is a class of defect neither
`identity/boltstore` nor `prompt/boltstore` has ever needed to guard against, since neither
maintains a secondary index at all today.

## Alternatives Considered

**Reuse or extend `auditstore` instead of building a new store.** Rejected on access-pattern
grounds, not because the idea is unreasonable on its face: `auditstore.Query`'s full-file
sequential scan (`auditstore.go:154-183`) is a correctness-preserving trade specifically because
admin-mutation volume is low (a handful of writes per day on a real deployment). Proxied-chat-
request volume is orders of magnitude higher — every `HandleChatCompletion` call, not just an
operator's own administrative actions — and a sequential scan over that volume, on every single
list/filter call, degrades in a way `auditstore`'s own design was never asked to survive. Reusing
it would also conflate two conceptually distinct audiences (an operator's own administrative
actions vs. a tenant's proxied traffic) inside one file and one `Filter` type never designed for
the second audience's query shape — filtering by `virtual_key_id` across months of chat
completions is not the same query shape as "everything about this one virtual key's admin
history."

**Do nothing until Replay or the bundled admin UI actually gets built.** A real option, and
arguably the honest default absent this RFC — the 2026-09-24 scoping pass's own verdict was "do
not scope an RFC for either yet" for the two downstream features themselves, precisely because
neither has demonstrated real operator urgency (both originated from a competitive-gap scan, not
an incident or complaint). This RFC does not contradict that verdict for Replay or the bundled UI
— it scopes only the shared prerequisite, on the reasoning that the finding itself ("both are
blocked on the exact same missing store") is fresh and cheap to record now, mirroring the same
"scope the how now, not the when" pattern the MCP outbound-credential RFC and the Redis-cache RFC
both already established for this codebase. Waiting to scope even this prerequisite risks the same
"nearest-at-hand shortcut" failure mode those two precedent RFCs were each written to head off —
whoever eventually starts building Replay or the bundled UI reaching for `auditstore` unchanged
because it is the nearest existing thing, discovering the volume mismatch only after real call
sites already depend on it.

**Build directly on ClickHouse now, skipping bbolt as an intermediate step.** Rejected for the same
reason the Redis-cache RFC rejected building a distributed cache ahead of a fired trigger:
ClickHouse is architecturally correct for this store's eventual query shape, but
`gateway/ARCHITECTURE.md:765`'s own "intended future target for a durable, queryable event store
at scale" framing has had no trigger of its own for any store in this codebase yet, and this RFC
does not manufacture one. Building it now, speculatively, adds a new deploy dependency (a
ClickHouse instance every single-binary Kelvran deployment would newly require) for a store whose
own downstream consumers (Replay, the bundled UI) are themselves not yet triggered.

## Unresolved Questions

**Who is authorized to read — and, if Replay is ever built, replay — another tenant's logged
request, and under what credential?** This is deliberately left open, not answered, matching the
2026-09-24 scoping pass's own framing of this exact question as Replay's "hardest open question...
a privilege-escalation decision, not a feature flag." This RFC's own read routes
(`GET /admin/requests`, `GET /admin/requests/{trace_id}`) sidestep the sharpest form of the
question by staying at the existing Admin/Viewer operator-credential tier — an already-authorized
operator reading ACROSS tenants, which is exactly what `GET /admin/audit` and
`GET /admin/virtual_keys` already do today, not a new kind of access. But that framing does not
survive contact with Replay specifically, which needs to ACT as a tenant (re-run its request,
under its own budget/rate-limit identity) without possessing that tenant's own `VirtualKey`
secret — a materially different, harder question this RFC does not resolve.

A dedicated check this session ran specifically for this question found no existing "act on behalf
of a virtual key" primitive anywhere in this codebase to build on. The Admin/Viewer/CostViewer/
Operator tier model (`docs/rfcs/2026-09-20-gateway-admin-rbac-risk-tiering.md`) is, by that RFC's
own explicit framing, an axis of WHICH OPERATION a static operator credential may perform, never
WHICH TENANT it acts as — that RFC's own Alternatives Considered section names the tenant-scoped
axis explicitly and defers it: "Per-virtual-key or per-tenant-scoped admin credentials... out of
scope here: this is the genuinely deferred multi-tenancy/RBAC question... a different axis (which
tenant) than this RFC's axis (which operation), and gated on a real customer trigger that has not
fired" (`docs/rfcs/2026-09-20-gateway-admin-rbac-risk-tiering.md`, line 82). The admin API's mTLS
authentication (shipped directly, with no dedicated RFC file — `DECISIONS.md`'s `[2026-09-23]`
fourth same-day entry, commit `d55764db`) authenticates the admin surface's TRANSPORT (an
operator's own client certificate against the server), not a per-tenant delegation either.

The closest existing precedent in this codebase for "prove authority over X without holding X's
own secret" is the OBO (On-Behalf-Of, RFC 8693) token-exchange SHAPE named — but not built — in the
deferred MCP outbound-credential RFC: "RFC 8693 On-Behalf-Of (OBO) token exchange... requires TWO
simultaneous credentials (the caller's own token AND a service credential) to mint the downstream
token" (`docs/rfcs/2026-09-11-gateway-mcp-outbound-credential-design.md`, lines 37-40). That RFC's
own direction is the opposite of this one — Kelvran-as-client authenticating outward to a
downstream MCP server, not an operator reading or replaying another tenant's own inbound traffic —
but the two-credential SHAPE (proving authority over a resource without holding that resource's
own secret) is the nearest documented analog either investigation for this RFC found. Naming it
here is deliberately not the same as resolving this RFC's own open question: whether a future
Replay implementation would extend the Operator/Admin tier model with a new tenant-scoped grant (a
real design gap `THREAT_MODEL.md` has not priced), borrow OBO's two-credential shape, or do
something else entirely once a real Replay implementation forces the question, remains genuinely
open.

**Retention defaults and `SECURITY.md` disclosure.** This RFC's storage design section names the
mechanism (an operator-configured max-age/max-size bound) but not a specific default number for
either layer. A future implementation should add this store's own row to `SECURITY.md`'s existing
Data Retention & Right to Erasure table (`SECURITY.md:93-99`), using that table's own established
"disclosed provisional default... not a number derived from a specific compliance requirement"
framing (`SECURITY.md:87-91`) rather than inventing new retention-disclosure language — but the
actual numbers (a metadata-row default retention, and a shorter, separately-configured
raw-content default retention given the PII exposure named in Drawbacks) are not decided here.
**Named but also not decided here: what that new row's own Erasure Mechanism cell should say.**
This is a distinct question from the retention-window numbers above — every other row in that
table has an explicit answer (a real `DELETE`/erase route, a TTL-only "None," or whole-log
disablement). This store's own bbolt design (a primary bucket keyed by `trace_id`, per the
Storage Backend section above) makes a real point-delete-by-`trace_id` primitive trivially cheap
to add — a genuine, positive design property this RFC did not initially point out — but whether a
future implementation actually exposes that as a new admin route (and, if so, at which auth tier;
see the Replay-authorization question above for why "any tenant's own trace_id" access needs its
own care) is left for that implementation to decide, not assumed here.

**Where `CaptureRequestContent`-equivalent configuration lives.** Whether the opt-in
content-capture flag belongs alongside `VirtualKeyConfig`'s existing per-key opt-in flags
(mirroring `CacheScopeToEndUser`'s own per-key scope) or as a deployment-wide `AdminConfig`-level
switch (mirroring `EnableAuditLog`'s own global-not-per-key scope,
`gateway/internal/gateway/controlplane/config.go:756-764`) is not decided here — both existing
precedents are real and legitimate, and the right choice likely depends on whether content capture
is expected to vary per tenant (per-key) or is an operator-wide compliance posture (global), a
question this design pass did not need to resolve to scope the store itself.

**ClickHouse migration trigger, named but not fired.** Mirroring
`docs/rfcs/2026-09-11-gateway-redis-backed-cache-design.md`'s own explicit trigger-recheck
convention, this RFC names its own: real multi-field `AND` filters, a real pagination-cursor need,
or aggregate counts outgrowing hand-rolled bbolt index-bucket maintenance. None of these have
fired as of this RFC's own date — bbolt v1 remains the right scope for a store with zero real
callers yet.

## Verification

None — this RFC is design-only, per its own Status line and the 2026-09-24 scoping pass's own
scope. No code changes accompany it; a future implementation pass would need its own new
`internal/requestlog` (or similarly named) package, tests mirroring `identity/boltstore`'s and
`prompt/boltstore`'s existing bbolt-integration test patterns plus new coverage for the
secondary-index and retention-sweep mechanisms neither existing package needed before, and its own
RFC status update from "Design-only" to "Accepted, implemented."
