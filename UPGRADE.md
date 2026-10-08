# Upgrade Guide

**Corrected 2026-10-08**: this file said "no breaking change has shipped in a release yet" while only `gateway/v0.1.0` and `evals/v0.1.0` existed. Twenty gateway and eleven evals releases later (`gateway/v0.17.0`, `evals/v0.10.1`), the gateway has still shipped no breaking change to a released surface (every `api/` change passed `buf breaking`; config keys were only added), but the evals CLI change this file itself flagged as pending did ship — it is the first row below. The support window and what counts as a public surface are being formalised in `docs/VERSIONING.md` (in progress per `docs/upgrade-research/kelvran-deep-research-round4-discoverability-2026-10-08.md`); until then the rule is: a `**BREAKING**` changelog entry must have a row here in the same release.

Rows are oldest first and cross-reference the `<deployable>/changelog/<version>.md` entry that introduced the change:

| Version | Breaking Change | Migration Steps |
|---|---|---|
| `evals/v0.2.0` | `evals run` and `evals rollout` require `--scores <path>` (persisted `Score` JSONL); in `evals/v0.1.0` scores were only printed. Introduced by commit `d3e54197` (2026-09-04, "add Score model + persistence"), first shipped in `evals/v0.2.0` (`evals/changelog/0.2.0.md`). | Add `--scores /path/to/scores.jsonl` to every `evals run`/`evals rollout` invocation; feed the same path to `evals report --scores`. |

Config-schema migration notes (virtual keys, budgets, routing rules, cache configuration) will be tracked here as they evolve, once there's a config schema to have opinions about.
