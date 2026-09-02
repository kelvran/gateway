> **For agentic executors:** work through this task-by-task, checking off each step as it's done. Run the build/test command at the end of each task before moving to the next — don't accumulate unverified work.

---

**Goal:** Produce the first buildable, tested skeleton of both `gateway` (Go) and `evals` (Python), proving out the two riskiest architectural seams (provider adapter translation, the `cache.Cache` interface boundary) for real rather than as a stub.

**Architecture:** Exactly the package layouts already committed to in `gateway/ARCHITECTURE.md` and `evals/ARCHITECTURE.md` — this plan does not redesign anything, it implements what's already specified there.

**Tech Stack:** Go 1.25 language level (1.26.5 toolchain verified locally), stdlib `net/http` + `log/slog`, no third-party Go deps for this pass (keeps the skeleton dependency-free until a real need arises). Python: `uv`-managed, `>=3.12`, `pydantic` for `EvalCase`, `click` for the CLI, `pytest` for tests, stdlib `subprocess`/`asyncio` for the Docker sandbox wrapper (no `docker` SDK dependency for this pass — shelling out to the `docker` CLI is sufficient and keeps deps minimal).

**Spec:** `docs/rfcs/2026-09-02-initial-code-scaffolding.md` — read it first; it defines exactly what's real vs. stubbed in this pass and why. Every task below stays inside that scope boundary.

**Global Constraints** (inherited from the spec + `AGENTS.md`, apply to every task):
- Never store secrets in a committed file.
- `cache` package never imports `provideradapter` or anything provider-specific.
- `evals` never imports `gateway`'s Go source; neither imports the other.
- A stubbed capability returns a clear, typed "not implemented" error — never a silent no-op or fake success.
- Every provider adapter gets a round-trip unit test (canonical → provider-native → canonical, lossless) before the task is considered done.

---

## Phase 1: Gateway Skeleton (Go)

### Task 1: Module + Canonical Types

**Files:**
- Create: `gateway/go.mod`
- Create: `gateway/internal/adapter/types.go`

**Interfaces:**
- Produces: the canonical `ChatRequest`/`ChatResponse`/`Message`/`ToolCall` types every adapter and the router/cache/pipeline code depends on.

**Steps:**
- [ ] `go mod init github.com/kelvran/gateway` with `go 1.25` in `go.mod`.
- [ ] Define canonical types in `types.go` (OpenAI Chat-Completions-shaped, per `gateway/ARCHITECTURE.md`): `ChatRequest{Model string; Messages []Message; Temperature *float64; MaxTokens *int; Tools []ToolDef; Stream bool}`, `Message{Role string; Content string; ToolCalls []ToolCall; ToolCallID string}`, `ToolCall{ID, Name, ArgumentsJSON string}`, `ChatResponse{ID, Model string; Choices []Choice; Usage Usage}`, `Usage{PromptTokens, CompletionTokens, TotalTokens int}`.
- [ ] Define the `Adapter` interface: `type Adapter interface { ToProvider(ChatRequest) (any, error); FromProvider(any) (ChatResponse, error); Name() string }`.

**Verify:** `cd gateway && go build ./...` — compiles with no adapters registered yet.

### Task 2: OpenAI Adapter (near-identity)

**Files:**
- Create: `gateway/internal/adapter/openai/openai.go`
- Create: `gateway/internal/adapter/openai/openai_test.go`

