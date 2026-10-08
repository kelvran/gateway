window.BENCHMARK_DATA = {
  "lastUpdate": 1791469777874,
  "repoUrl": "https://github.com/kelvran/gateway",
  "entries": {
    "kelvran-gateway (hosted-runner trend)": [
      {
        "commit": {
          "author": {
            "name": "sairam0424",
            "username": "sairam0424",
            "email": "uggesairam0000@gmail.com"
          },
          "committer": {
            "name": "sairam0424",
            "username": "sairam0424",
            "email": "uggesairam0000@gmail.com"
          },
          "id": "82027a9c0a77f6ac2ac1c5fc0080aac701ac7355",
          "message": "feat(gateway): benchmark harness, methodology page and nightly trend\n\nPlan item 5 of the 2026-10-08 discoverability round. Kelvran had no\nmeasurement of its own overhead and no methodology to publish one under.\n\ngateway/cmd/kelvran-bench: `run` drives open-loop Poisson load (seeded\nexponential gaps, so a slow gateway cannot hide queueing by slowing the\nload) through a gateway and records per request the status, the total\nlatency, the X-Kelvran-Overhead-Duration-Ms header, and for streams the\ntime to first content frame and every inter-chunk gap; nearest-rank\npercentiles, error rate (a stream that ends in an error frame or without\n[DONE] is an error), sent and completed rates, a schedule-lateness\nsummary (the coordinated-omission check), a cache-hit ratio derived from\ncompletion ids (replays keep the stored id; warm-up fills count as hits),\noptional RSS sampling of the gateway's pid before, during and after; output as results.json, github-action-benchmark\ncustomSmallerIsBetter JSON and a markdown table. `upstream` serves the\ndeterministic OpenAI-shaped mock provider (latency, chunk count and\ncadence, token counts) the gateway is pointed at. Seven presets: S1a/S1b\noverhead against a 0 ms and a 200 ms upstream, S2 streaming, S3 cache\nhits with Zipf repeats, S4 two replicas on Redis, S5 memory soak, S6\nsaturation sweep. Core in internal/bench and internal/benchupstream, both\nstdlib-only leaves, tested (schedule statistics, percentiles, SSE\ntiming, runner against the mock, report format, presets, prompt\nuniqueness across runs, warm-up-aware hit ratio, failed-stream\naccounting, PII-safe prompt suffix). Every prompt carries a letters-only\nrun nonce: the S6 sweep runs several Runs against one gateway, and a\ndigit nonce read as a phone number to the default guardrail policy.\n\nscripts/bench-ci.sh (make bench / make bench-ci) runs it end to end on\none host: builds both binaries, starts the mock and one gateway per\nscenario with a generated config and a random virtual key kept in a\n0600 file, loopback only, Redis only for S4, refuses busy ports, stops\neverything with SIGTERM; BENCH_BASELINE=1 adds a leg straight at the\nmock so every gateway number has its floor beside it. .github/workflows/bench-nightly.yml runs S1a-S4 nightly on a\nhosted runner (Redis service pinned by digest, every action pinned by\nSHA) as a regression trend: artifact, job summary, history on the\nbench-data branch via github-action-benchmark, a logged alert at 150 %\nof the previous value, never a failing job.\n\ndocs/operations/BENCHMARKS.md states the method, the scenarios, how to\nread the numbers, the methodology block any published number must carry,\nand records the first laptop run as a trend only (the gateway's own time\non a buffered request under 1 ms at the median and 2-4 ms at p99 whether\nthe upstream answers in 0 or 200 ms; about 1 ms added to a stream's time\nto first token at the median against the baseline leg, 1.6 ms to its\ninter-chunk p99; 95 % cache hits at 0.5 ms median; a 5-second S6 sweep\nput this laptop's knee between 500 and 1000 rps, inside the gateway). There are no published performance numbers: those\nawait the dedicated instance (gate G21). docs/rfcs/2026-10-08-gateway-\nstreaming-overhead-measurement.md proposes how to attribute the gateway's\nshare of a streamed response (gate G20).\n\nVerification: go build/vet, go test -race on the module (40 packages),\ngolangci-lint, go-arch-lint, shellcheck, actionlint; the recipe run end\nto end on this machine three times (results in\ndocs/operations/benchmarks/2026-10-08-dev-laptop/). Two adversarial\nreviews (four lenses, three refuters per finding), the second over the\nfirst's repairs: see docs/agents/LOGS.md.\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>",
          "timestamp": "2026-10-08T14:18:47Z",
          "url": "https://github.com/kelvran/gateway/commit/82027a9c0a77f6ac2ac1c5fc0080aac701ac7355"
        },
        "date": 1791469776912,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 1.16835,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 2.58294,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 3.431059,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p50",
            "value": 0,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p99",
            "value": 2,
            "unit": "ms"
          },
          {
            "name": "S1a error rate",
            "value": 0,
            "unit": "%"
          },
          {
            "name": "S1a rps shortfall",
            "value": 0,
            "unit": "req/s"
          },
          {
            "name": "S1b latency p50",
            "value": 201.787575,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 202.971237,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 203.998913,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p50",
            "value": 0,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p99",
            "value": 2,
            "unit": "ms"
          },
          {
            "name": "S1b error rate",
            "value": 0,
            "unit": "%"
          },
          {
            "name": "S1b rps shortfall",
            "value": 0,
            "unit": "req/s"
          },
          {
            "name": "S2 latency p50",
            "value": 572.881828,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 577.945037,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 580.923254,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 51.80166,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 53.91895,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.46925,
            "unit": "ms"
          },
          {
            "name": "S2 error rate",
            "value": 0,
            "unit": "%"
          },
          {
            "name": "S2 rps shortfall",
            "value": 0,
            "unit": "req/s"
          },
          {
            "name": "S3 latency p50",
            "value": 0.298123,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.55159,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.544861,
            "unit": "ms"
          },
          {
            "name": "S3 overhead p50",
            "value": 0,
            "unit": "ms"
          },
          {
            "name": "S3 overhead p99",
            "value": 0,
            "unit": "ms"
          },
          {
            "name": "S3 error rate",
            "value": 0,
            "unit": "%"
          },
          {
            "name": "S3 rps shortfall",
            "value": 0,
            "unit": "req/s"
          },
          {
            "name": "S4 latency p50",
            "value": 52.174813,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 52.948226,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 53.581196,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p99",
            "value": 2,
            "unit": "ms"
          },
          {
            "name": "S4 error rate",
            "value": 0,
            "unit": "%"
          },
          {
            "name": "S4 rps shortfall",
            "value": 0,
            "unit": "req/s"
          }
        ]
      }
    ]
  }
}