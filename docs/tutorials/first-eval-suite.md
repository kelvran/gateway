# Your first eval suite

This tutorial takes you from a fresh checkout to a working CI/CD quality gate with the Kelvran `evals` CLI. You install the CLI, write a three-case suite, score it with the deterministic scorer, read the Wilson confidence interval, and make `evals report` fail when the lower bound of that interval drops below a threshold. It is for a developer who has cloned this repository and has not used `evals` before. It needs no API keys, no Docker, and no running gateway.

The page matches the `evals` CLI on `main` as of 2026-10-08, whose [`evals/pyproject.toml`](../../evals/pyproject.toml) records `version = "0.10.1"`. The `evals/v0.10.1` tag ships the same `evals/evals/cli.py` but declares `version = "0.8.0"` (the bump was missed at release time and fixed on `main`; see [`evals/changelog/unreleased.md`](../../evals/changelog/unreleased.md)), so a checkout of that tag reports `0.8.0` in step 2.

## What you build

By the end you have:

- a suite file with three `EvalCase` objects, one of which fails on purpose;
- a JSONL file of persisted `Score` lines, one per case;
- a report line that reads `pass_rate=0.6667 (2/3) 95% CI=[0.2077, 0.9385]`;
- a `--fail-under` gate that exits non-zero when the Wilson lower bound is below your threshold;
- the same tiered, tagged gate that this repository's own CI runs on every push to `main` and every pull request.

## Before you start

You need:

- Python 3.12 or newer. `evals/pyproject.toml` declares `requires-python = ">=3.12"`.
- `uv`. Everything under `evals/` is managed with `uv`; CI installs with the same `uv sync` you run below.
- A local checkout of the Kelvran repository.

Not available today: `evals` is not published on PyPI. `uv sync` from the checkout is the only install path. There is no `evals --version` flag; step 2 shows how to read the installed version instead. The `evals/` directory has no README of its own; the package's long-form document is [`evals/ARCHITECTURE.md`](../../evals/ARCHITECTURE.md).

Every command in this tutorial runs from the `evals/` directory.

## Step 1: Install the CLI

```bash
cd evals
uv sync
```

What you should see: `uv` prints the packages it resolves and installs, creates a `.venv/` directory inside `evals/`, and exits 0. The `evals` console script (`evals.cli:main`) is now available through `uv run`.

## Step 2: Confirm the CLI runs

```bash
uv run evals --help
```

What you should see: a `Usage:` line, the description `Kelvran evals CLI.`, and a `Commands:` list with ten entries: `audit-corpus`, `check-corpus-staleness`, `cost-report`, `flag-candidates`, `ingest`, `promote`, `report`, `rollout`, `run`, `trend`.

Now read the installed version:

```bash
uv run python -c 'import importlib.metadata as m; print(m.version("kelvran-evals"))'
```

What you should see: `0.10.1` on `main`. On a checkout of the `evals/v0.10.1` tag you see `0.8.0` instead; `evals/evals/cli.py` is identical on both.

## Step 3: Write the suite

A suite is a JSON array of `EvalCase` objects. Save the following as `/tmp/kelvran-suite.json`:

```json
[
  {
    "id": "capital-of-france",
    "revision": 1,
    "task_spec": { "output": "Paris", "match": "exact" },
    "reference": "Paris",
    "tier": "golden",
    "tags": ["geography"]
  },
  {
    "id": "contains-a-number",
    "revision": 1,
    "task_spec": { "output": "the answer is 42", "match": "regex", "pattern": "\\d+" },
    "reference": null,
    "tier": "golden",
    "tags": ["format"]
  },
  {
    "id": "wrong-answer",
    "revision": 1,
    "task_spec": { "output": "London", "match": "exact" },
    "reference": "Paris",
    "tier": "golden",
    "tags": ["geography"]
  }
]
```

What each field does:

