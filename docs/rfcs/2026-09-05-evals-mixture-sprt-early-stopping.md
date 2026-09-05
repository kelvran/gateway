# RFC: Real mixture-SPRT (anytime-valid) early-stopping, replacing the two-checkpoint rule

## Status

Accepted, implemented 2026-09-05.

## Context

`docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md` shipped `two_checkpoint_early_stop`: check the running Wilson interval against `baseline_pass_rate` at exactly two pre-declared trial counts (`min_trials`, `max_trials`), Bonferroni-splitting the false-positive budget across both checks. That RFC's own Alternatives Considered section explicitly named the more rigorous fix — "Real SPRT / anytime-valid confidence sequences" — and rejected it for that pass because it "needs an operator-supplied alternative pass rate `p1`, not just a baseline... a real scope addition this backlog item never asked for." This RFC was scoped, via an explicit user brainstorming check-in (`AskUserQuestion`), to build that real fix now: "Replace two-checkpoint with true SPRT."

**Correcting the prior RFC's own stated blocker, not just implementing around it**: the "needs an operator-supplied `p1`" objection is accurate for **classical Wald SPRT** (Wald 1945), which tests one fixed null against one fixed, pre-committed alternative. It does **not** apply to the **mixture SPRT (mSPRT)** — Robbins (1970), popularized for production A/B testing by Johari, Koomen, Pekelis & Walsh (KDD 2017) — the exact paper this project's own `two_checkpoint_early_stop` docstring already cites as the authoritative source for the optional-stopping failure mode it was built to avoid. mSPRT replaces a single fixed alternative with a *mixing distribution* over possible alternatives; the test remains valid (false-positive rate ≤ `1 - confidence`, at every trial count, simultaneously, with no Bonferroni correction) for **any** choice of mixing-distribution variance — the operator needs only the same `baseline_pass_rate` this project already asks for. The mixing variance is a pure power/speed tuning knob with a well-documented sensible default (Johari et al.'s own guidance: "on the order of the null variance"), never a correctness-relevant input. The prior RFC's rejection reason simply doesn't hold for this specific, already-cited method — found by reading the actual mSPRT construction, not assumed from the general "SPRT needs an alternative" intuition that's true of Wald's original test.

## Design

### The test: one-sample mSPRT against a fixed baseline

For a repeated-trial group with `n` trials so far, `successes` passes, observed rate `p̂ = successes / n`, and operator-supplied `baseline_pass_rate = θ₀`:

```
σ² = θ₀(1 - θ₀)                          # fixed, KNOWN null-hypothesis Bernoulli variance
τ² = relative_mixing_variance × σ²       # mixing-distribution variance; tuning-only, never
                                          # affects the false-positive guarantee (see below)
Λₙ = √(σ² / (σ² + nτ²)) × exp( n²τ²(p̂ - θ₀)² / (2σ²(σ² + nτ²)) )

stop (reject the null) when Λₙ ≥ 1 / (1 - confidence)
```

This is the standard closed-form mSPRT likelihood ratio for Bernoulli/normal-approximated data under a `N(0, τ²)` mixing prior over the effect `p̂ - θ₀` (the same construction Johari et al.'s paper and the widely-cited `mixtureSPRT` R package implement for two-sample A/B tests), reduced to the one-sample case: the "difference between two streams" `μ_A - μ_B` becomes `p̂ - θ₀`, and the "pooled variance" `ν_A + ν_B` becomes the single stream's own variance.

By Ville's inequality, `Λₙ` is a nonnegative martingale under the null, so `P(∃n : Λₙ ≥ 1/α) ≤ α` — the false-positive-rate guarantee holds **simultaneously at every trial count**, checked after every single trial, with no fixed checkpoints and no Bonferroni correction. This is the actual property `two_checkpoint_early_stop`'s own Bonferroni workaround was standing in for.

### A deliberate choice: fixed null variance, not a running estimate — for EXACT, not just asymptotic, validity

`σ² = θ₀(1 - θ₀)` is fixed at the operator-supplied baseline, never re-estimated from the group's own running data (`p̂(1 - p̂)`). This keeps the martingale property **exact**, not asymptotic: a variant using an online-estimated variance (common in some mSPRT implementations, e.g. a Welford-updated running variance) only satisfies Ville's inequality approximately, and independent implementers of that variant document a real, measurable consequence — meaningfully inflated empirical Type-I error at low sample counts unless a substantial warmup period (order 100 trials) is used before the test is trusted. This project has no such warmup budget to spend (the whole point of early-stopping is cost savings on small-to-moderate trial counts), so the fixed-variance form is the correct choice, not a simplification made for convenience.

### `relative_mixing_variance`: a real parameter, but not a correctness-relevant one

Exposed as an optional tuning knob (default `1.0`, i.e. `τ² = σ²` — Johari et al.'s own stated rule of thumb: "best if `τ²` is on the order of `σ²`"). Unlike the old design's `min_trials`/`max_trials` checkpoint placement (which directly determined the Bonferroni split and therefore the actual false-positive rate at each check), `relative_mixing_variance` provably never affects the `≤ 1 - confidence` guarantee for any positive value — it only trades off how many trials are needed to detect a real deviation (a smaller value detects large, obvious deviations faster but is slower on subtle ones; a larger value is the reverse). This is named explicitly so a future operator tuning it for cost/power reasons knows they are adjusting efficiency, not correctness.

### API changes (breaking, deliberately — `evals` has no PyPI release yet)

- `evals.stats.two_checkpoint_early_stop` → `evals.stats.mixture_sprt_early_stop(successes, trials_run, baseline_pass_rate, confidence=0.95, relative_mixing_variance=1.0) -> bool`. Pure, stateless, one call per trial — the caller (`scheduler.run_suite`) still owns the running tally, exactly as before.
- `EarlyStopConfig.min_trials` is **removed** — mSPRT needs no floor before its first valid check; every trial from the first is a real, valid checkpoint. `max_trials` is **kept**, but its meaning changes: it is now a pure **resource/cost ceiling** (force-stop this group at this many trials regardless of the SPRT's own decision), never a second statistical checkpoint — removing it would not reintroduce the old design's flaw (mSPRT has no "checkpoint placement" concept to get wrong), but an unconditional per-group trial cap is still real, deliberate cost control this project has consistently valued (per the sibling result-cache/Score-cache RFCs' own cost-control framing).
- `EarlyStopConfig.relative_mixing_variance: float = 1.0` added.
- CLI (`evals rollout`): `--early-stop-min-trials` **removed**; `--early-stop-max-trials`/`--early-stop-baseline-pass-rate` kept; `--early-stop-relative-mixing-variance` added (optional, defaults to `1.0`).
- `run_suite`'s own group-tallying/skip-remaining-trials logic in `scheduler.py` is otherwise unchanged — it already treats "the group stopped" as an opaque boolean signal from `_stop_after`, so swapping the statistical test underneath is a self-contained change to that one function and `EarlyStopConfig`'s field set, not a scheduler rewrite.

## Alternatives considered

**Classical Wald SPRT** (Wald 1945) — rejected, same reason the prior RFC gave: needs a committed alternative `p1`, which this project has no calibrated way to supply (no production traffic yet, per that RFC's own repeatedly-stated posture).

**Nonparametric time-uniform confidence sequences** (Howard, Ramdas, McAuliffe & Sekhon 2021) — a more general, distribution-free family that would also solve this problem, and arguably the more "modern default" recommendation in current literature for cases needing distribution-free guarantees. Rejected here in favor of mSPRT specifically because: (a) this project's own code already cites the exact paper (Johari et al. 2017) that popularized mSPRT for this exact use case, so mSPRT is the more directly-grounded choice, not a new citation; (b) the data here is genuinely Bernoulli (pass/fail), not an arbitrary bounded random variable, so mSPRT's parametric assumption is actually satisfied exactly, not just approximately — the general nonparametric machinery's extra robustness (built for distributions where the parametric form is unknown or untrusted) buys nothing extra here and costs real implementation complexity (Howard et al.'s closed forms are more involved and less widely worked out for the plain one-sample-Bernoulli-vs-fixed-baseline case specifically, most of the literature and worked examples targeting two-sample A/B effect sizes instead).

**Running/estimated variance instead of the fixed null variance** — rejected; see "A deliberate choice" above. Exact validity at zero warmup cost is worth more here than the (small) efficiency gain from using the group's own observed variance instead of the null's.

**Keeping `min_trials` as a soft floor "for safety"** — rejected: it would silently suggest the test is somehow *less* valid before that floor, which is false — mSPRT's guarantee holds at `n=1` exactly as it does at `n=1000`. Keeping a parameter whose only effect is to delay a provably-valid check would be conservatism with no real statistical justification, the same kind of unearned caution this RFC exists to remove.

## Verification

`evals/tests/test_stats.py`: closed-form correctness tests (known inputs → hand-computed `Λₙ`), boundary/validation tests (baseline/confidence must be in the open interval `(0, 1)`, `relative_mixing_variance > 0`, `trials_run == 0` never stops). The load-bearing proof, mirroring the two-checkpoint design's own prior verification convention exactly: a Monte Carlo simulation under the true null (`baseline_pass_rate` trials, i.i.d. Bernoulli, checked after *every* trial up to a real cap, across many independent simulated groups) asserting the empirical false-stop rate stays at or below the nominal `1 - confidence`, and a second simulation under a real alternative pass rate proving the test has real power to detect a genuine deviation before `max_trials`. `evals/tests/test_scheduler.py`/`test_cli_integration.py`: updated for the new `EarlyStopConfig` field set and CLI flags; the existing "never called before a stop decision, never double-scores" tests carry over structurally unchanged since `run_suite`'s own control flow around `_stop_after` didn't change. Sanity-checked by temporarily reverting the fixed null variance to a naive running estimate and re-running the null-simulation test at low trial counts, confirming the empirical false-positive rate measurably exceeds the nominal level (the exact inflation risk this RFC's design section names), then restoring.
