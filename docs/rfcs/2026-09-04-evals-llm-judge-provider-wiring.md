- **Status**: accepted
- **Date**: 2026-09-04
- **Author(s)**: project founder + Claude Code

## Summary

Wire `evals/evals/judge/llm_judge.py`'s `judge()` to a real, single-provider (Anthropic-only) SDK call, exposed through a new `--llm-judge` flag on both `evals run` and `evals rollout`. `judge()` itself is untouched — it already takes `call_model` as a dependency-injected callable specifically so a real implementation could be added later without a signature change. This RFC writes that real implementation (`evals/evals/judge/providers.py`), wires it into both CLI commands, and gates its own tests behind a new `RUN_LIVE_LLM_TESTS=1` environment variable, mirroring the exact `RUN_DOCKER_TESTS=1` convention this repo already established for `evals/evals/rollout/sandbox.py`.

## Motivation

Confirmed directly against the live tree, not assumed: `evals/evals/judge/llm_judge.py:86-89`'s `judge()` signature is unchanged since it was scaffolded — `call_model: Callable[[str], Awaitable[str]]` remains required and un-defaulted, and the module's only imports are `re`, `collections.abc`, and `pydantic` (zero SDK code). `evals/pyproject.toml:7-11`'s dependencies are exactly `pydantic`, `click`, `protobuf` — no `anthropic`/`openai` anywhere in the `evals/` source tree (confirmed via a repo-wide grep). `evals/evals/cli.py`'s `run_cmd` (`--llm-judge` flag, lines 114-135) unconditionally raises `click.ClickException` before ever touching `judge()`; `rollout_cmd` (the command `docs/rfcs/2026-09-04-evals-rollout-scheduler.md` just shipped) has no `--llm-judge` flag at all. `judge()`'s only callers anywhere in the repo are its own test file.

This gap is qualitatively different today than it would have been before the Rollout Scheduler shipped. Before that RFC, the only "output" anywhere in `evals/` was `task_spec.output` — a string baked into a suite's fixture file by whoever wrote it, never produced by real execution. Wiring `judge()` to a live SDK against a fixture author's own pre-written string would have graded a fairly hollow question — exactly why the Rollout Scheduler RFC's own Alternatives Considered declined to build this at the same time, naming it instead as "the natural, smaller follow-up... once this scheduler exists to call it." Now that `run_suite()`/`Run.stdout` are real (Docker-captured output from a real, live execution), `judge(output=run.stdout, reference=case.reference, ...)` grades something that actually happened — this RFC closes that follow-up.