- `id` is a string and `revision` an integer. Together they identify the case in every persisted `Score`.
- `task_spec` is a free-form object. For `evals run` it carries the `output` to score, already baked into the file. `match` selects the scorer and defaults to `exact`.
- `match: "exact"` compares `task_spec.output` with `reference`. `reference` is required.
- `match: "regex"` searches `task_spec.output` with `task_spec.pattern`. `pattern` is required and `reference` may be `null`.
- `tier` is one of `golden`, `regression`, or `drift_sample`. Any other value fails validation.
- `tags` is a list of strings and defaults to `[]`. `flaky` is a boolean that defaults to `false`; it is omitted here.

The third case compares `London` with `Paris` and is meant to fail, so the report has something to say.

This file has the same shape as the checked-in fixture [`evals/tests/fixtures/golden_example.json`](../../evals/tests/fixtures/golden_example.json); only the ids differ.

What you should see: nothing yet. The next step loads and validates the file.

## Step 4: Run the suite

```bash
uv run evals run --suite /tmp/kelvran-suite.json --scores /tmp/kelvran-scores.jsonl
```

What you should see:

```text
capital-of-france: PASS
contains-a-number: PASS
wrong-answer: FAIL
pass_rate=0.6667 (2/3) 95% CI=[0.2077, 0.9385]
```

`evals run` prints one `<id>: PASS` or `<id>: FAIL` line per case, then the pass rate with its 95% Wilson score interval. The CLI never prints a bare percentage on its own.

Two things to know about `--scores`:

- It is required. It became mandatory in `evals/v0.2.0`, the only breaking change recorded in [`UPGRADE.md`](../../UPGRADE.md).
- It appends. The file is created if it does not exist, and every later run adds more lines. If you re-run this step, delete `/tmp/kelvran-scores.jsonl` first so the counts in the following steps still match.

If a case cannot be scored, the command exits 1 with a Python traceback whose last line is `ValueError: case 'x': task_spec has no 'output' to score` or `ValueError: case 'x': exact match requires a reference`. Scores for the cases already processed are still appended before the error is reported.

## Step 5: Look at the persisted scores

```bash
cat /tmp/kelvran-scores.jsonl
```

What you should see: three JSON lines, one per case, in suite order. Each line is a `Score` and carries:

- `eval_case_id` and `eval_case_revision` from the case;
- `run_id` set to `null`, because `evals run` executes nothing;
- `scorer_id` set to `exact_match` or `regex_match`, and `scorer_type` set to `deterministic`;
- `value` set to `true` or `false`;
- `cost_usd` set to `"0"` (a `Decimal`, which lands in JSON as a string), because a deterministic scorer makes no external call;
- `tier`, `tags`, and `flaky` copied from the case, so `evals report` can filter and gate without re-reading the suite.

The remaining fields (`rationale`, `rubric_axis`, `bias_mitigations_applied`, `score_cache_key`, `from_cache`, `panel_votes`, `quorum_reached`, `quote_grounded`, `judge_prompt_version`) belong to LLM-judge scoring and stay at their defaults here: `null`, except `bias_mitigations_applied` (`[]`) and `from_cache` (`false`).

This JSONL shape is part of the versioned public surface described in [`docs/VERSIONING.md`](../VERSIONING.md).

## Step 6: Report from the scores

```bash
uv run evals report --scores /tmp/kelvran-scores.jsonl
```

What you should see:

```text
deterministic: pass_rate=0.6667 (2/3) 95% CI=[0.2077, 0.9385] total_cost_usd=0
```

`evals report --scores` prints one line per distinct `scorer_type` found in the file. Deterministic and LLM-judge scores are never blended into one number. Every line in the file counts as one trial; there is no de-duplication by case id, which is why step 4 asks you to start from a fresh file.

## Step 7: Add a CI/CD gate

`--fail-under RATE` makes `evals report` exit non-zero when a group's Wilson lower bound is below `RATE`. It compares the lower bound, not the point estimate. Your suite's lower bound is 0.2077.

First, a gate the suite clears:

```bash
uv run evals report --scores /tmp/kelvran-scores.jsonl --fail-under 0.2 ; echo "exit=$?"
```

What you should see:

```text
deterministic: pass_rate=0.6667 (2/3) 95% CI=[0.2077, 0.9385] total_cost_usd=0
exit=0
```

