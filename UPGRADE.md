# Upgrade Guide

No breaking change has shipped in a release yet — `gateway/v0.1.0` and `evals/v0.1.0` are real, tagged releases with none. (A genuinely breaking `evals` CLI change — `--scores` becoming a required option on `run`/`rollout` — has landed on `main` since that tag, but not yet in a release; it belongs here once `evals` cuts the version that actually ships it, per `evals/changelog/unreleased.md`'s own "moves into a dated version file at release time" convention.)

When the first breaking change lands, record it here as a row in this table (oldest first), and cross-reference the specific `<version>.md` changelog entry that introduced it:

| Version | Breaking Change | Migration Steps |
|---|---|---|
| — | — | — |

Config-schema migration notes (virtual keys, budgets, routing rules, cache configuration) will be tracked here as they evolve, once there's a config schema to have opinions about.
