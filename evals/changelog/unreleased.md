# Unreleased

Entries accumulate here under the six [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories until the next `evals` release. At release time this file's content is moved into a new dated `<version>.md` file (e.g. `0.1.0.md`) in this same folder, and this file is reset to empty category headers.

Versioning: [SemVer](https://semver.org/) by default. Revisit CalVer (`YYYY.MM.PATCH`) once `evals` ships continuously without hard breaking changes — see `RELEASE.md`.

## Added

- Initial code skeleton per `docs/plans/2026-09-02-initial-code-scaffolding.md`: `EvalCase` pydantic model (immutable, versioned revisions); a real Wilson confidence-interval calculator; a deterministic (exact-match/regex) scorer; an LLM-as-judge scorer with a CoT-forcing prompt template, dependency-injected `call_model` so tests never require a live API key; a Docker-sandboxed rollout wrapper (`run_in_sandbox`, network-isolated by default) with an integration-tagged, skip-by-default test suite; a CLI (`evals run`, `evals report`) that always prints a pass rate alongside its Wilson CI, never a bare percentage. Rollout scheduling, trace collection, and the full CI/CD gate tiers are explicitly deferred — see `docs/rfcs/2026-09-02-initial-code-scaffolding.md`.
- Deepened test suite: end-to-end `evals run`/`evals report` CLI coverage via Click's `CliRunner`; Hypothesis property-based regression tests pinning `wilson_interval`'s mathematical invariants; a golden-fixture test locking the exact LLM-judge prompt text; a non-Docker regression test for `run_in_sandbox`'s missing-`docker`-binary error path. Wired a real `[tool.ruff]` config (E/F/I/UP/B) and fixed the small pre-existing findings it surfaced.

## Changed

## Deprecated

## Removed

## Fixed

## Security