Now a gate the suite does not clear:

```bash
uv run evals report --scores /tmp/kelvran-scores.jsonl --fail-under 0.5 ; echo "exit=$?"
```

What you should see:

```text
deterministic: pass_rate=0.6667 (2/3) 95% CI=[0.2077, 0.9385] total_cost_usd=0
Error: CI/CD gate failed:
deterministic: Wilson lower bound 0.2077 < --fail-under 0.5000
exit=1
```

The point estimate (0.6667) is above 0.5, but the gate still fails because the lower bound (0.2077) is not. With only three cases the interval is wide: even a perfect 3/3 suite has a lower bound of 0.4385 and cannot pass a 0.5 gate. The gate only passes when the data supports the claim at the requested confidence, which is the point. Add cases to narrow the interval.

## Step 8: Run the gate that CI runs

This repository's CI runs the following two `uv run` commands from `evals/` on every push to `main` and every pull request against `main` (`.github/workflows/ci.yml`, step "CI gate smoke test (evals run + report --fail-under/--tier/--category-fail-under)"). They exercise three more gate features at once: tier filtering, flaky exclusion, and a per-category gate. The `rm -f` line is not part of the CI step: `--scores` appends and `evals/.ci-smoke-scores.jsonl` is gitignored, so a file left by an earlier local run would otherwise double every count below.

```bash
rm -f .ci-smoke-scores.jsonl
uv run evals run --suite tests/fixtures/ci_gate_example.json --scores .ci-smoke-scores.jsonl
uv run evals report --scores .ci-smoke-scores.jsonl --tier regression --fail-under 0.35 --category-fail-under "category:safety:0.15"
```

What you should see from the first command:

```text
ci-gate-regression-pass-1: PASS
ci-gate-regression-pass-2: PASS
ci-gate-regression-safety-pass: PASS
ci-gate-regression-flaky-fail: FAIL
ci-gate-golden-excluded-fail: FAIL
pass_rate=0.6000 (3/5) 95% CI=[0.2307, 0.8824]
```

What you should see from the second command, with exit code 0:

```text
deterministic: pass_rate=0.7500 (3/4) 95% CI=[0.3006, 0.9544] total_cost_usd=0 (1 flaky excluded from gate)
deterministic [category:safety]: pass_rate=1.0000 (1/1) 95% CI=[0.2065, 1.0000]
```

What happened, line by line:

- `--tier regression` keeps only the four `tier: "regression"` scores. The failing `golden` case is dropped before anything is printed, so the first line reports 3/4, not 3/5.
- The `flaky: true` case is still printed as part of the 3/4, but the gate ignores it. The gate is computed on 3 of 3 eligible scores, whose lower bound is 0.4385. That is above 0.35, so the aggregate gate passes.
- `--category-fail-under "category:safety:0.15"` adds an independent gate over the scores tagged `category:safety`. One case matches and passes: 1/1, lower bound 0.2065, above 0.15. A category tag that matches no score is an error, not a silent pass.
- The threshold 0.35 is chosen so that a regression in either mechanism flips the step red. If tier filtering or flaky exclusion stopped working, the eligible set would become 3 of 4 and its lower bound 0.3006 would fail the gate.

Remove the scratch file when you are done:

```bash
rm .ci-smoke-scores.jsonl
```

You now have the full loop: a suite file in version control, a `run` that persists scores, and a `report` whose exit code a CI job can act on.

## Beyond this tutorial

Two commands build on what you just did. Both need resources this tutorial deliberately avoided; each section below shows one command, and `uv run evals rollout --help` / `uv run evals run --help` list the options.

### Score real command output with `evals rollout`

`evals rollout` executes each case instead of reading a baked-in `output`. Its `task_spec` convention is `{image, command, timeout_s?, match?, pattern?}`; `timeout_s` defaults to 30. Each case runs one at a time in a locked-down Docker container (no network, read-only root, memory, CPU, and pid limits; see [`evals/evals/rollout/sandbox.py`](../../evals/evals/rollout/sandbox.py)). The captured stdout is stripped and scored exactly as in step 4. It needs a local `docker` CLI and the `alpine:3.20` image used by the fixture.

