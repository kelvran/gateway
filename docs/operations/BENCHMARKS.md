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
| S1r | The Redis cost per request: S1a's twin with the gateway in Redis mode (rate limits, budgets and the ledger on one Redis) | buffered, 0 ms | needs `BENCH_REDIS_ADDR`; read latency p99 and the `Redis RTT/req` column against S1a in the same run; since 2026-10-16 (round-5 item 10) |
| S1b | That the overhead does not grow with upstream time | buffered, 200 ms | compare the overhead header with S1a, not the latency |
| S2 | What the gateway adds to a stream: time to first token and inter-chunk latency | streaming, 50 ms to first chunk, 50 chunks at 10 ms | TTFT minus 50 ms and inter-chunk p99 minus 10 ms bound the gateway's share from above; `BENCH_BASELINE=1` measures the harness-plus-mock floor inside that bound (see the RFC below for a direct signal) |
| S3 | Cache hit ratio and hit latency | buffered, 200 ms; prompts drawn Zipf(1.1) from a pool of 200 | p50 is the hit path, p95/p99 the misses |
| S4 | Two replicas sharing Redis for rate limits, budgets, identity and propagation | buffered, 50 ms | needs `BENCH_REDIS_ADDR`; with S1r, one of the two scenarios whose gateways are configured with Redis (S1r measures one gateway, S4 two sharing it); load round-robins across both |
| S5 | Memory: RSS idle, under load and over a soak | buffered, 1000 rps, 10 min | long; run deliberately |
| S6 | The saturation knee | buffered, 100 → 500 → 1000 → 2000 rps | one result per step, named `S6-<rate>rps`; each step's prompts are new to the gateway, so the cache cannot flatter the later steps; `BENCH_RPS` does not apply; watch the error rate, the schedule lateness and p99 |

