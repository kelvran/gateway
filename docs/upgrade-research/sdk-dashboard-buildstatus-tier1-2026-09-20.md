# Client SDK + Admin Dashboard UI — Build-Status Confirmation & Tier-1 Slice (2026-09-20)

## Research Question

Since `client-sdk-strategy-2026-09-13.md` and `admin-dashboard-ui-2026-09-14.md` were written
(both concluded `not_yet`), has any client SDK (any language) or admin dashboard UI actually
been built for Kelvran — checked directly against the live repo, not assumed from the prior
reports? Given the admin API surface has grown substantially since those two reports (virtual
keys, prompt labels + promote/rollback, deployment weight, cache erasure, backup, embeddings,
`GET /admin/virtual_keys/{name}/spend`), what is the single most valuable, minimal-scope v1
slice for each — grounded in how comparable platforms (LiteLLM, Portkey, Kong) actually shipped
theirs first, not in the abstract?

## Ground Truth: Build Status (confirmed directly against the live repo, 2026-09-20)

**Neither has been built. Zero client SDK and zero dashboard UI exist anywhere in this repo
today.**

- `find . -maxdepth 3 -type d` (excluding `.git`/`node_modules`) shows exactly two deployables —
  `gateway/` (Go) and `evals/` (Python) — plus the `api/` protobuf contract, `deploy/`,
  `terraform/`, and `docs/`. No `sdk/`, `client/`, `dashboard/`, `frontend/`, `ui/`, or `web/`
  directory exists at any depth.
- `find . -name package.json -o -name go.mod -o -name pyproject.toml` (excluding
  `.git`/`node_modules`) returns exactly `evals/pyproject.toml` and `gateway/go.mod` — the two
  known deployables, nothing else. A dashboard would need its own `package.json` (there is
  none); a new-language SDK would need its own manifest (there is none).
- A repo-wide grep for `sdk|dashboard|frontend|client-sdk|admin-ui|webui` (excluding
  `.git`/`node_modules`) turns up only: two Grafana JSON dashboard files (infra metrics, already
  disclosed as existing in the 2026-09-14 report), the two prior research reports themselves,
  and unrelated matches inside `evals/.venv`'s vendored `botocore`/`opentelemetry` packages
  (paginator files literally named `*.sdk-extras.json`, and `google.auth._cloud_sdk` — third-party
  SDKs *Kelvran depends on*, not anything Kelvran built).
- `git log --oneline --since=2026-09-14 -- gateway/internal/admin/` shows 20 real commits
  touching the admin surface since the dashboard report's date — cache erasure, config
  propagation, prompt label promote/rollback, audit-log toggle, deployment-weight mutation,
  bbolt backup, virtual-key rotation, a narrow CostViewer tier + spend endpoint, pprof — real,
  substantial admin-surface growth, confirmed by commit log, not doc claims. None of those 20
  commits touch a client or UI directory, because none exists to touch.

**The admin API surface as it stands today** (`gateway/internal/admin/admin.go:167-227`, read
directly):

```
GET    /admin/config
POST   /admin/virtual_keys/{name}
DELETE /admin/virtual_keys/{name}
POST   /admin/virtual_keys/{name}/rotate
GET    /admin/virtual_keys/{name}/spend      (admin, viewer, OR the narrow cost_viewer tier)
GET    /admin/prompts
GET    /admin/prompts/{id}
GET    /admin/prompts/{id}/versions/{version}
POST   /admin/prompts/{id}
DELETE /admin/prompts/{id}
PUT    /admin/prompts/{id}/labels/{label}     (promote — and rollback, via SetLabel to an older version)
DELETE /admin/prompts/{id}/labels/{label}
POST   /admin/backup
POST   /admin/deployments/{name}/weight
POST   /admin/cache/erase
GET    /admin/debug/pprof/*                    (opt-in)
```