All four options are required: `--suite` plus the three output files `--results`, `--scores`, and `--traces`:

```bash
uv run evals rollout --suite tests/fixtures/rollout_example.json \
  --results /tmp/kelvran-runs.jsonl --scores /tmp/kelvran-rollout-scores.jsonl --traces /tmp/kelvran-spans.jsonl
uv run evals report --scores /tmp/kelvran-rollout-scores.jsonl
uv run evals report --traces /tmp/kelvran-spans.jsonl
```

The fixture [`evals/tests/fixtures/rollout_example.json`](../../evals/tests/fixtures/rollout_example.json) has one passing `echo hello-rollout` case and one failing `echo goodbye` case. A case that times out or fails to launch is printed with its run status in upper case (`<id>: TIMED_OUT` or `<id>: ERROR`) and never passes. `report --traces` prints an infrastructure signal (`spans: ok_rate=... avg_duration_ms=...`), not a quality judgment.

Not available today: rollouts are strictly sequential. There is no sandbox pool or concurrency setting.

### Score with an LLM judge

Adding `--llm-judge` to `evals run` replaces the deterministic scorer with one judge call per case. The default provider is AWS Bedrock with Claude Haiku 4.5 (`global.anthropic.claude-haiku-4-5-20251001-v1:0`); credentials come from boto3's standard chain (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, or an equivalent such as an IAM role). `--llm-judge-provider anthropic` uses `claude-haiku-4-5-20251001` with `ANTHROPIC_API_KEY`; `--llm-judge-provider openai` uses `gpt-4o-mini` with `OPENAI_API_KEY`. A judged case must have a `reference`, and a failed judge call prints `<id>: JUDGE_ERROR`. Every judged `Score` carries `judge_prompt_version`. On `main` that is `v2` ([`evals/evals/judge/llm_judge.py`](../../evals/evals/judge/llm_judge.py)); the `evals/v0.10.1` tag produces `v1` scores, and the two are not comparable, so do not mix a tag's judge scores with a `main` checkout's in one `--scores` file.

```bash
uv run evals run --suite tests/fixtures/llm_judge_example.json --scores /tmp/kelvran-judge-scores.jsonl --llm-judge
```

On every invocation the CLI loads `evals/.env` (the file next to `pyproject.toml`, not your current directory) without overriding variables you have already exported. Copy [`evals/.env.example`](../../evals/.env.example) to `evals/.env` for the variable names; the file is optional and gitignored. Ignore the comments inside it: they still say `ANTHROPIC_API_KEY` is what `--llm-judge` uses and that nothing calls OpenAI, neither of which is the case any more. The provider-to-key mapping in the paragraph above is the current one.

Not available today: `--llm-judge-panel` (two Bedrock judges with a strict majority vote) is Bedrock-only and cannot mix providers. There is no online LLM-judge scoring of production traffic; `evals ingest` samples and filters gateway decision events, which carry no prompt or completion text to judge.

## Where next

- [`evals/ARCHITECTURE.md`](../../evals/ARCHITECTURE.md) explains the `EvalCase`, `Run`, `Score`, and `Span` models and what is real versus planned in the evals system. There is no results dashboard; results are the JSONL files you created here, and the evals CLI never talks to a running gateway.
- [`docs/VERSIONING.md`](../VERSIONING.md) defines which parts of `evals` are the public surface: the CLI commands and options, and the persisted `Run` and `Score` schemas.
- [`UPGRADE.md`](../../UPGRADE.md) lists breaking changes, currently the single `evals/v0.2.0` row.
- [`docs/testing/TESTING.md`](../testing/TESTING.md) places evals suites within the repository's overall test strategy. To run the evals package's own tests: `uv run pytest tests/` from `evals/`. Docker-sandbox tests are skipped unless `RUN_DOCKER_TESTS=1`; live-LLM tests unless `RUN_LIVE_LLM_TESTS=1`.
- [Quickstart](quickstart.md) starts the gateway, the other Kelvran deployable, which this tutorial did not touch.
