"""Scorers: deterministic (exact/regex match) and LLM-judge.

See evals/ARCHITECTURE.md's Rollout Lifecycle — the Scorer Service runs
deterministic checks before any LLM-judge call, per docs/testing/TESTING.md
§10's cost-blowout mitigation (cheap deterministic checks run first).
"""