Default windows are 120 s measured after 60 s warm-up (S6: 60 s after 30 s; S5: 10 min). `make bench` shortens them to 30 s / 5 s at 100 rps for a quick local trend (the nightly workflow's shape); `make bench-ci` keeps the presets' windows and is the shape any published number must use (the laptop record below states the shorter windows it was taken with).

## Running it

```bash
make bench                                     # S1a S1b S2 S3, 30 s each, 100 rps, output in bench-out/
make bench-ci                                  # the same four with the presets' full windows (120 s after 60 s warm-up)
BENCH_SCENARIOS="S2" BENCH_RPS=500 make bench-ci  # one scenario at another rate
BENCH_SCENARIOS="S6" make bench-ci             # the saturation sweep: one result per rate step
BENCH_REDIS_ADDR=127.0.0.1:6379 make bench-ci  # adds S1r (one gateway in Redis mode) and S4 (two replicas sharing that Redis)
BENCH_BASELINE=1 make bench                    # also runs each scenario straight at the mock, recorded as <scenario>-baseline
```

`scripts/bench-ci.sh` builds both binaries, starts the mock, writes a config with a random virtual key (the secret lives only in a 0600 file under a temporary directory; the gateway sees its hash) that carries an RPM bucket, a TPM bucket and a daily token allowance, all far above what the offered rate can reach, so every preset runs the same code path and only the store behind it differs (since 2026-10-16, round-5 item 10; expect the in-memory presets' nightly trend to move by microseconds from the first night that carries this key). The key carries `budget_usd` and a warn threshold as well, so the capped path (the cap check and the warn and alert reads) is in the round-trip count; it was off the key for part of 2026-10-16 after the first S1r run found R19 ([FAILURE-MODES.md](FAILURE-MODES.md)), fixed the same day. Then starts one gateway per scenario so each begins with an empty cache, runs the harness, and stops everything with SIGTERM. Only S1r's and S4's gateways are configured with Redis, and only those are given `telemetry.prometheus` and scraped by the harness (`-metrics-url`, one per gateway) for the Redis round-trip counters; every other scenario runs the gateway with its in-memory stores, so it measures the gateway alone. Outputs: `bench-out/results.json` (one object per scenario — per rate step for S6, plus one per baseline leg when enabled, named `<scenario>-baseline`, or `S6-baseline-<rate>rps` for the stepped preset; the schema is `bench.Result`), `bench-out/bench.json` (the smaller-is-better metrics `github-action-benchmark` charts), `bench-out/summary.md`. The virtual key, deployment and listener live on loopback only.

To benchmark a running gateway instead, point the harness at it directly:

```bash
kelvran-bench run -scenario S1a -targets http://gateway:8080 -key-file ./key -model <model> -rps 200 -duration 2m -warmup 30s -summary -
```

## Reading the numbers

- The **overhead header** is the gateway's own time on a buffered request. It must stay flat between S1a and S1b; if it tracks the upstream latency something is wrong with the measurement or the gateway.
- **Redis RTT/req** (S1r, S4) is the window's delta of the gateway's own `kelvran.redis.round_trips` counter over the window's successful requests, read from the Prometheus endpoint at the warm-up boundary and after the last response (`results.json` also carries `redis_commands_per_request`, how many commands those round trips carried). It is a client-side count — one per single command, one per pipeline — so it falls when the pre-call scripts and the settlement are pipelined while the server's command count stays put; round-5 item 10's pass criterion is at most 2 per billable request. Refused and failed requests still pay their pre-call round trips and those stay in the numerator while the requests leave the divisor, so read the column only against an `Errors` column of 0 — a window with refusals overstates the per-success cost. A streaming Redis preset would also need its end scrape after `finalize`, which runs after the last chunk; today's two Redis presets are buffered, where the response is written after the settlement. **S1r minus S1a**, latency p99 against latency p99 (the overhead header's whole milliseconds cannot resolve it), is what Redis mode costs per request on a loopback Redis, where a round trip is tens of microseconds; against a managed Redis, multiply the round trips by the real RTT.
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

### 2026-10-16 — developer laptop, dev build, the first Redis-mode record (trend only)

- Gateway `dev` build of `main` at `f5411ff3` plus this change; harness at the same tree. Apple Silicon laptop (10 CPUs, `GOMAXPROCS` 10), Darwin 27.0.0 arm64; mock, gateway and harness co-located on one machine with nothing else running; loopback network; Redis 8.6.2 (Homebrew `redis-server`, a scratch instance on 127.0.0.1:16379 with persistence off, started and stopped for the run) on the same host; 100 rps offered, 30 s measured after 5 s warm-up, one repetition (`BENCH_REDIS_ADDR=127.0.0.1:16379 BENCH_SCENARIOS="S1a S1r" BENCH_DURATION=30s BENCH_WARMUP=5s BENCH_RPS=100 ./scripts/bench-ci.sh`, S4 added by the recipe — not the presets' 120 s / 60 s); `max_tokens` 64, one user message with the run-and-request suffix; mock latency per scenario above, tokens 50 in / 100 out; config as `scripts/bench-ci.sh` writes it on this date: the key with RPM, TPM and tokens-per-day limits and no `budget_usd`. Caveat: the first run of the same shape that day, with `budget_usd` on the key, refused 15.7 % of S1r's requests and 84.3 % of S4's as `429 budget exceeded` — the R19 finding ([FAILURE-MODES.md](FAILURE-MODES.md)), not a number to record; its `Redis RTT/req` read 12.3 because refused requests pay their pre-call round trips and sit outside the divisor.

| Scenario | Offered rps | Sent/s | Completed/s | Requests | Errors | p50 | p95 | p99 | Overhead p99 | TTFT p50 | TTFT p99 | Inter-chunk p99 | Cache hit | Redis RTT/req |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| S1a | 100 | 100 | 100 | 3015 | 0 | 1.5 ms | 3.2 ms | 4.7 ms | 3.0 ms | – | – | – | – | – |
| S1r | 100 | 100 | 100 | 3015 | 0 | 2.3 ms | 4.4 ms | 6.5 ms | 5.0 ms | – | – | – | – | 9.0 |
| S4 | 100 | 100 | 100 | 3015 | 0 | 52.8 ms | 65.8 ms | 136.1 ms | 63.0 ms | – | – | – | – | 9.0 |

Gateway RSS: S1a 21.4 MB idle, 52.6 MB peak; S1r 24.1 MB idle, 57.2 MB peak; S4's first replica 23.9 MB idle, 46.2 MB peak. Schedule lateness p99 was 1.2, 2.1 and 11.9 ms on the three legs (max 36 ms), so the harness kept its schedule.

What it says, and no more: on this laptop, against a loopback Redis, a billable buffered request costs the gateway **9.0 Redis round trips carrying 9.0 commands** — the five pre-call calls (the RPM bucket, the TPM and tokens-per-day reservations, the spend read, the uncapped budget reserve) and the four post-call ones (the budget adjust, the TPM and tokens-per-day settlements, the ledger record), none pipelined — and S4's two replicas pay the same 9.0 each. Redis mode adds about 0.8 ms to the median and 1.8 ms to the p99 of a 0 ms-upstream request here (S1r against S1a), roughly 0.1–0.2 ms per round trip, which is what a loopback round trip costs; the overhead header reads 5 against 3 ms at p99 and cannot resolve more. What it does not say: anything about a managed Redis, where the same nine round trips are nine network RTTs; about the capped-budget path (two more reads and the cap check), which the second record below measures after the R19 fix; or about a production host, a real network or higher rates. The pipelining work (round-5 item 10, phase 2) is read against this table: round trips per request first, latency p99 second.

### 2026-10-16, later the same day — developer laptop, dev build, the capped path after the R19 fix (trend only)

- Gateway `dev` build of `main` at `70e38c0e` plus the R19 fix; harness at the same tree. The same laptop, co-location, loopback Redis 8.6.2 (a scratch instance on 127.0.0.1:16379), 100 rps, 30 s after 5 s warm-up, one repetition, `BENCH_SCENARIOS="S1a S1r"` with S4 added by the recipe — as the record above, with one difference: the recipe's key carries `budget_usd` (1 000 000) and `budget_warn_percent` (0.8) again, so the capped budget path runs. Earlier the same day, with the pre-fix reserve script and this key, S1r refused 15.7 % of its requests and S4 84.3 % as `429 insufficient_quota` ([FAILURE-MODES.md](FAILURE-MODES.md) R19). Caveat: other work ran on the host between the two records, so compare round trips and errors across them, not latencies.

| Scenario | Offered rps | Sent/s | Completed/s | Requests | Errors | p50 | p95 | p99 | Overhead p99 | TTFT p50 | TTFT p99 | Inter-chunk p99 | Cache hit | Redis RTT/req |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| S1a | 100 | 100 | 100 | 3015 | 0 | 2.1 ms | 7.4 ms | 9.9 ms | 7.0 ms | – | – | – | – | – |
| S1r | 100 | 100 | 100 | 3015 | 0 | 4.6 ms | 9.2 ms | 12.7 ms | 10.0 ms | – | – | – | – | 11.0 |
| S4 | 100 | 100 | 100 | 3015 | 0 | 55.0 ms | 61.3 ms | 78.1 ms | 21.0 ms | – | – | – | – | 11.0 |

Gateway RSS: S1a 22.0 MB idle, 52.8 MB peak; S1r 24.0 MB idle, 58.7 MB peak; S4's first replica 26.3 MB idle, 43.1 MB peak. Schedule lateness p99 was 1.5, 1.3 and 3.4 ms (max 37 ms).

What it says, and no more: with the cap on the key, a billable buffered request costs **11.0 Redis round trips** — the nine of the record above plus the warn-threshold and alert-ladder reads — and every one of S1r's 3 015 requests and S4's 3 015 was served: the R19 fix holds under the load that found the defect, on one gateway and on two replicas sharing the Redis, with no `budget_warn_threshold_crossed` on a key whose spend is 0 USD. The nightly trend has no `Redis RTT/req` point yet. Its first point reads about 11.0 if this commit is on `main` before the next 04:00 UTC run (the capped path in the count from the start), 9.0 if that run catches the measurement commit alone; either way a later move between the two is the capped path joining the count, not a regression. What it does not say: anything about the mean itself — the harness's mock model is unpriced, every settlement is 0 USD, so from the first settlement on the reservation is 0 and the run proves only that the capped path admits concurrent requests (the sizing is `redisbudget`'s tests' to prove); anything about a managed Redis, a production host, a real network or higher rates; the pipelining work (round-5 item 10, phase 2) is read against this table first, the uncapped one above second.

### 2026-10-16, late evening — developer laptop, dev build, the folded spend reads (round-5 item 10 phase 2a) (trend only)

- Gateway `dev` build of `main` at `ef9bb40f` plus the phase-2a fold; harness at the same tree. The same laptop, co-location, loopback Redis 8.6.2 (a scratch instance on 127.0.0.1:16379), 100 rps, 30 s after 5 s warm-up, one repetition, `BENCH_SCENARIOS="S1a"` with S1r and S4 added by the recipe, the key as in the record above (`budget_usd` 1 000 000, `budget_warn_percent` 0.8). The one code difference from that record is the fold: the reserve script reports the spend before the reservation (the decision event's `budget_spent_usd`) and the adjust script the spend after the settlement (the warn-threshold and alert-ladder checks), so the three plain `HGET`s they replace are gone. Caveat: review agents read the tree during this run, so compare round trips and errors across the records, not latencies.

| Scenario | Offered rps | Sent/s | Completed/s | Requests | Errors | p50 | p95 | p99 | Overhead p99 | TTFT p50 | TTFT p99 | Inter-chunk p99 | Cache hit | Redis RTT/req |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| S1a | 100 | 100 | 100 | 3015 | 0 | 2.7 ms | 6.6 ms | 10.8 ms | 8.0 ms | – | – | – | – | – |
| S1r | 100 | 100 | 100 | 3015 | 0 | 2.7 ms | 7.1 ms | 15.4 ms | 13.0 ms | – | – | – | – | 8.0 |
| S4 | 100 | 100 | 100 | 3015 | 0 | 54.4 ms | 57.6 ms | 60.1 ms | 7.0 ms | – | – | – | – | 8.0 |

Gateway RSS: S1a 20.1 MB idle, 50.9 MB peak; S1r 23.1 MB idle, 55.2 MB peak; S4 23.2 MB idle, 42.3 MB peak (S4: its first replica). Schedule lateness p99 was 1.5, 2.9, 1.5 ms (max 65 ms).

What it says, and no more: a billable buffered request on the capped key costs **8.0 Redis round trips** (11.0 in the record above): the four pre-call calls — the RPM bucket, the TPM and tokens-per-day reservations, the budget reserve — and the four post-call ones — the budget adjust, the TPM and tokens-per-day settlements, the ledger record — none pipelined, and every one of S1r's 3 015 requests and S4's 3 015 was served with the decision event's spend and the two threshold checks reading the scripts' own replies. The capped path's extra round trips are now only the two mark scripts, one round trip each on every billable request once the spend is at or above `budget_warn_percent` (warn) or 50 % of the cap (ladder) — the bench key, at 0 USD spent, never reaches either. An uncapped key is 8.0 by construction, not measured here: its one plain read was the same `HGET` the reserve script now replaces (9.0 in the first record). What it does not say: anything about a managed Redis, a production host, a real network or higher rates; the latency columns are a trend (other work ran on the host), the round-trip count is exact. Phase 2b of round-5 item 10 was read against this table; 2c and 2d are read against the 2026-10-17 record below.

### 2026-10-17 — developer laptop, dev build, the rate-limit gate and the paired settle (round-5 item 10 phase 2b) (trend only)

- Gateway `dev` build of `main` at `c51adfa1` plus the phase-2b merge; harness at the same tree. The same laptop, co-location, loopback Redis 8.6.2 (a scratch instance on 127.0.0.1:16379), 100 rps, 30 s after 5 s warm-up, one repetition, `BENCH_SCENARIOS="S1a"` with S1r and S4 added by the recipe, the key as in the records above (RPM, TPM and tokens-per-day limits far above the offered rate, `budget_usd` 1 000 000, `budget_warn_percent` 0.8). The code difference from the record above: the three pre-call rate-limit scripts are one gate script that writes only when all three dimensions admit, and the two token settlements are one script. Caveat: review agents read the tree during this run, so compare round trips and errors across the records, not latencies.

| Scenario | Offered rps | Sent/s | Completed/s | Requests | Errors | p50 | p95 | p99 | Overhead p99 | TTFT p50 | TTFT p99 | Inter-chunk p99 | Cache hit | Redis RTT/req |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| S1a | 100 | 100 | 100 | 3015 | 0 | 2.0 ms | 5.7 ms | 8.0 ms | 6.0 ms | – | – | – | – | – |
| S1r | 100 | 100 | 100 | 3015 | 0 | 2.8 ms | 6.1 ms | 8.7 ms | 7.0 ms | – | – | – | – | 5.0 |
| S4 | 100 | 100 | 100 | 3015 | 0 | 53.5 ms | 56.2 ms | 57.8 ms | 5.0 ms | – | – | – | – | 5.0 |

Gateway RSS: S1a 20.7 MB idle, 50.4 MB peak; S1r 23.0 MB idle, 54.1 MB peak; S4 23.1 MB idle, 42.0 MB peak (S4: its first replica). Schedule lateness p99 was 1.3, 1.3, 2.2 ms (max 9 ms).

What it says, and no more: a billable buffered request on this key costs **5.0 Redis round trips** (8.0 in the record above): two pre-call — the rate-limit gate and the budget reserve — and three post-call — the budget adjust, the token settle, the ledger record — and every one of S1r's 3 015 requests and S4's 3 015 was served. `results.json`'s `redis_commands_per_request` reads 5.001 on both: one command per round trip, because both merges are single EVALs, not pipelines; the thousandth above 5 is the handshake go-redis sends through the hooks when it opens a pooled connection (`internal/redishook/hook.go`'s `DialHook` note), a few commands over the whole window, not the request path's. What it does not say: anything about a managed Redis, a production host, a real network or higher rates; anything about refusals — the key's limits are far above the offered rate, so the gate's write-only-on-admit rule is `redislimiter`'s tests' to prove; the latency columns are a trend, the round-trip count is exact. The floor with three go-redis clients is reached: phase 2c (one shared client when the rate-limit and budget addresses match, one pipeline each side) is read against this table, phase 2d after it.

## Nightly trend

`.github/workflows/bench-nightly.yml` runs S1a, S1b, S2, S3, S1r and S4 (the last two with a Redis service) every night at 04:00 UTC on a hosted runner at 100 rps for 30 s each, uploads `bench-out/` as an artifact, writes the table to the job summary, and appends the metrics to the `bench-data` branch, which `github-action-benchmark` charts; a metric above 150 % of its previous value is logged as an alert and never fails the job. The branch is created by the workflow on its first run.

## Not in scope here

Comparisons with other gateways (plan item 16), the streaming overhead signal (the RFC above, gate G20), and the dedicated instance for published numbers (gate G21).