Picked over the other open candidate assessed in the same grounding pass — fixing `THREAT_MODEL.md`'s Evals STRIDE-table gap (Denial of Service/Information Disclosure/Elevation of Privilege rows claiming unbuilt mitigations) — because that gap, once actually sized, does not fit one bounded RFC the way this one does. It splits into a near-zero-cost doc correction (no new code) and a 3-to-5-RFC security-hardening program (a real network-egress-allowlist proxy, a real credential-broker proxy, a sandbox-runtime-isolation upgrade to gVisor/Firecracker-class isolation, a separate CI-cost-gating concern, and an audit-trail piece blocked on `api/otel`'s still-undecided transport) — each independently a full subsystem per real production precedent (Docker Sandboxes, E2B, Modal, OpenAI's Codex sandbox all ship these as separate product surfaces, not one config flag). The original "Guardrails-sized" estimate for that gap, from this RFC's own predecessor, undersold how blocked most of it actually is and oversold how much work the honestly-buildable slice needs. That doc-correction is recommended as a fast, separate, near-zero-cost follow-up pass — not bundled here, to keep this RFC's blast radius single-purpose.

## Detailed Design

### Grounding

Grounded via a dynamic-workflow research pass (task `wusbd9y3q`, run `wf_348d608d-e87`): 4 parallel angles (a direct code-audit confirming the LLM-judge-wiring gap's exact current shape, a full sizing/scope audit of the THREAT_MODEL.md Evals gap against real code and real production sandboxing precedent, a fresh whole-repo doc-vs-code gap rescan, and an industry-precedent check for both candidates) plus a synthesis that independently re-read every load-bearing file/line cited before trusting it (`judge()`'s signature, `cli.py`'s two command bodies, `pyproject.toml`'s dependency list, `THREAT_MODEL.md`'s exact row wording, `PRD.md`'s exact scope lines, and — for the whole-repo rescan's own two new findings — directly confirming no `internal/router` directory exists anywhere in `gateway/` and no `go-arch-lint` step exists in `.github/workflows/ci.yml`, both real, previously-unflagged `gateway/ARCHITECTURE.md` overclaims left for a future pass, not fixed here since they're gateway-side and unrelated to this RFC's scope).

### v1 scope: one provider, one call site pattern reused twice, one new CI-safety gate

**SDK choice: Anthropic only, Haiku-tier, for v1.** Grounded directly in this repo's own stated constraints, not a generic "pick one" call:
- `PRD.md`'s Scope section (line 28) already pre-authorizes exactly this: *"deterministic + single LLM-judge scoring at v1, with the interface for a multi-judge skeptic panel designed in from the start even though the panel itself is a v2 feature."* One provider, one call, matches "single."
- `THREAT_MODEL.md`'s Evals Tampering row lists adversarial skeptic-panel verification as its own v2 upgrade path — a second/cross-vendor SDK's main justification (cross-vendor adversarial checking) is explicitly a v2 concern, not v1.
- Two SDKs would mean two secrets (`ANTHROPIC_API_KEY` + `OPENAI_API_KEY`) in every environment that ever runs `--llm-judge`, doubling the new-secret surface for zero v1 requirement.
- `judge()`'s `call_model` injection already makes the design provider-agnostic — adding a second provider later (mirroring `gateway/internal/adapter`'s own multi-provider split as precedent) is a same-shaped follow-on function in `providers.py`, not a signature change to anything.

Default judge model: `claude-haiku-4-5-20251001` — a pinned, dated snapshot id (never a bare alias like `claude-haiku-4-5`), so the judge model never silently drifts underneath a suite of results without a deliberate, reviewable bump — the same "bumped by hand on a deliberate release" discipline `gateway/internal/guardrail`'s `guardrailPolicyVersion` constant already established for this repo.

### `evals/evals/judge/providers.py` (new) — the sole SDK import site

```python
from anthropic import AsyncAnthropic

_DEFAULT_JUDGE_MODEL = "claude-haiku-4-5-20251001"

def make_anthropic_call_model(
    model: str = _DEFAULT_JUDGE_MODEL,
    client: AsyncAnthropic | None = None,
) -> Callable[[str], Awaitable[str]]:
    """Build a call_model closure backed by a real Anthropic API call.

    The client is constructed lazily — inside this function, not at module
    import time — matching the Anthropic SDK's own default behavior
    (ANTHROPIC_API_KEY is read at AsyncAnthropic() construction, never at
    import). Importing this module never requires a key; only calling this
    factory (or the returned closure) does.
    """
    anthropic_client = client or AsyncAnthropic()

    async def call_model(prompt: str) -> str:
        response = await anthropic_client.messages.create(
            model=model,
            max_tokens=1024,
            messages=[{"role": "user", "content": prompt}],
        )
        return response.content[0].text

    return call_model
```

`evals/evals/judge/llm_judge.py` is **not modified at all** — its "zero network calls, testable without a live provider API key" property, stated in its own module docstring, remains true forever. `providers.py` is the only file in `evals/` that imports `anthropic`. `evals/pyproject.toml` gains `anthropic` in `dependencies` (a real runtime dependency of the CLI path, not `dependency-groups.dev` — the same tier `pydantic`/`click`/`protobuf` already sit in).

### CLI wiring

**`run_cmd`** (`evals/evals/cli.py:114-135` today): replace the unconditional `click.ClickException` stub with a real branch. When `--llm-judge` is set, build `call_model = make_anthropic_call_model()` once per invocation, then score each case sequentially (no `asyncio.gather` — matches `evals/evals/rollout/scheduler.py`'s own stated v1 sequential posture, not a shortcut taken only here) via `judge(output=case.task_spec["output"], reference=case.reference, call_model=call_model)`. `case.reference` being `None` raises a clear `click.ClickException` before any call is made — `judge()`'s `reference` parameter is a required `str`, not `Optional`, so this must be checked explicitly rather than left to fail inside the SDK call, mirroring `_score_output_deterministic`'s existing exact-match-requires-a-reference check. Same `PASS`/`FAIL`-per-case-line + `format_report` output shape as the deterministic path.

**`rollout_cmd`** (`evals/evals/cli.py:150-189` today, no `--llm-judge` flag exists): gains the identical flag. When set, score each `run.status == "completed"` `Run`'s captured `stdout` via `judge(output=run.stdout.strip(), reference=case.reference, call_model=...)` instead of `_score_run_deterministic` — a `Run` that didn't complete (timed out or errored) is still never judged, exactly like the deterministic path's existing `if run.status != "completed": return False` rule.

**Judge-call failure semantics** (an open question this RFC resolves decisively, not left dangling): a `judge()` call that raises (a malformed response per `_parse_judge_response`'s existing `ValueError`, or a real SDK error — auth failure, rate limit, timeout) marks that one case as a judged failure (prints `{case.id}: JUDGE_ERROR` and does not increment `successes`) and continues to the next case, rather than aborting the whole invocation. This mirrors the exact precedent `evals/evals/rollout/scheduler.py`'s `run_suite` already established for sandbox-launch failures: "a single case's failure never aborts the suite." A case still counts toward `total` in the Wilson-CI denominator — it was attempted, not skipped.

### CI-safety mechanism

New `evals/tests/test_llm_judge_integration.py`, same shape as `evals/tests/test_sandbox_integration.py`:

```python
pytestmark = [
    pytest.mark.llm_integration,
    pytest.mark.skipif(
        os.environ.get("RUN_LIVE_LLM_TESTS") != "1",
        reason="requires a live Anthropic API key; set RUN_LIVE_LLM_TESTS=1 to run",
    ),
]
```

A new, separate `llm_integration` marker registered in `evals/pyproject.toml`'s `[tool.pytest.ini_options] markers` list, alongside the existing `integration` entry — and a **new, separate env var** from `RUN_DOCKER_TESTS`, deliberately not reused: they gate two different live dependencies (a paid provider API key/secret vs. a local Docker daemon). Conflating them would either make a Docker-only test run spend real API money, or make an API-key-only run silently skip Docker tests for the wrong reason. Default `uv run pytest tests/` / root `make verify` never sets `RUN_LIVE_LLM_TESTS`, so CI never needs `ANTHROPIC_API_KEY` to exist as a secret to pass — the identical safety property the `RUN_DOCKER_TESTS=1` convention already delivers, applied to a second real dependency. This exact "env-var-gated, skip-by-default integration test" pattern is also the industry-standard shape for this problem, not a novel design: DeepEval, Ragas, and LangChain's own testing docs all converge on the same marker-plus-skip-on-missing-secret idiom.

### Doc correction bundled with this RFC

`evals/ARCHITECTURE.md`'s Tech Stack table's `Judge SDKs` row currently reads *"Not yet wired: `judge()`'s real CoT-forcing/reference-guided prompt logic exists..., but takes `call_model` as an injected callable with zero SDK code behind it..."* — flipped to name the real, wired state this RFC ships, the same "doc correction bundled with the RFC" pattern Guardrails' own RFC applied to `gateway/ARCHITECTURE.md:136` and the Rollout Scheduler RFC applied to this same table's `Statistics` row.

## Drawbacks

- `evals`' first-ever live-secret dependency: `ANTHROPIC_API_KEY` must be present in any environment that actually runs `--llm-judge` (not in default `make verify`/CI, which never sets `RUN_LIVE_LLM_TESTS`). This is a real, new operational surface, not free.
- `evals`' first source of test/run nondeterminism when `--llm-judge` is used — a real model call is not perfectly reproducible run-to-run, unlike every other scorer this repo has shipped so far (deterministic exact/regex match, Wilson CI's closed-form math). Accepted as inherent to what an LLM judge is, not a defect to fix here.
- `Run.cost_usd` stays exactly `None` for a rollout scored with `--llm-judge`, same as without it — deliberately not extended to track judge cost. `Run` represents the cost of *producing* an output (a sandboxed execution, no billed call in v1's harness); the judge call is a separate, scoring-time cost with a different lifecycle. Tracking it belongs to a future `Score` record (the still-unbuilt `evals/ARCHITECTURE.md` Data Model entity, already independently surfaced as the next-ready-now candidate by this RFC's own grounding research), not to `Run`. Stated plainly here so the decision reads as deliberate, not an oversight.
- Judge non-determinism plus a real API cost means `--llm-judge` genuinely cannot be the default scoring path for CI-gated regression checks without careful throttling/cost-budgeting — out of scope for this RFC, left as a real, named open question.

## Alternatives Considered

1. **Wire both Anthropic and OpenAI SDKs in this pass** — rejected: doubles the new-secret surface for a v1 whose own `PRD.md` scope line explicitly asks for "single LLM-judge scoring," not a panel; a second provider is a same-shaped, independent follow-on function later, not blocked by this RFC's choice.
2. **Pick the THREAT_MODEL.md Evals STRIDE-table gap instead** — rejected for this pass, per Motivation above: once sized against real production sandboxing precedent, it is not one bounded RFC — it is a near-zero-cost doc correction plus 3-to-5 independently subsystem-sized security-hardening RFCs, several gated on undecided architectural prerequisites (a tool-calling harness concept that doesn't exist anywhere in `evals/`; the Sandbox Pool/concurrency layer; `api/otel`'s still-undecided transport). The doc-correction slice is recommended as a fast, separate follow-up, not bundled here.
3. **Persist judge results as a `Score` record in the same pass** — rejected: `Score` is a clean, independently-scoped, zero-infra-dependent next feature (the last remaining unbuilt entity in `evals/ARCHITECTURE.md`'s Data Model sketch), but bundling it here would blur two RFCs' blast radius into one. Named explicitly as the natural next-next pick.
4. **Gate the new integration test behind the existing `RUN_DOCKER_TESTS` variable** — rejected: it gates a different live dependency (a paid API key/secret vs. a local Docker daemon); reusing it would either spend real API money on Docker-only test runs or silently skip live-LLM tests for the wrong stated reason. A new, distinct `RUN_LIVE_LLM_TESTS` variable keeps each gate's meaning honest.
5. **Concurrent (`asyncio.gather`) judge calls across a suite** — rejected for v1: matches `evals/evals/rollout/scheduler.py`'s own already-stated sequential-only v1 posture; no rollout/judge volume yet exists to justify the added complexity, and concurrent real API calls multiply the cost-and-rate-limit surface for no proven need.

## Unresolved Questions

- Whether/how a future nightly or cost-budgeted CI job should exercise `--llm-judge` for real on a schedule, rather than remaining permanently opt-in-only — deliberately not decided here, the same "gated on real need, not a fixed timeline" posture this project has applied to every other deferred item (a real embedding-based L3, Guardrails' own ML-moderation tier).
- Whether a second provider (OpenAI) is ever worth adding, and under what trigger — not designed here; `call_model`'s existing DI shape means it's a same-shaped follow-on, not a blocked one.
- The `Score` model + persistence (this RFC's own Alternatives Considered #3) is the natural next-next pick, but its exact shape (whether it should record a judge call's real cost, closing this RFC's own Drawbacks item about `Run.cost_usd` staying `None`) is left to that future RFC.
- The `THREAT_MODEL.md` Evals STRIDE-table doc correction, and the further split of its full five-layer vision into 3-to-5 separately-scoped future RFCs — named here, not designed here.

## Research Trail

Grounded via a dynamic-workflow research pass (task `wusbd9y3q` / run `wf_348d608d-e87`): 4 parallel angles (llm-judge-wiring code-audit + RFC-seed design; THREAT_MODEL.md Evals-gap sizing against real sandbox code and Sandbox Tiering docs; a fresh whole-repo doc-vs-code gap rescan; and an industry-precedent sizing check for both candidates, citing DeepEval/Ragas/LangChain/promptfoo's real-provider-test-gating conventions and Docker Sandboxes/E2B/Modal/OpenAI Codex's real sandbox-hardening architectures) plus a synthesis. The synthesis independently re-read every load-bearing file/line cited — `judge()`'s exact signature and docstring, `cli.py`'s two command bodies, `pyproject.toml`'s dependency list, `THREAT_MODEL.md`'s exact row wording, `PRD.md`'s exact scope lines — and independently confirmed the whole-repo rescan's two new, unrelated findings (no `internal/router` directory exists in `gateway/`, contradicting `gateway/ARCHITECTURE.md`'s Package Layout prose; no `go-arch-lint` CI step exists anywhere, contradicting that same document's Dependency Direction Rules claim) before setting them aside as out of this RFC's scope. One nuance the synthesis resolved rather than treated as a contradiction: two research passes appeared to disagree on how "cheap" sandbox hardening is, but were in fact scoping different things — one's "cheap slice" explicitly excluded network-egress allowlisting (calling it "pure YAGNI" given no current egress need), while the other was sizing the *full* five-layer vision, where it agrees allowlisting alone is subsystem-sized; both converge on "doc-fix-now, 3-to-5 separate future RFCs for the rest."