**Steps:**
- [ ] Implement `ToProvider`/`FromProvider` as near-identity transforms (the canonical schema already matches OpenAI's shape) — but do the field mapping explicitly, not via a raw type-cast, so the seam is real even though the transform is simple.
- [ ] Round-trip test: build a `ChatRequest` with a tool call, `ToProvider` → `FromProvider`, assert equality with the original (modulo fields OpenAI doesn't echo back, like `Stream`).

**Verify:** `go test ./internal/adapter/openai/...`

### Task 3: Anthropic Adapter (real translation)

**Files:**
- Create: `gateway/internal/adapter/anthropic/anthropic.go`
- Create: `gateway/internal/adapter/anthropic/anthropic_test.go`

**Steps:**
- [ ] Implement the two documented hazards for real: system-prompt placement (canonical's in-array `role:"system"` message must be pulled out into Anthropic's top-level `system` field, per `gateway/ARCHITECTURE.md` §"Canonical Schema & Provider Adapters" point 2), and tool-call argument shape (canonical carries `ArgumentsJSON` as a string; Anthropic's native shape is an already-parsed object — `FromProvider` must marshal it back to a string so the canonical type stays consistent across providers).
- [ ] Round-trip test: a request with a system message AND a tool call must survive `ToProvider` → `FromProvider` with the system content preserved and the tool-call arguments still valid JSON on the way back.

**Verify:** `go test ./internal/adapter/anthropic/...`

### Task 4: Stubbed Adapters

**Files:**
- Create: `gateway/internal/adapter/gemini/gemini.go`
- Create: `gateway/internal/adapter/bedrock/bedrock.go`
- Create: `gateway/internal/adapter/openaicompat/openaicompat.go`

**Steps:**
- [ ] Each implements the `Adapter` interface; both methods return `fmt.Errorf("adapter %q not implemented (scaffolding pass, see docs/rfcs/2026-09-02-initial-code-scaffolding.md)", name)`.
- [ ] `openaicompat` is the one exception worth a real implementation later first (self-hosted vLLM/Ollama speak the OpenAI shape natively per `gateway/ARCHITECTURE.md`) — noted in a comment, not implemented this pass.

**Verify:** `go build ./...` still compiles with all five adapters registered in a lookup map.

### Task 5: Cache Interface + L1 In-Process Adapter

**Files:**
- Create: `gateway/internal/cache/port.go`
- Create: `gateway/internal/cache/inprocess/inprocess.go`
- Create: `gateway/internal/cache/inprocess/inprocess_test.go`
- Create (stub, unimplemented): `gateway/internal/cache/grpcserver/grpcserver.go`, `gateway/internal/cache/grpcclient/grpcclient.go`

**Interfaces:**
- Consumes: nothing outside `cache`'s own package (per `AGENTS.md`'s dependency rule — cache never imports `provideradapter`).
- Produces: `cache.Cache` interface, consumed by the dataplane pipeline in Task 8.

**Steps:**
- [ ] `port.go`: `type Cache interface { Get(ctx context.Context, key string) (resp []byte, ok bool, err error); Put(ctx context.Context, key string, resp []byte, ttl time.Duration) error }`. Value objects only — no pointer into anything outside this package, per `docs/decisions/0002-cache-embedded-in-gateway.md`.
- [ ] `inprocess/inprocess.go`: a mutex-protected `map[string]cacheEntry{data []byte; expiresAt time.Time}`, exact-match only (L1) — this pass does not implement L2 normalized-match or L3 semantic (those are `PRD.md` Phase 1, and L3 specifically must never ship without the entity/freshness hard-gate — do not build a partial L3).
- [ ] Cache key = `sha256(model + serialized-messages + temperature + max_tokens)` — a real exact-match key fabricator, not a placeholder.
- [ ] Test: put then get returns the same bytes; get after TTL expiry returns `ok=false`; get of a never-set key returns `ok=false, err=nil`.
- [ ] `grpcserver.go`/`grpcclient.go`: package + type declarations implementing `cache.Cache`'s shape are present, but every method returns `errors.New("not implemented — dormant extraction seam, see docs/decisions/0002-cache-embedded-in-gateway.md")`. This is the seam, not a working feature.

**Verify:** `go test ./internal/cache/...`

### Task 6: Identity (Single Static Virtual Key)

**Files:**
- Create: `gateway/internal/identity/identity.go`

**Steps:**
- [ ] A single configured API key (loaded from config, never hardcoded) checked via constant-time comparison (`crypto/subtle.ConstantTimeCompare`) against the `Authorization: Bearer <key>` header — real, minimal, explicitly not the full team/budget/hierarchical-scope model described in `gateway/ARCHITECTURE.md`'s Data Model (that's Phase 1).

**Verify:** unit test asserting a correct key passes, wrong key is rejected, missing header is rejected.

### Task 7: In-Memory Rate Limiter

**Files:**
- Create: `gateway/internal/ratelimit/ratelimit.go`

**Steps:**
- [ ] A single-instance, in-memory token bucket (per `PRD.md`'s explicit Phase 0 scope — Redis-backed distributed limiting is Phase 1, not this pass). Real token-bucket algorithm (refill rate + burst capacity), not a naive counter.

**Verify:** unit test — burst capacity is consumable immediately, then requests are rejected until refill.

### Task 8: Dataplane Pipeline + `cmd/gateway`

**Files:**
- Create: `gateway/internal/gateway/dataplane/dataplane.go`
- Create: `gateway/internal/gateway/controlplane/config.go`
- Create: `gateway/internal/costaccounting/costaccounting.go`
- Create: `gateway/cmd/gateway/main.go`
- Create: `gateway/config.example.yaml`

**Steps:**
- [ ] `config.go`: YAML config struct (listen address, static API key, static price table, provider credentials by env-var name — never the raw value in the file itself).
- [ ] `costaccounting.go`: `Calculate(model string, usage Usage) float64` against the static price table — float64 for this pass, `PRD.md`'s Decimal-precision requirement is a documented Phase 1 upgrade, not silently dropped.
- [ ] `dataplane.go`: the pipeline exactly as `gateway/ARCHITECTURE.md`'s Request Lifecycle describes, minus streaming (buffered only this pass) and minus guardrails/MCP (not built yet): auth (Task 6) → rate-limit (Task 7) → cache lookup (Task 5) → hit returns immediately → miss → router (round-robin across configured deployments for the request's model, single fallback to the next deployment on error) → adapter (Tasks 2-4) → upstream HTTP call → cache write-back → structured JSON log (via `log/slog`) including the cost calculation → response.
- [ ] `main.go`: load config, wire every component above, start `http.ListenAndServe`.
- [ ] `config.example.yaml`: a real example with placeholder values and a comment pointing at `SECURITY.md`'s "never commit secrets" rule.

**Verify:** `cd gateway && go build ./... && go vet ./...` — full binary builds. Manual smoke test (not automated this pass): start the binary, confirm it listens and rejects a request with no `Authorization` header.

### Task 9: Dockerfile

**Files:**
- Create: `gateway/Dockerfile`

**Steps:**
- [ ] Multi-stage build (Go builder stage → scratch/alpine final stage), matching `gateway/ARCHITECTURE.md`'s stated distribution shape (single static binary).

**Verify:** `docker build -t kelvran-gateway:scaffold gateway/` succeeds.

---

## Phase 2: Evals Skeleton (Python)

*(Independent of Phase 1 — no shared code, only the not-yet-real `api/` contract eventually connects them. Can be done in parallel with Phase 1.)*

### Task 1: Project + `EvalCase` Model

**Files:**
- Create: `evals/pyproject.toml`
- Create: `evals/evals/__init__.py`
- Create: `evals/evals/models.py`
- Create: `evals/tests/test_models.py`

**Steps:**
- [ ] `pyproject.toml`: `requires-python = ">=3.12"`, deps: `pydantic`, `click`; dev deps: `pytest`.
- [ ] `models.py`: `EvalCase(BaseModel)` with `id: str`, `revision: int`, `task_spec: dict`, `reference: str | None`, `tier: Literal["golden", "regression", "drift_sample"]`, `tags: list[str] = []` — matching `evals/ARCHITECTURE.md`'s Data Model sketch. IDs are stable, revisions are immutable once created (a `with_revision()` method returns a new instance, never mutates in place — per the org's global immutability convention).

