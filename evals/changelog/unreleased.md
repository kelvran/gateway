# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `evals` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.1.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) by default. Revisit CalVer (`YYYY.MM.PATCH`) once `evals` ships continuously without hard breaking changes — see `RELEASE.md`.

## Added

- Initial code skeleton per `docs/plans/2026-09-02-initial-code-scaffolding.md`: `EvalCase` pydantic model (immutable, versioned revisions); a real Wilson confidence-interval calculator; a deterministic (exact-match/regex) scorer; an LLM-as-judge scorer with a CoT-forcing prompt template, dependency-injected `call_model` so tests never require a live API key; a Docker-sandboxed rollout wrapper (`run_in_sandbox`, network-isolated by default) with an integration-tagged, skip-by-default test suite; a CLI (`evals run`, `evals report`) that always prints a pass rate alongside its Wilson CI, never a bare percentage. Rollout scheduling, trace collection, and the full CI/CD gate tiers are explicitly deferred — see `docs/rfcs/2026-09-02-initial-code-scaffolding.md`.
- Deepened test suite: end-to-end `evals run`/`evals report` CLI coverage via Click's `CliRunner`; Hypothesis property-based regression tests pinning `wilson_interval`'s mathematical invariants; a golden-fixture test locking the exact LLM-judge prompt text; a non-Docker regression test for `run_in_sandbox`'s missing-`docker`-binary error path. Wired a real `[tool.ruff]` config (E/F/I/UP/B) and fixed the small pre-existing findings it surfaced.
- `evals/ingestion/` — real, v1 (decode-only), per `docs/rfcs/2026-09-03-api-gatewayevents-contract.md`: `decode_gateway_decision_event` decodes a `gatewayevents.v1.GatewayDecisionEvent` via the newly-real generated Python bindings (`evals/contracts/gatewayevents/v1/`, `google-protobuf` added as a runtime dependency — not `betterproto`, real ownership churn). `tests/test_ingestion_golden_roundtrip.py` is the golden-fixture round-trip test `docs/testing/TESTING.md` §5 already promised, made real: the fixture (`tests/fixtures/gateway_decision_event.json`) was produced by `gateway`'s own real generated Go bindings, not hand-authored, so this proves genuine cross-language wire-format agreement. Live production-trace sampling remains explicitly deferred — the transport (queue vs. periodic file export vs. push endpoint) is still undecided. Generated code lives at `evals/contracts/` and is excluded from `ruff` (machine-written, not ruff-clean by construction, unlike Go's equivalent).

## Changed

## Deprecated

## Removed

## Fixed

- `pyproject.toml`'s PyPI distribution name was `evals`, not the `kelvran-evals` name `RELEASE.md`'s Publish Targets table already committed to — found while prepping for the first real release. Fixed the distribution name only; the importable module (`evals`) and the installed CLI command (`evals`) are unchanged, matching the standard "distribution name differs from import name" PyPI pattern.

## Security
