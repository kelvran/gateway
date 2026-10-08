# Benchmarks

**Status**: methodology and harness in force from 2026-10-08. There are **no published performance numbers yet**. The only numbers in this file come from a developer laptop; the nightly workflow's hosted-runner numbers live on the `bench-data` branch. Both are trends that catch regressions between commits, and neither is a figure to size a deployment by. Published numbers will come from a dedicated instance the maintainer chooses (plan gate G21); when they exist they will be added below with the full methodology block, and this sentence will go.

## What is measured, and how

The harness is `gateway/cmd/kelvran-bench` (core in `gateway/internal/bench`, mock provider in `gateway/internal/benchupstream`). A run has three processes on one host: the mock provider, one gateway (or two sharing a Redis), and the load generator.

- **Mock provider** (`kelvran-bench upstream`): answers `POST …/chat/completions` with an OpenAI-shaped body after a configured latency; a streamed response sends a configured number of content chunks at a configured cadence, then the finish chunk, a usage chunk and `data: [DONE]`. Token counts in `usage` are configured too, so the gateway's cost and TPM accounting run on known inputs. Deterministic: whatever the gateway adds on top is the gateway's.
- **Load** (`kelvran-bench run`): **open loop**. Request start times are drawn from a Poisson process at the offered rate (exponential gaps, seeded), so a slow gateway does not slow the load down and hide queueing, which a closed loop of workers would. Requests beyond an in-flight cap (default 4096) are counted as dropped, never silently skipped. Every prompt carries a suffix naming the run and the request, so neither an earlier request nor an earlier run against the same gateway (the S6 sweep runs several) can have warmed the cache for it; the cache scenario draws the request part from a pool instead. The run part is letters only: the gateway's default guardrail policy runs its PII detectors over every prompt, and a digit run reads as a phone or card number — an early run with a 19-digit nonce added a finding and a WARN log write to every request, which showed up as gateway overhead. A prompt of your own must not trip them either (`internal/bench/prompt_pii_test.go` guards the harness's). Each request's **schedule lateness** — how long after its Poisson instant it actually left the harness — is recorded, so a harness that cannot keep up shows it there instead of hiding it in optimistic latencies (coordinated omission).
- **Per request**: HTTP status; total latency (first byte of the request to the last byte of the body); the gateway's `X-Kelvran-Overhead-Duration-Ms` header on buffered responses (whole-request gateway time, integer milliseconds); for streams the **time to first token** (to the first `data:` frame whose delta carries content — the first frame of an OpenAI-shaped stream is role-only and is not the first token) and every **inter-chunk gap** between consecutive content frames; whether the stream ended with `[DONE]`, an in-band error frame, or neither.
- **Aggregation**: nearest-rank percentiles (p50, p95, p99: always a latency some request actually saw, never interpolated), min, max, mean; the error rate, where a stream that ended in an in-band error frame or without `[DONE]` is an error and contributes no latency, TTFT or inter-chunk sample; the **sent** rate (requests per second of the measured window) and the **completed** rate (successes per second) against the offered one; the schedule-lateness percentiles; for the cache scenario the hit ratio, computed from completion ids (a replay keeps the stored id, a fresh upstream call mints a new one) and counting the warm-up's fills as entries the window can hit.
- **Warm-up**: the first window of each scenario runs but is not recorded (connections, caches and the Go runtime settle).
- **Memory**: when the harness is given the gateway's pid it samples its RSS before the load (idle; for a sweep's later steps that is where the previous step ended), every second during it (peak) and after the last response (end).

## Scenarios

| Name | What it isolates | Mock | Notes |
|---|---|---|---|
| S1a | The gateway's own per-request overhead with nothing to hide behind | buffered, 0 ms | overhead header p99 is the headline |
| S1b | That the overhead does not grow with upstream time | buffered, 200 ms | compare the overhead header with S1a, not the latency |
| S2 | What the gateway adds to a stream: time to first token and inter-chunk latency | streaming, 50 ms to first chunk, 50 chunks at 10 ms | TTFT minus 50 ms and inter-chunk p99 minus 10 ms bound the gateway's share from above; `BENCH_BASELINE=1` measures the harness-plus-mock floor inside that bound (see the RFC below for a direct signal) |
| S3 | Cache hit ratio and hit latency | buffered, 200 ms; prompts drawn Zipf(1.1) from a pool of 200 | p50 is the hit path, p95/p99 the misses |
| S4 | Two replicas sharing Redis for rate limits, budgets, identity and propagation | buffered, 50 ms | needs `BENCH_REDIS_ADDR`; the only scenario whose gateways are configured with Redis; load round-robins across both |
| S5 | Memory: RSS idle, under load and over a soak | buffered, 1000 rps, 10 min | long; run deliberately |
| S6 | The saturation knee | buffered, 100 → 500 → 1000 → 2000 rps | one result per step, named `S6-<rate>rps`; each step's prompts are new to the gateway, so the cache cannot flatter the later steps; `BENCH_RPS` does not apply; watch the error rate, the schedule lateness and p99 |