**Verify:** `cd evals && uv sync && uv run pytest tests/test_models.py`

### Task 2: Wilson Confidence Interval

**Files:**
- Create: `evals/evals/stats.py`
- Create: `evals/tests/test_stats.py`

**Steps:**
- [ ] `wilson_interval(successes: int, total: int, confidence: float = 0.95) -> tuple[float, float]` — the actual Wilson score interval formula, not a normal approximation (normal approximation breaks down at small `n` or extreme proportions, exactly the regime a golden-tier eval set often runs in).
- [ ] Test against known reference values (e.g. 8/10 successes at 95% confidence has a well-documented expected interval) and edge cases (`total=0` raises, `successes=total` still returns a bounded upper edge below 1.0, never exactly 1.0).

**Verify:** `uv run pytest tests/test_stats.py`

### Task 3: Deterministic Scorer

**Files:**
- Create: `evals/evals/judge/__init__.py`
- Create: `evals/evals/judge/deterministic.py`
- Create: `evals/tests/test_deterministic_judge.py`

**Steps:**
- [ ] `exact_match(output: str, reference: str) -> bool` and `regex_match(output: str, pattern: str) -> bool` — real, simple, no external dependency.

**Verify:** `uv run pytest tests/test_deterministic_judge.py`

### Task 4: LLM-Judge Scorer (real prompt, mocked in tests)