One concrete, previously-unreported gap surfaced by reading this route list directly: **there is
no `GET /admin/virtual_keys` (list) route.** Every virtual-key operation is scoped to
`{name}` — an operator (or a future dashboard) must already know a key's name to do anything
with it; there is no way to enumerate keys via the admin API today. This is not a missing
data-layer capability — `internal/identity/identity.go:284`'s `func (v *Verifier) Keys()
[]VirtualKey` already returns the full list in-process — it is purely a missing HTTP route.
`embeddings` (named in the research question as part of the admin-surface growth) is not
actually an admin-API item: `POST /v1/embeddings` (`gateway/cmd/gateway/main.go:397`) is a
data-plane endpoint, wired to `dataplane.Pipeline.HandleEmbeddings`, same tier as chat
completions — it has no admin-surface footprint at all.

## Findings

### Finding 1 — LiteLLM's SDK and its dashboard UI have never been separate build decisions; LiteLLM *is* the Python package, and its UI shipped within days of the core repo, not as a later addition
**Confidence: high** (primary source: GitHub API, `gh api repos/BerriAI/*`)

`BerriAI/litellm` (language: Python) was created 2023-07-27. There is no separate first-party
LiteLLM *client* SDK repo for a different language doing the equivalent of "call
chat-completions" — `BerriAI/litellm-agent-sdk` (TypeScript, created 2026-05-07) is a distinct,
much later product for LiteLLM's *Agents* feature (Agent → Session → Run), not a chat-completion
client. This means LiteLLM's real-world SDK sequencing is: **Python first, by nearly three
years, with no first-party TS/JS chat-completion client ever following** — corroborating
`client-sdk-strategy-2026-09-13.md` Finding 3's point that LiteLLM actively steers proxy callers
toward `base_url`-redirected OpenAI/Anthropic SDKs instead. On the dashboard side:
`BerriAI/litellm-ui` (JavaScript) was created 2023-07-29 — **two days** after the core repo —
though `gh api repos/BerriAI/litellm-ui` shows it was pushed once and never touched again
(`pushed_at` == `created_at` + 34 minutes); the UI that actually persisted and is documented
today lives inside the main repo at `ui/litellm-dashboard/` (confirmed present via
`gh api repos/BerriAI/litellm/contents/ui`), which `admin-dashboard-ui-2026-09-14.md` Finding 1
already independently verified as a real, actively-maintained Next.js/React SPA. Net signal:
for a self-hosted multi-tenant proxy whose primary audience runs teams/keys/budgets
day-to-day, LiteLLM's operators needed *some* key-management UI almost immediately — the two-day
gap, even though that specific repo was abandoned, shows the UI was never an afterthought
relative to the SDK/proxy for this product shape.

### Finding 2 — Kong's Admin UI shipped roughly 8.6 years after Kong's Admin API, not alongside it — the opposite sequencing from LiteLLM
**Confidence: high** (primary source: GitHub API, `gh api repos/Kong/*`)

`Kong/kong` (the gateway itself, Lua, whose Admin API has existed since the project's early
architecture) was created 2014-11-17. `Kong/kong-manager` — described by Kong itself as
"Admin GUI for Kong Gateway (Official)," language TypeScript — was created 2023-06-20: **~8.6
years later.** Kong's target audience (platform/API-gateway ops teams operating at scale,
typically via IaC/Terraform/CI against the Admin API directly, per the ecosystem's own tooling
patterns — e.g. multiple third-party Terraform Kong providers exist and predate the official
UI) tolerated raw-API-only operation for the better part of a decade before an official
dashboard became a priority. This is the clearest available real-world evidence that "no admin
UI yet" is not inherently a competitive gap — it depends entirely on the target operator's
actual workflow, exactly the judgment call `admin-dashboard-ui-2026-09-14.md`'s Bottom Line
already flagged as outside research's power to resolve and requiring "a real roadmap decision."

### Finding 3 — Kelvran's current position is structurally closer to Kong's first 8 years (single/few self-serve operators, API-first) than to LiteLLM's day-one multi-tenant-team posture — and the open question from the 2026-09-14 report (a second tenant/operator) is still unanswered
**Confidence: medium** (inference from Kelvran's own repo state, not a new external primary source)

`admin-dashboard-ui-2026-09-14.md`'s own open questions ended on: "Is there an actual near-term
second tenant/operator on Kelvran's roadmap... or is the pilot still single-operator with no
onboarding plan?" Nothing found in this pass answers that question — it was not re-litigated
here, and no roadmap artifact in the repo settles it. What *is* newly confirmed: even if that
trigger fired today, the admin API itself could not yet back a "list your virtual keys" screen
— the one screen every comparable dashboard (LiteLLM's key/budget UI, Portkey's Model Catalog)
leads with — because `GET /admin/virtual_keys` does not exist (see Ground Truth section above).
Any dashboard decision and any "build the list endpoint" decision are now coupled in a way they
were not on 2026-09-14, when the admin surface was smaller.

### Finding 4 — Kelvran's one real external caller, and its own second deployable, are both already Python — reinforcing (not just repeating) the prior report's Python-leaning signal
**Confidence: high** (primary source: this repo's own `AGENTS.md` and directory layout, re-confirmed this pass)

`client-sdk-strategy-2026-09-13.md` already established that Deep-Research's `PlannerClient`/
`AgentToolClient` call Kelvran successfully via plain Python `httpx`, with zero Kelvran-provided
client. This pass reconfirms Kelvran's own second deployable, `evals/`, is Python (`uv`,
`evals/pyproject.toml`) — there is no TypeScript/Go anywhere in this repo outside `gateway/`
itself (`gateway/go.mod` is the only other manifest). Combined with Finding 1 (LiteLLM is
Python-native with no first-party JS/TS chat-completion client), every concrete signal
available — Kelvran's own real external caller, Kelvran's own second deployable, and the
nearest comparable OSS gateway's actual language choice — points the same direction. If the
`not_yet` on a first-party SDK is ever converted to `build_now` (still gated on a real trigger
per the 2026-09-13 report's Open Question 1, which remains open), Python is the language with
zero counter-evidence and three independent lines of supporting evidence, not a default guess.

## Recommendation (build_now vs not_yet — reaffirms, does not overturn, both prior reports)

| Item | Verdict | Why |
|---|---|---|
| Ship any client SDK today | **not_yet** (unchanged) | No new trigger found; still zero external caller asking for one beyond Deep-Research's working `httpx` client |
| *If* triggered later: which language first | **Python** | Findings 1 + 4: Kelvran's only real caller, Kelvran's own `evals/` deployable, and LiteLLM's own real sequencing all agree; no evidence favors another language first |
| Ship a bespoke dashboard UI today | **not_yet** (unchanged) | Finding 2/3: Kelvran's current single-operator, API-first posture matches Kong's first ~8 years more than LiteLLM's day-one team posture; the 2026-09-14 report's own trigger question (second tenant/operator) is still unanswered |
| *If* triggered later: minimal v1 dashboard slice | **Virtual-key list + per-key spend view** | Matches what LiteLLM's UI led with (key/budget management) and what Portkey's Model Catalog leads with (org-level credential + budget visibility, per `admin-dashboard-ui-2026-09-14.md` Finding 3) — and is the one view an operator cannot currently get via curl at all, list-by-name only |
| Prerequisite before that v1 slice is even buildable | **build_now, small** | `GET /admin/virtual_keys` does not exist yet (Ground Truth section) even though the underlying data (`identity.Verifier.Keys()`) already does — this is a small, scoped backend addition, not a dashboard-scale project, and unblocks the v1 dashboard slice above whenever it's triggered |

## Caveats

- The Exa web-search MCP tool hit its free-tier rate limit mid-session; all cross-repo evidence
  in this report (LiteLLM, Kong, BerriAI org repos) was gathered instead via the GitHub API
  directly (`gh api` / `gh search repos`), which is a primary source, not a fallback of lower
  quality — but it means this report did not independently re-fetch LiteLLM's or Kong's own
  *docs pages* fresh in this pass; it relies on the two prior reports' already-verified doc
  claims plus this session's own fresh GitHub repository metadata.
- "`BerriAI/litellm-ui` shipped two days after the core repo" is a real, dated fact, but that
  specific repo was abandoned after one push — it is evidence about *intent/priority timing*,
  not evidence that repo itself became LiteLLM's enduring UI (the enduring one is
  `ui/litellm-dashboard/` inside the monorepo, already verified by the 2026-09-14 report).
- Kong's ~8.6-year Admin-API-to-Admin-UI gap is a real, dated fact from GitHub repo creation
  timestamps, but repo-creation date is a proxy for "when the *official* GUI effort started,"
  not necessarily "when Kong operators first got *any* GUI" — third-party community tools
  (e.g. Konga) existed earlier in Kong's history and were not independently re-verified here;
  the claim is scoped specifically to Kong's own *official* Admin GUI.
- The "Kelvran is structurally closer to Kong's early years than LiteLLM's day one" claim
  (Finding 3) is this report's own inference from Kelvran's repo state, not a claim sourced from
  an external comparable platform — flagged at medium confidence and should not be read as
  equivalent in strength to Findings 1/2/4, which rest on direct GitHub API primary sources.

## Open Questions

1. (Carried over, still unanswered) Is there an actual near-term second tenant/operator on
   Kelvran's roadmap? This remains the single concrete trigger that would convert the dashboard
   `not_yet` to `build_now`, per `admin-dashboard-ui-2026-09-14.md`.
2. (Carried over, still unanswered) Has any real external caller besides Deep-Research asked for
   a typed client, in any language? Per `client-sdk-strategy-2026-09-13.md` Open Question 1.
3. (New) If `GET /admin/virtual_keys` is added, should it return spend/budget fields inline (one
   call, matching what a list-view dashboard would want to render directly) or stay minimal
   (name/created/rotation-state only, forcing N follow-up calls to the existing per-key
   `/spend` route)? This is a real API-design decision now that the list-route gap is
   identified, not something this report resolves.
4. (New) Given Kong's own official Admin UI took ~8.6 years to arrive after its Admin API, is
   there a documented reason to believe Kelvran's timeline should compress dramatically relative
   to that precedent — or is Kong's own history itself the strongest evidence yet that `not_yet`
   is comfortably sustainable for a good while longer?