Default windows are 120 s measured after 60 s warm-up (S6: 60 s after 30 s; S5: 10 min). `make bench` shortens them to 30 s / 5 s at 100 rps for a quick local trend (the nightly workflow's shape); `make bench-ci` keeps the presets' windows and is the shape any published number must use (the laptop record below states the shorter windows it was taken with).

## Running it

```bash
make bench                                     # S1a S1b S2 S3, 30 s each, 100 rps, output in bench-out/
make bench-ci                                  # the same four with the presets' full windows (120 s after 60 s warm-up)
BENCH_SCENARIOS="S2" BENCH_RPS=500 make bench-ci  # one scenario at another rate
BENCH_SCENARIOS="S6" make bench-ci             # the saturation sweep: one result per rate step
BENCH_REDIS_ADDR=127.0.0.1:6379 make bench-ci  # adds S4 with two gateway replicas sharing that Redis
BENCH_BASELINE=1 make bench                    # also runs each scenario straight at the mock, recorded as <scenario>-baseline
```

`scripts/bench-ci.sh` builds both binaries, starts the mock, writes a config with a random virtual key (the secret lives only in a 0600 file under a temporary directory; the gateway sees its hash), starts one gateway per scenario so each begins with an empty cache, runs the harness, and stops everything with SIGTERM. Only S4's gateways are configured with Redis; every other scenario runs the gateway with its in-memory stores, so it measures the gateway alone. Outputs: `bench-out/results.json` (one object per scenario — per rate step for S6, plus one per baseline leg when enabled, named `<scenario>-baseline`, or `S6-baseline-<rate>rps` for the stepped preset; the schema is `bench.Result`), `bench-out/bench.json` (the smaller-is-better metrics `github-action-benchmark` charts), `bench-out/summary.md`. The virtual key, deployment and listener live on loopback only.

To benchmark a running gateway instead, point the harness at it directly:

```bash
kelvran-bench run -scenario S1a -targets http://gateway:8080 -key-file ./key -model <model> -rps 200 -duration 2m -warmup 30s -summary -
```

## Reading the numbers

- The **overhead header** is the gateway's own time on a buffered request. It must stay flat between S1a and S1b; if it tracks the upstream latency something is wrong with the measurement or the gateway.
- **Latency p99 in S1b ≈ 200 ms + overhead**; anything beyond that is queueing on the host (open-loop load exposes it).
- **TTFT in S2 minus the mock's 50 ms** is an upper bound on the gateway's contribution to time to first token, and **inter-chunk p99 minus 10 ms** on its contribution to the cadence: the harness's own client-side cost and the mock's scheduling jitter sit inside both. `BENCH_BASELINE=1` runs the same load straight at the mock and records that floor as `S2-baseline`; the gateway's share is what is left when the floor is taken out of the bound — S2 minus S2-baseline, read percentile against percentile, as an estimate — with S2 minus 50 ms the hard upper bound. The header is not set on streams today; `docs/rfcs/2026-10-08-gateway-streaming-overhead-measurement.md` proposes how to attribute the gateway's share directly.
- **S3 hit ratio** depends on the pool size and the Zipf exponent as much as on the cache; compare runs with identical settings only. It counts the warm-up's fills as entries the window can hit (so a one-prompt pool reads 100 %, not one miss short of it).
- **Schedule lateness** must stay small — p99 well under the latencies it accompanies. If it grows, the harness or the host fell behind the offered rate and every latency column is optimistic: lower the rate, add CPU, or move the harness to another host, and do not record the run.
- **Sent/s against offered, completed/s against sent**: a gap between offered and sent is dropped arrivals (the in-flight cap) or the Poisson draw itself (a seed's schedule carries a few per cent more or fewer arrivals than rate × window); lateness never changes the sent count — it has its own column; a gap between sent and completed is the error rate, which includes streams that ended in an error frame or without `[DONE]`.
- The header has one-millisecond granularity, so sub-millisecond overheads read as 0 or 1 ms; the latency columns carry the precision.
- A hosted-runner result is a **trend**: shared, noisy machines whose neighbours change nightly. A 50 % swing on one night means nothing; a sustained shift after a specific commit does.

## Methodology block (required with any published number)

```
date, gateway version + commit, harness commit
instance type, vCPU, RAM, kernel; co-located (mock + gateway + harness on one host) or not, and CPU pinning if any
network path between harness and gateway
arrival model (open-loop Poisson), offered rate, warm-up, measured window, repetitions
request shape (messages, max_tokens), mock latency distribution, chunk count and cadence, token counts
Redis version and placement when used; config diff from config.example.yaml
caveats
```

## Results on record

### 2026-10-08 — developer laptop, dev build (trend only)

- Gateway `dev` build of `main` at `abcc1a4e` plus this change; harness at the same tree. Apple Silicon laptop (10 CPUs, `GOMAXPROCS` 10), Darwin 27.0.0 arm64; mock, gateway and harness co-located on one machine with other work running, including a Docker VM with five containers of another project; loopback network; 100 rps offered, 20 s measured after 3 s warm-up, one repetition (`BENCH_DURATION=20s BENCH_WARMUP=3s BENCH_RPS=100 BENCH_BASELINE=1 ./scripts/bench-ci.sh` — not the presets' 120 s / 60 s); `max_tokens` 64, one user message with the run-and-request suffix; mock latency per scenario above, tokens 50 in / 100 out; no Redis; config as `scripts/bench-ci.sh` writes it; `BENCH_BASELINE=1`, so each scenario also ran straight at the mock. Caveat: an earlier run of the same shape the same day, before the baseline leg existed and with less running on the host, read overhead p99 1.0 ms for S1a and S1b; the spread between runs is this laptop's noise, which is why this is a trend. Raw data: [`benchmarks/2026-10-08-dev-laptop/results.json`](benchmarks/2026-10-08-dev-laptop/results.json); the S6 sweep below: [`s6-5s-steps.json`](benchmarks/2026-10-08-dev-laptop/s6-5s-steps.json).

| Scenario | Offered rps | Sent/s | Completed/s | Requests | Errors | p50 | p95 | p99 | Overhead p99 | TTFT p50 | TTFT p99 | Inter-chunk p99 | Cache hit |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| S1a | 100 | 100 | 100 | 2006 | 0 | 1.2 ms | 2.5 ms | 3.7 ms | 2.0 ms | – | – | – | – |
| S1a-baseline | 100 | 100 | 100 | 2006 | 0 | 0.2 ms | 0.4 ms | 2.8 ms | – | – | – | – | – |
| S1b | 100 | 100 | 100 | 2006 | 0 | 202.1 ms | 204.8 ms | 209.9 ms | 2.0 ms | – | – | – | – |
| S1b-baseline | 100 | 100 | 100 | 2006 | 0 | 201.1 ms | 202.4 ms | 202.5 ms | – | – | – | – | – |
| S2 | 100 | 100 | 100 | 2006 | 0 | 563.9 ms | 578.8 ms | 607.1 ms | – | 51.6 ms | 56.9 ms | 12.6 ms | – |
| S2-baseline | 100 | 100 | 100 | 2006 | 0 | 562.8 ms | 592.2 ms | 607.3 ms | – | 50.5 ms | 54.8 ms | 13.1 ms | – |
| S3 | 100 | 100 | 100 | 2006 | 0 | 0.4 ms | 201.1 ms | 203.1 ms | 0.0 ms | – | – | – | 95 % |
| S3-baseline | 100 | 100 | 100 | 2006 | 0 | 201.1 ms | 202.5 ms | 202.6 ms | – | – | – | – | 0 % |

Gateway RSS during S1a: 20.7 MB idle, 44.5 MB peak, 44.5 MB at the end; S2 peaked at 63.7 MB. Schedule lateness p99 stayed between 2.0 and 3.0 ms on every leg (max 13 ms), so the harness kept its schedule.

What it says, and no more: on this laptop the gateway's own time on a buffered request (the overhead header) is under a millisecond at the median and 2 ms at p99 whether the upstream answers in 0 or 200 ms; read against the baseline leg it adds about 1.0 ms to the buffered median (1.2 against 0.2 ms) and 0.9 ms to the p99 at 0 ms upstream; about 1.0 ms to a stream's time to first token at the median (51.6 against 50.5 ms) and 2.1 ms at p99, and nothing the run can resolve to the inter-chunk p99 (12.6 ms against the baseline's 13.1 ms: inside this laptop's run-to-run noise); a cache hit answers in 0.4 ms at the median, with 95 % hits from the Zipf pool. What it does not say: anything about a production host, a real network, higher rates, or more than 20 seconds of load.

A 5-second-step S6 sweep on the same laptop (not a record — the preset's steps are 60 s) put the saturation knee between 500 and 1000 rps and inside the gateway: the overhead header tracked the latency (p99 4 ms at 500 rps, 4.2 s at 1000 rps, 9.3 s at 2000 rps) while the harness's schedule lateness p99 stayed under 2.2 ms; RSS reached 256 MB at 1000 rps and 519 MB at 2000 rps, where the in-flight cap dropped 7534 arrivals. The cause is not identified; it is the first question for the dedicated instance.

## Nightly trend

`.github/workflows/bench-nightly.yml` runs S1a, S1b, S2, S3 and S4 (with a Redis service) every night at 04:00 UTC on a hosted runner at 100 rps for 30 s each, uploads `bench-out/` as an artifact, writes the table to the job summary, and appends the metrics to the `bench-data` branch, which `github-action-benchmark` charts; a metric above 150 % of its previous value is logged as an alert and never fails the job. The branch is created by the workflow on its first run.

## Not in scope here

Comparisons with other gateways (plan item 16), the streaming overhead signal (the RFC above, gate G20), and the dedicated instance for published numbers (gate G21).