**Files:**
- Create: `evals/evals/judge/llm_judge.py`
- Create: `evals/tests/test_llm_judge.py`

**Steps:**
- [ ] A real CoT-forcing judge prompt template (per `evals/ARCHITECTURE.md`'s bias-mitigation defaults: CoT-forcing, reference-guided grading) and a `judge(output, reference, call_model: Callable) -> JudgeResult` function that takes the model-calling function as a dependency-injected parameter — this is what makes it testable without a real API key: tests pass a fake `call_model` that returns a scripted response, production code passes a real provider SDK call.
- [ ] `JudgeResult` includes `rationale` (the CoT text) and `bias_mitigations_applied: list[str]`, per the Data Model's `Score.bias_mitigations_applied` field already specified in `evals/ARCHITECTURE.md`.

**Verify:** `uv run pytest tests/test_llm_judge.py` — must pass with zero network calls and zero API keys present.

### Task 5: Docker-Sandboxed Rollout Wrapper

**Files:**
- Create: `evals/evals/rollout/__init__.py`
- Create: `evals/evals/rollout/sandbox.py`
- Create: `evals/tests/test_sandbox_integration.py` (integration-tagged, skipped by default)

**Steps:**
- [ ] `run_in_sandbox(command: list[str], timeout_s: int) -> SandboxResult` — shells out to `docker run --rm --network=none <image> <command>` via `asyncio.create_subprocess_exec`, enforcing the timeout and network isolation (`--network=none`) called for in `THREAT_MODEL.md`'s Evals STRIDE table (egress allowlisting — for this pass, "no egress at all" is the honest, simplest safe default, not a partial allowlist implementation).
- [ ] The integration test is marked `@pytest.mark.integration` and skipped unless `RUN_DOCKER_TESTS=1` is set — real Docker-daemon-requiring behavior is verified, but never blocks the default `pytest` run.

**Verify:** `uv run pytest tests/` (default run, sandbox integration test skipped) and, separately, `RUN_DOCKER_TESTS=1 uv run pytest tests/test_sandbox_integration.py` (requires local Docker).

### Task 6: CLI

**Files:**
- Create: `evals/evals/cli.py`
- Update: `evals/pyproject.toml` (add `[project.scripts] evals = "evals.cli:main"`)

**Steps:**
- [ ] `evals run --suite <path>`: loads `EvalCase`s from a JSON/YAML file, runs the deterministic scorer (LLM-judge is opt-in via a flag, since it needs a real API key), prints results.
- [ ] `evals report`: given a set of results, prints pass rate **and its Wilson CI together, always** — never a bare percentage, enforcing `PRD.md`'s stated success metric in code, not just in a doc.

**Verify:** `uv run evals run --suite tests/fixtures/golden_example.json` against a small checked-in fixture with 2-3 trivial cases; confirm the CI appears in the output.

---

## Post-Implementation

- Run `find gateway evals -type f -name "*.go" -o -name "*.py" | sort` and confirm it matches this plan's file list, no drift.
- `gateway/changelog/unreleased.md` and `evals/changelog/unreleased.md` each get an "Added" entry summarizing what this scaffolding pass shipped.
- `docs/agents/LOGS.md` gets a new append-only entry.
- `STATUS.md` updates: Current Phase, Verification State (real command output), Next Action.
- `DECISIONS.md` gets one line: the Go/Python version pins actually used, since that was an Unresolved Question in the spec and is now settled by what was verified locally.
