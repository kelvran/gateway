window.BENCHMARK_DATA = {
  "lastUpdate": 1792831018139,
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
      },
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
          "id": "c451705795809f064b94eeaa72c245ee37b74ac3",
          "message": "feat(telemetry): kelvran.fallback.rescued counter, fallback outcome on the duration histogram, hops on the span\n\nPlan item 13b (docs/rfcs/2026-10-09-gateway-fallback-rescued-and-prometheus-pull.md).\nA fallback was visible only as a per-hop span event, three decision-event\nfields and the anomaly detector's warning; nothing aggregated how often the\nchain saved a request.\n\nfallbackInfo gains class (the first failure's classifyFallbackError) and\nhops, counted by the call closure the chain sites already pass so the\nchain's signature is untouched. finalize records kelvran.fallback.rescued\n{virtual_key.id, fallback.from, deployment.name, fallback.hop.error_class}\nunder the same gate it uses for pricing (err == nil || billable), puts\nkelvran.fallback.outcome (none / rescued / exhausted) on\ngen_ai.client.operation.duration only (its own attribute slice: the shared\nattrs feeds eight instruments), and sets kelvran.fallback.hops on the span.\n\nFixed on the way: runMissPath dropped its fallback record on the error\nbranch, so an exhausted buffered chain or a rescued-then-guardrail-blocked\nrequest read as \"no fallback\". The closure now assigns the named return;\na failed buffered request's decision event therefore carries its fallback\nfields (streaming always did) and the anomaly detector observes those too.\nCoalesced followers of a rescued leader count as rescued, as their decision\nevent always said.\n\nTwelve tests, written red first or added from review; full -race run clean;\nlint and arch-lint clean. Live on real Bedrock through a scratch gateway:\n3 of 12 extractor requests landed on the broken twin and were rescued, with\ncounter, histogram and decision events agreeing. Two Grafana panels, the\ntelemetry and reference tables, FAILURE-MODES U1 and the changelog updated.\nThree-lens review with two refuters per finding (18 confirmed, no code\ndefect) and a fresh reviewer over the repairs (none).\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>",
          "timestamp": "2026-10-09T10:41:02Z",
          "url": "https://github.com/kelvran/gateway/commit/c451705795809f064b94eeaa72c245ee37b74ac3"
        },
        "date": 1791544055942,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 1.504564,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 2.886591,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 3.604871,
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
            "value": 201.874131,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 202.851929,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 203.757086,
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
            "value": 567.130608,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 572.764211,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 575.479835,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 51.916058,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 54.076017,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.554307,
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
            "value": 0.363438,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.617999,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.594653,
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
            "value": 52.335704,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 52.982267,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 53.390541,
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
      },
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
          "id": "c8f1d5b5044792ec82f795f9618da2e321fa9e6c",
          "message": "docs(deploy): the Kustomize how-to names the v0.18.0 pin\n\nThe re-pin commit 9aab55e9 moved deploy/k8s/base/deployment.yaml to the\ngateway/v0.18.0 index digest and updated README.md and the upgrade\nhow-to, but missed the Kustomize how-to's own sentence describing the pin.\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>",
          "timestamp": "2026-10-10T10:21:20Z",
          "url": "https://github.com/kelvran/gateway/commit/c8f1d5b5044792ec82f795f9618da2e321fa9e6c"
        },
        "date": 1791627863935,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 0.814352,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 2.811341,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 3.812283,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p50",
            "value": 0,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p99",
            "value": 3,
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
            "value": 201.324572,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 202.06807,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 202.668788,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p50",
            "value": 0,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p99",
            "value": 1,
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
            "value": 573.222082,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 577.444955,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 579.205094,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 51.878687,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 54.201915,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.392748,
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
            "value": 0.207269,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.534182,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.43423,
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
            "value": 51.57766,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 52.108128,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 53.002353,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p50",
            "value": 0,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p99",
            "value": 1,
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
      },
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
          "id": "6dc2589537dabb38f659ee692db33f86b6b1cf18",
          "message": "fix(gateway): request faults decided before upstream are invalid requests, not upstream errors\n\nThe 400s system_parts_unsupported, lossy_ingress_rejected and\ninvalid_prompt_reference (all four causes) are decided before any\nupstream call, so plan gate G16's rule applies: no Retry-After, no bump of the key's\nretry backoff, and OUTCOME_INVALID_REQUEST in the decision event. Their\nsix sentinels fell through outcomeFor's default to OUTCOME_UPSTREAM_ERROR\n(two postdated G16: fix(system-parts) 3de8cbd8 and slice S9b; the four\nprompt-reference ones predate it, found by the go review), so a\nclient that honoured the header waited a second for nothing, the key's\nnext real rejection started from a longer backoff, and telemetry counted\nan upstream error for a call that never left the gateway. Found by item\n14's design review against the route inventory.\n\n- dataplane.go: outcomeFor's G16 case gains errors.Is(err,\n  adapter.ErrSystemPartsUnsupported), isLossyIngressRejected(err) -- a\n  new errors.As helper for the pointer-typed lossy sentinel -- and the\n  four prompt-reference sentinels.\n- Tests first (red on the unmodified tree): six rows in\n  TestOutcomeForClassifiesEverySentinelError, six errors in\n  TestRetryStormEligibilityExcludesRequestFaults, Retry-After-absent\n  assertions in the system-parts and lossy integration tests and a new\n  prompt_id-with-messages integration test; the lossy test also pins the\n  logged OUTCOME_INVALID_REQUEST and the absence of OUTCOME_UPSTREAM_ERROR.\n  Five compiling overlay mutants (drop either new condition, drop the four\n  prompt conditions, helper returns false, helper matches the wrong type)\n  are each caught.\n- docs/reference/error-codes.md and data-plane-api.md: the Retry-After\n  tables name the three codes as never carrying it; FAILURE-MODES.md's\n  sentence on the local errors that still do is corrected (it named\n  model_not_allowed and the embeddings error, which never did); data-plane-api.md's Routes\n  row also stops saying chat-only (it is set on /v1/messages and on\n  count_tokens' 429 since gateway/v0.19.0) and its Not-eligible row gains\n  the three G16 codes it had missed.\n- docs/how-to/clients/openai-python.md, docs/reference/compatibility.md,\n  docs/how-to/troubleshooting.md: one Retry-After sentence each that said\n  invalid_prompt_reference carries it or that the header is chat-only.\n- gateway/changelog/unreleased.md: Fixed entry.\n\nVerification: go build/vet, golangci-lint, go-arch-lint, go test ./...\n-race (green except the Docker-container tests, whose Redis and mock\nupstream containers colima could not start -- a local content-store I/O\nerror, unrelated; CI runs them); three read-only reviewers (go FIX FIRST\n-> repaired, security PASS, docs FIX FIRST -> repaired) and fresh re-reads\nof every repair.\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>",
          "timestamp": "2026-10-11T10:14:11Z",
          "url": "https://github.com/kelvran/gateway/commit/6dc2589537dabb38f659ee692db33f86b6b1cf18"
        },
        "date": 1791714771596,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 1.694025,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 3.918743,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 8.248466,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p99",
            "value": 6,
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
            "value": 202.006157,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 203.080725,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 203.983851,
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
            "value": 567.874486,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 573.136906,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 575.392099,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 52.07968,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 54.864911,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.609552,
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
            "value": 0.380781,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.650019,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.656177,
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
            "value": 52.450645,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 53.364379,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 53.973818,
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
      },
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
          "id": "696dd69cc8172772c8db67dec2eae10e88dba220",
          "message": "feat(streaming): OpenAI-shaped tool-call deltas beside the flat members, positions on the wire\n\nStreamed tool-call deltas on POST /v1/chat/completions now carry\nOpenAI's ChatCompletionMessageToolCallChunk members -- `type`\n(\"function\", on the fragment that introduces the call, beside `id`)\nand `function.{name,arguments}` -- beside Kelvran's flat `name` and\n`arguments_json`, which are deprecated (DEPRECATED.md, first row;\nremoval no earlier than gateway/v0.22.0 and 90 days after v0.20.0\nships). `function` is present on every fragment with `arguments`\nalways a string, as in OpenAI's own stream. The Go struct stays flat\nfor the eight producers; the wire shape is a MarshalJSON/UnmarshalJSON\npair, adapter.ToolCall's device; reads accept flat, nested or both.\n\nOn anthropic and bedrock deployments the decoders number tool deltas\nby the provider's content-block index, so with a thinking or text\nblock first the first call streamed as index 1; openai-python's\nstream helper raised IndexError and openai-go's accumulator padded a\nphantom empty call (probed). streaming.Writer -- the only OpenAI SSE\nsink -- now renumbers per stream and per choice to positions by first\nappearance, on a copy; internal chunks keep the provider index, which\nthe cache accumulator's reasoning Sequence rebuild and the Anthropic\nmessages encoder rely on. openai upstreams and cache replays already\ncarry positions, so the mapping is the identity there.\n\nWhy both shapes: the SSE body has been public surface since the\npolicy took effect, and section 3 allows a minor to break it only\nwith a deprecation window where the old surface can live on -- it\ncan, by emitting both. Freezing the flat shape would have carried the\nbuffered/streamed asymmetry into 1.0 with the official SDKs' stream\naccumulators broken.\n\nContract: the ToolCallDelta schema gains `type`, a required `function`\n(with required `arguments`) and `deprecated: true` on the flat members; the mirror\ntest checks the type by a marshalled sample. Compat: the openai-go\naccumulator and the openai-python stream helper each reassemble a\nstreamed tool call through the gateway; the conformance probe\nvalidates every frame of a streamed tool turn.\n\nProof: reds recorded for the shape, the mirror and the renumbering;\n9/9 overlay mutants caught; whole non-Docker suite with -race, lint,\narch-lint green; compat suite green; live on real Bedrock the official\nopenai-python 3.28.0 helper returned get_weather({\"city\":\"Boston\"})\nwith a text block first and wire index 0 -- the first live run had\nfailed on Bedrock's index-only fragment, which is what made `function`\nunconditional.\n\nOwner decisions 2026-10-12: brainstorm item (j) first question, the\nwire-only renumbering, and the design approvals. Docs: data-plane\nreference, compatibility, streaming how-to, six client pages, the\nBifrost comparison, DEPRECATED.md, VERSIONING section 5, changelog\n(Added, Deprecated, Fixed), DECISIONS.md, LOGS.md.\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>",
          "timestamp": "2026-10-12T10:28:44Z",
          "url": "https://github.com/kelvran/gateway/commit/696dd69cc8172772c8db67dec2eae10e88dba220"
        },
        "date": 1791804922086,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 1.266895,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 2.255011,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 3.203539,
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
            "value": 201.852268,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 202.695106,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 203.586266,
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
            "value": 566.928154,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 572.282226,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 575.499122,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 51.785616,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 53.571476,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.561201,
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
            "value": 0.362922,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.545439,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.478903,
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
            "value": 52.637857,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 53.282834,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 53.69586,
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
      },
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
          "id": "9269dec85f962dd8a0dd18ce0fcedaf1ee81235f",
          "message": "feat(costaccounting): price the members Anthropic bills beyond flat tokens\n\nBrainstorm item (i), approved by the owner 2026-10-13 as \"accounting first, a\npolicy knob only on a trigger\". The gateway priced every request from tokens x\nmodel; Anthropic bills three response-reported members differently: cache\nwrites with a 1-hour TTL (2x base input against 1.25x for 5-minute writes),\nserver-side web searches ($10 per 1,000) and inference_geo \"us\" (1.1x on every\ntoken category). The item's recorded premise -- that forward_known betas change\nBedrock billing -- was wrong: the four forwarded betas are token-billed. Bedrock's\nreal gap is cachePoint.ttl \"1h\", whose 2x price Converse's usage never splits out.\n\nWhat changes:\n- adapter.Usage gains CacheCreation1hTokens, WebSearchRequests, InferenceGeo and\n  ServiceTier as internal json:\"-\" fields; the anthropic adapter parses\n  usage.cache_creation, usage.server_tool_use, usage.inference_geo and\n  usage.service_tier, buffered and streamed (anthropic-sdk-go v1.80.0 shape).\n- price_table.<model> accepts cache_creation_1h_per_token, web_search_per_request\n  and us_inference_multiplier; Calculate splits writes into the 5m and 1h slices\n  (1h clamped to [0, writes]), scales token cost by the multiplier on \"us\" and\n  adds the search fee unscaled. Negative rates, a multiplier below 1 and a 1h\n  rate below the 5-minute rate it falls back from are load errors. An absent key\n  keeps the earlier behaviour for that member: no hard-coded ratio, the same\n  choice as the 2026-09-10 cache-rate fallback.\n- dataplane.finalize settles the 1h slice once: on a bedrock deployment a\n  request that carried a 1h breakpoint on a model the adapter forwards the ttl\n  for prices every write at the 1h rate (bedrock.ModelSupportsCacheTTL, now\n  exported; adapter.HasOneHourCacheBreakpoint); the inference is confined to\n  bedrock and pinned by a test.\n- Four kelvran.usage.* span attributes, the two upstream strings bounded before\n  the span; kelvran init renders the 1h rate (2x base on every Anthropic row).\n- Docs: config reference, budgets how-to, THREAT_MODEL residual (1) narrowed,\n  metrics reference, RFC 2026-09-10 addendum, RFC-1 S11a resolution, data-plane\n  and CLI references, example config, changelog, DECISIONS, LOGS.\n\nVerification: red tests first in eight packages; 19 overlay mutants all caught;\nwhole non-Docker suite -race, compat suite, golangci-lint and go-arch-lint\ngreen; a live proof against a mock Anthropic Messages upstream moved the logged\ncost from 0.00785 to 0.04111 (buffered) and 0.00465 to 0.025511 (streamed) once\nthe keys were set, with the internal members absent from the client body.\nReviews: Go SHIP, docs SHIP, security FIX_FIRST on one MED (the loader floor\nabove), repaired and re-read SHIP by a fresh reviewer.\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>",
          "timestamp": "2026-10-12T22:04:15Z",
          "url": "https://github.com/kelvran/gateway/commit/9269dec85f962dd8a0dd18ce0fcedaf1ee81235f"
        },
        "date": 1791890609523,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 1.456738,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 2.649934,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 3.909043,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p50",
            "value": 0,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p99",
            "value": 3,
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
            "value": 201.952276,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 202.924729,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 203.693772,
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
            "value": 569.776588,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 576.492415,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 578.984845,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 52.075899,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 54.404067,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.605536,
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
            "value": 0.373694,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.599414,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.715285,
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
            "value": 52.653755,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 53.476342,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 54.054436,
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
      },
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
          "id": "739b11df4285bc7206911fac4582efcf4dac8e2a",
          "message": "fix(gateway): gate Claude 5.x fixed sampling, prefill, thinking disabled and forced tool_choice before the call\n\nRound-5 research 2026-10-14, Tier A item 2; closes the 2026-10-08 live\nverification's F5. The Claude 5.x models answer 400 to request shapes the\n4.x generation accepted, and every one reached Kelvran's clients as a 502\nupstream_error. One provider-agnostic table in adapter/capabilities.go now\ndecides each shape per family, matched at a family boundary so\nclaude-sonnet-5 never admits claude-sonnet-5-5:\n\n- Fixed sampling (Sonnet 5, Sonnet 5.5, Haiku 5.5, Fable 5.1 proven on\n  Bedrock; Mythos 5.1 by release note): a temperature other than 1, a top_p\n  other than 0.99, top_p beside temperature 1, and any top_k are left off\n  the wire by the anthropic and bedrock adapters and logged by the dataplane\n  on the existing request_field_dropped line (field, value, reason).\n  Dropping beats relaying the 400: a fallback chain or alias can land a\n  request on a generation the client did not pick, and the request then\n  runs at the model's only sampling anyway. Opus 5.5 is deliberately not\n  gated (unprobeable on the verifying account, undocumented).\n- Assistant prefill (Sonnet 4.6, Sonnet 5, Sonnet 5.5, Haiku 5.5, Fable 5.1\n  proven; Mythos 5.1 by release note) cannot be dropped without changing the\n  request, so it is a routing property (capabilityOKForRequest's fourth):\n  the pool is searched for an accepting deployment, otherwise the first-pick\n  sites answer 400 assistant_prefill_unsupported (param messages) before any\n  upstream call, with the per-call check kept as a backstop. A /v1/messages\n  body relayed as received to an anthropic deployment is never checked; a\n  bedrock translate hop is (isPassthroughHop mirrors passthroughRequestFor).\n- thinking disabled: rewritten to between_tools on Sonnet 5.5 (its documented\n  off switch, both halves proven) and dropped on the always-on families\n  (Fable 5.1 proven; Opus 5.5, Mythos 5.1 documented), where the request runs\n  as one without a thinking field, the block-binding default included.\n- Sonnet 5.5 joins the forced-tool-choice rejecters, and the bedrock adapter's\n  required branch (Converse any) now refuses the rejecters as tool already did.\n\nEvidence: Bedrock Converse probes with max_tokens 1 on 2026-10-14 (through\nthe gateway and directly), 19 overlay mutants caught, the whole non-Docker\nsuite with -race, golangci-lint, go-arch-lint and the compat suite green, a\nlive run of the new build against real Bedrock (200s with drop lines, the\nprefill 400, the forced-choice refusal, thinking disabled a 200 on all three\nfamilies). Three read-only review lanes plus a fresh re-read of the repairs;\nDECISIONS.md and docs/agents/LOGS.md record the design and the evidence.\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>",
          "timestamp": "2026-10-14T06:47:43Z",
          "url": "https://github.com/kelvran/gateway/commit/739b11df4285bc7206911fac4582efcf4dac8e2a"
        },
        "date": 1791969316649,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 1.410104,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 2.711832,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 3.645609,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p50",
            "value": 0,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p99",
            "value": 3,
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
            "value": 202.047458,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 203.323832,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 204.318779,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p50",
            "value": 0,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p99",
            "value": 3,
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
            "value": 572.775878,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 578.085679,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 579.997266,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 51.991173,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 54.447463,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.541276,
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
            "value": 0.326525,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.567253,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.632631,
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
            "value": 52.52981,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 53.367325,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 53.818847,
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
      },
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
          "id": "044645d4fc2c64c0a695e81f456ded6b3d410746",
          "message": "feat(admin): mount Go 1.27's goroutineleak pprof profile under /admin/debug/pprof/\n\nRound-5 item 8, the second leg of Tier B item 7 (after the toolchain commit\n3607d09f, which it is gated on): the named profiles are mounted by name through\npprof.Handler, which answers 404 \"Unknown profile\" for a name the running\ntoolchain does not register, so on a 1.26 build this route would have been a\n404 behind a green route table.\n\n- internal/admin/routes.go: pprofProfiles gains \"goroutineleak\" (TierAdmin,\n  admin.enable_pprof, the same capPprofSeconds wrapper as its siblings); the\n  table is now 34 rows over 29 paths, thirteen pprof patterns, nine routes\n  that read seconds=.\n- New TestPprofGoroutineleakProfileIsServed asks for the profile itself (the\n  text header under debug=1 and text/plain): without the route the request\n  falls into the /admin/debug/pprof/ subtree and the index answers 200 HTML,\n  so a status-only test would not notice a missing route.\n- Count pins moved red-first: routes_test (33/34, 29 paths), limits_http_test\n  (ninth seconds route), openapi_contract_test (37 path items, 42 operations),\n  openapi_contract_admin_test (pprofGoroutineleak: 200/400/408/500).\n- OpenAPI: one path item cloned from goroutine (load -> mutate -> dump), with\n  an operation description naming the cost; docs: admin-api.md (route index,\n  pprof table, Timeouts row, pprof prose), config.md, config.example.yaml,\n  openapi.md counts, THREAT_MODEL's Denial of Service row + change log.\n- Cost, stated where operators and threat readers look: each collection\n  forces a leak-detecting GC cycle (two with seconds=, whatever its value) and\n  collections serialize on a package-level mutex in runtime/pprof, so a caller\n  looping it slows the whole process for the duration; the 60 s seconds cap and\n  write timeout bound the connection, not the GC work; off-by-default flag and\n  Admin tier as for its siblings -- accepted as the Admin tier's own power.\n- changelog: ## Added (additive on the OpenAPI document, VERSIONING § 2/§ 3);\n  RELEASE.md's qualifier sweep now greps gateway/openapi and config.example.yaml.\n\nRed first, 5 overlay mutants caught (including the wrong profile behind the\nright route, which only the explicit test sees), whole non-Docker suite -race +\nlint + arch-lint on go1.27.2, compat, check-doc-paths; live against a scratch\ngateway: 200 text header, the 60 s cap, a one-second delta, the viewer token\nrefused, the index listing the profile. Three read-only review lanes plus a\nre-read; details in docs/agents/LOGS.md.\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>",
          "timestamp": "2026-10-15T07:36:19Z",
          "url": "https://github.com/kelvran/gateway/commit/044645d4fc2c64c0a695e81f456ded6b3d410746"
        },
        "date": 1792054100961,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 1.493915,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 2.923728,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 4.05018,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p50",
            "value": 0,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p99",
            "value": 3,
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
            "value": 202.106297,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 203.382788,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 204.274862,
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
            "value": 568.190577,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 573.67452,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 576.873662,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 52.077262,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 54.533297,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.629838,
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
            "value": 0.398231,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.683207,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.731194,
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
            "value": 53.012741,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 53.873005,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 54.491285,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p99",
            "value": 3,
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
      },
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
          "id": "5ac164af0e05fff0d7e5412511825a80fbd7eb7f",
          "message": "ci(gateway): stop persisting the token in the gateway job's checkout\n\nThe gateway job runs the test code of whatever commit it checks out -- a pull\nrequest's included -- and pushes nothing, so the GITHUB_TOKEN has no business\nsitting in .git/config for that code to read. The compat, evals and docs-links\njobs already set persist-credentials: false on their checkouts; the\n2026-10-16 role-scoping review (docs/agents/LOGS.md, the ci-mirror entry's\nINFO finding and the item-25 entry's follow-up) flagged this job as the odd\none out. One key on one step; local git commands keep working, only the\nremote auth header goes. The proto and publish-image checkouts still persist\nit -- proto for the same reason could follow, publish-image pushes and is\nleft alone deliberately.\n\nVerification: actionlint (no new finding); the two tests that read ci.yml\n(TestOneGoToolchain, TestOpenAPIBreakingGateIsWired); the whole non-Docker\nsuite; CI on this push is the live proof that the job needs no token after\ncheckout.\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>",
          "timestamp": "2026-10-16T07:39:28Z",
          "url": "https://github.com/kelvran/gateway/commit/5ac164af0e05fff0d7e5412511825a80fbd7eb7f"
        },
        "date": 1792140221681,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 1.502722,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 2.723124,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 3.673144,
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
            "value": 201.950327,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 202.805866,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 203.62772,
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
            "value": 566.549555,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 571.964605,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 573.932313,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 51.832563,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 53.829599,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.530018,
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
            "value": 0.371161,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.584487,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.597237,
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
            "value": 52.698278,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 53.396287,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 53.86458,
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
      },
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
          "id": "5c7fa0ac034b91fc70aa1b26b9f9d79a42cccb62",
          "message": "perf(ratelimit): one gate script for RPM, TPM and tokens-per-day and one settlement script for both token dimensions -- 8.0 -> 5.0 Redis round trips per billable request; a refused request consumes nothing (round-5 item 10 phase 2b)\n\nAfter phase 2a a billable Redis-mode request still made three pre-call rate-limit calls, each\nconditional on the previous one, a fourth releasing the minute reservation on a tokens-per-day\nrefusal, and two settle calls after the response. The three become one script that reserves nothing\nunless all three dimensions admit, the two settles one script.\n\n- redislimiter: admitLuaSrc (KEYS: the RPM hash, the TPM TAT and history, the day TAT and\n  history) evaluates RPM, then TPM, then tokens-per-day -- the day cold cap lowered to the minute\n  cost it has just sized, dayColdStartCap's rule -- and reserves every dimension only on a full\n  admit; the RPM state is still persisted on every call (TTL refresh, Allow's own write), the token\n  taken only on an admit; the RPM part keeps the client clock and the GCRA parts Redis TIME, as before. Returns\n  {allowed, refused_by, retry_after, tpm_cost, tpd_cost, tpd_cold}, floats tostring()'d.\n  settlePairLuaSrc settles up to two dimensions in one EVAL (not a pipeline: go-redis's NOSCRIPT\n  fallback is inert inside one and a retried pipeline would settle twice).\n- ratelimit: RedisBackend gains Admit and SettleTokens over the new leaf package\n  ratelimit/admission (redislimiter keeps satisfying the interface structurally, without\n  importing ratelimit); KeyLimiter.Admit builds the request (per-model RPM/TPM keys, the day key\n  with the 1/24 cold cap) and maps the result with the cold-refusal tolerance; in memory it runs\n  the three buckets and, for parity, refunds the RPM token (TokenBucket.Credit) and releases the\n  minute reservation when a later dimension refuses. KeyLimiter.SettleTokens keeps finalize's\n  per-dimension gates and skips the call when nothing is configured. .go-arch-lint.yml declares the\n  leaf as the ratelimit-admission component the ratelimit component may depend on.\n- dataplane: checkRateLimit is one Admit call -- a backend error fails the whole gate open once\n  (one ratelimit_backend_unavailable line, one kelvran.ratelimit.fail_open increment, nothing\n  reserved); a day refusal keeps its wait; RPM and TPM refusals keep the backoff alone. finalize\n  settles both token dimensions with one SettleTokens call.\n- tests: redislimiter admit_test.go (real Redis: a fresh key reserves all three; nothing written\n  on a TPM, day or RPM refusal; off dimensions; the paired settle teaches the mean), ratelimit\n  admit_test.go (in-memory refund and release; the Redis request's keys and cold cap; refusal\n  mapping incl. the %.14g cold case; settle entries and gates), dataplane admit_gate_test.go (one\n  gate call, a day refusal reserving nothing, one fail-open for all dimensions, one paired settle,\n  two streaming twins); the three Redis-mode tokens-per-day tests and the fail-open table\n  re-pinned to the gate; four fakes extended (a fifth inherits by embedding) and one new gate fake;\n  the streaming harness gains a limiter-taking form.\n- docs: FAILURE-MODES R1 (the gate), R2/R2b historical with dated notes, the settle-discard bullet\n  and a double-settle known gap; TELEMETRY, metrics-and-logs, the how-to, config.md, caching,\n  glossary, troubleshooting, data-plane-api; Implemented notes on the distributed-rate-limiting\n  and TOCTOU RFCs (+ a Corrected note on the stale addendum); ARCHITECTURE; BENCHMARKS 2026-10-17\n  record; the round-5 report's item-10 note; changelog Changed; DECISIONS; LOGS.\n\nVerification: red first, then green; 15 one-at-a-time mutants, all caught; gofmt, vet,\ngolangci-lint, go-arch-lint; the redislimiter package through Docker under -race; the real-Redis\ncmd/gateway integration tests; the whole non-Docker suite; compat module; check-doc-paths; lychee;\na live run of the real recipe against a scratch Redis (S1a/S1r/S4) recorded in BENCHMARKS.md;\nreview lanes and a re-read as recorded in docs/agents/LOGS.md.\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>",
          "timestamp": "2026-10-17T07:35:49Z",
          "url": "https://github.com/kelvran/gateway/commit/5c7fa0ac034b91fc70aa1b26b9f9d79a42cccb62"
        },
        "date": 1792224910509,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 1.410669,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 2.887577,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 4.022684,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p50",
            "value": 0,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p99",
            "value": 3,
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
            "value": 202.070807,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 203.360628,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 204.614139,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p50",
            "value": 0,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p99",
            "value": 3,
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
            "value": 568.660973,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 573.424087,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 575.573943,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 52.059415,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 54.479952,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.632124,
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
            "value": 0.401714,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.740351,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.784215,
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
            "name": "S1r latency p50",
            "value": 2.583156,
            "unit": "ms"
          },
          {
            "name": "S1r latency p95",
            "value": 4.617478,
            "unit": "ms"
          },
          {
            "name": "S1r latency p99",
            "value": 6.805106,
            "unit": "ms"
          },
          {
            "name": "S1r overhead p50",
            "value": 2,
            "unit": "ms"
          },
          {
            "name": "S1r overhead p99",
            "value": 5,
            "unit": "ms"
          },
          {
            "name": "S1r redis round trips per request",
            "value": 5.003316749585406,
            "unit": "rtt/req"
          },
          {
            "name": "S1r redis commands per request",
            "value": 5.0049751243781095,
            "unit": "cmd/req"
          },
          {
            "name": "S1r error rate",
            "value": 0,
            "unit": "%"
          },
          {
            "name": "S1r rps shortfall",
            "value": 0,
            "unit": "req/s"
          },
          {
            "name": "S4 latency p50",
            "value": 52.908118,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 53.952339,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 54.753047,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p99",
            "value": 3,
            "unit": "ms"
          },
          {
            "name": "S4 redis round trips per request",
            "value": 5.001326699834163,
            "unit": "rtt/req"
          },
          {
            "name": "S4 redis commands per request",
            "value": 5.001990049751244,
            "unit": "cmd/req"
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
      },
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
          "id": "5c7fa0ac034b91fc70aa1b26b9f9d79a42cccb62",
          "message": "perf(ratelimit): one gate script for RPM, TPM and tokens-per-day and one settlement script for both token dimensions -- 8.0 -> 5.0 Redis round trips per billable request; a refused request consumes nothing (round-5 item 10 phase 2b)\n\nAfter phase 2a a billable Redis-mode request still made three pre-call rate-limit calls, each\nconditional on the previous one, a fourth releasing the minute reservation on a tokens-per-day\nrefusal, and two settle calls after the response. The three become one script that reserves nothing\nunless all three dimensions admit, the two settles one script.\n\n- redislimiter: admitLuaSrc (KEYS: the RPM hash, the TPM TAT and history, the day TAT and\n  history) evaluates RPM, then TPM, then tokens-per-day -- the day cold cap lowered to the minute\n  cost it has just sized, dayColdStartCap's rule -- and reserves every dimension only on a full\n  admit; the RPM state is still persisted on every call (TTL refresh, Allow's own write), the token\n  taken only on an admit; the RPM part keeps the client clock and the GCRA parts Redis TIME, as before. Returns\n  {allowed, refused_by, retry_after, tpm_cost, tpd_cost, tpd_cold}, floats tostring()'d.\n  settlePairLuaSrc settles up to two dimensions in one EVAL (not a pipeline: go-redis's NOSCRIPT\n  fallback is inert inside one and a retried pipeline would settle twice).\n- ratelimit: RedisBackend gains Admit and SettleTokens over the new leaf package\n  ratelimit/admission (redislimiter keeps satisfying the interface structurally, without\n  importing ratelimit); KeyLimiter.Admit builds the request (per-model RPM/TPM keys, the day key\n  with the 1/24 cold cap) and maps the result with the cold-refusal tolerance; in memory it runs\n  the three buckets and, for parity, refunds the RPM token (TokenBucket.Credit) and releases the\n  minute reservation when a later dimension refuses. KeyLimiter.SettleTokens keeps finalize's\n  per-dimension gates and skips the call when nothing is configured. .go-arch-lint.yml declares the\n  leaf as the ratelimit-admission component the ratelimit component may depend on.\n- dataplane: checkRateLimit is one Admit call -- a backend error fails the whole gate open once\n  (one ratelimit_backend_unavailable line, one kelvran.ratelimit.fail_open increment, nothing\n  reserved); a day refusal keeps its wait; RPM and TPM refusals keep the backoff alone. finalize\n  settles both token dimensions with one SettleTokens call.\n- tests: redislimiter admit_test.go (real Redis: a fresh key reserves all three; nothing written\n  on a TPM, day or RPM refusal; off dimensions; the paired settle teaches the mean), ratelimit\n  admit_test.go (in-memory refund and release; the Redis request's keys and cold cap; refusal\n  mapping incl. the %.14g cold case; settle entries and gates), dataplane admit_gate_test.go (one\n  gate call, a day refusal reserving nothing, one fail-open for all dimensions, one paired settle,\n  two streaming twins); the three Redis-mode tokens-per-day tests and the fail-open table\n  re-pinned to the gate; four fakes extended (a fifth inherits by embedding) and one new gate fake;\n  the streaming harness gains a limiter-taking form.\n- docs: FAILURE-MODES R1 (the gate), R2/R2b historical with dated notes, the settle-discard bullet\n  and a double-settle known gap; TELEMETRY, metrics-and-logs, the how-to, config.md, caching,\n  glossary, troubleshooting, data-plane-api; Implemented notes on the distributed-rate-limiting\n  and TOCTOU RFCs (+ a Corrected note on the stale addendum); ARCHITECTURE; BENCHMARKS 2026-10-17\n  record; the round-5 report's item-10 note; changelog Changed; DECISIONS; LOGS.\n\nVerification: red first, then green; 15 one-at-a-time mutants, all caught; gofmt, vet,\ngolangci-lint, go-arch-lint; the redislimiter package through Docker under -race; the real-Redis\ncmd/gateway integration tests; the whole non-Docker suite; compat module; check-doc-paths; lychee;\na live run of the real recipe against a scratch Redis (S1a/S1r/S4) recorded in BENCHMARKS.md;\nreview lanes and a re-read as recorded in docs/agents/LOGS.md.\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>",
          "timestamp": "2026-10-17T07:35:49Z",
          "url": "https://github.com/kelvran/gateway/commit/5c7fa0ac034b91fc70aa1b26b9f9d79a42cccb62"
        },
        "date": 1792312339551,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 1.912699,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 3.508627,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 4.535585,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p99",
            "value": 3,
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
            "value": 202.479629,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 203.838405,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 204.939531,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p99",
            "value": 3,
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
            "value": 570.488076,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 578.196707,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 580.526569,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 52.176856,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 54.616393,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.629315,
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
            "value": 0.469235,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.721317,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.835867,
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
            "name": "S1r latency p50",
            "value": 2.80066,
            "unit": "ms"
          },
          {
            "name": "S1r latency p95",
            "value": 4.770148,
            "unit": "ms"
          },
          {
            "name": "S1r latency p99",
            "value": 5.858519,
            "unit": "ms"
          },
          {
            "name": "S1r overhead p50",
            "value": 2,
            "unit": "ms"
          },
          {
            "name": "S1r overhead p99",
            "value": 4,
            "unit": "ms"
          },
          {
            "name": "S1r redis round trips per request",
            "value": 5.001326699834163,
            "unit": "rtt/req"
          },
          {
            "name": "S1r redis commands per request",
            "value": 5.001990049751244,
            "unit": "cmd/req"
          },
          {
            "name": "S1r error rate",
            "value": 0,
            "unit": "%"
          },
          {
            "name": "S1r rps shortfall",
            "value": 0,
            "unit": "req/s"
          },
          {
            "name": "S4 latency p50",
            "value": 53.007254,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 53.987904,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 54.558579,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p99",
            "value": 3,
            "unit": "ms"
          },
          {
            "name": "S4 redis round trips per request",
            "value": 5.001326699834163,
            "unit": "rtt/req"
          },
          {
            "name": "S4 redis commands per request",
            "value": 5.001990049751244,
            "unit": "cmd/req"
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
      },
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
          "id": "5c7fa0ac034b91fc70aa1b26b9f9d79a42cccb62",
          "message": "perf(ratelimit): one gate script for RPM, TPM and tokens-per-day and one settlement script for both token dimensions -- 8.0 -> 5.0 Redis round trips per billable request; a refused request consumes nothing (round-5 item 10 phase 2b)\n\nAfter phase 2a a billable Redis-mode request still made three pre-call rate-limit calls, each\nconditional on the previous one, a fourth releasing the minute reservation on a tokens-per-day\nrefusal, and two settle calls after the response. The three become one script that reserves nothing\nunless all three dimensions admit, the two settles one script.\n\n- redislimiter: admitLuaSrc (KEYS: the RPM hash, the TPM TAT and history, the day TAT and\n  history) evaluates RPM, then TPM, then tokens-per-day -- the day cold cap lowered to the minute\n  cost it has just sized, dayColdStartCap's rule -- and reserves every dimension only on a full\n  admit; the RPM state is still persisted on every call (TTL refresh, Allow's own write), the token\n  taken only on an admit; the RPM part keeps the client clock and the GCRA parts Redis TIME, as before. Returns\n  {allowed, refused_by, retry_after, tpm_cost, tpd_cost, tpd_cold}, floats tostring()'d.\n  settlePairLuaSrc settles up to two dimensions in one EVAL (not a pipeline: go-redis's NOSCRIPT\n  fallback is inert inside one and a retried pipeline would settle twice).\n- ratelimit: RedisBackend gains Admit and SettleTokens over the new leaf package\n  ratelimit/admission (redislimiter keeps satisfying the interface structurally, without\n  importing ratelimit); KeyLimiter.Admit builds the request (per-model RPM/TPM keys, the day key\n  with the 1/24 cold cap) and maps the result with the cold-refusal tolerance; in memory it runs\n  the three buckets and, for parity, refunds the RPM token (TokenBucket.Credit) and releases the\n  minute reservation when a later dimension refuses. KeyLimiter.SettleTokens keeps finalize's\n  per-dimension gates and skips the call when nothing is configured. .go-arch-lint.yml declares the\n  leaf as the ratelimit-admission component the ratelimit component may depend on.\n- dataplane: checkRateLimit is one Admit call -- a backend error fails the whole gate open once\n  (one ratelimit_backend_unavailable line, one kelvran.ratelimit.fail_open increment, nothing\n  reserved); a day refusal keeps its wait; RPM and TPM refusals keep the backoff alone. finalize\n  settles both token dimensions with one SettleTokens call.\n- tests: redislimiter admit_test.go (real Redis: a fresh key reserves all three; nothing written\n  on a TPM, day or RPM refusal; off dimensions; the paired settle teaches the mean), ratelimit\n  admit_test.go (in-memory refund and release; the Redis request's keys and cold cap; refusal\n  mapping incl. the %.14g cold case; settle entries and gates), dataplane admit_gate_test.go (one\n  gate call, a day refusal reserving nothing, one fail-open for all dimensions, one paired settle,\n  two streaming twins); the three Redis-mode tokens-per-day tests and the fail-open table\n  re-pinned to the gate; four fakes extended (a fifth inherits by embedding) and one new gate fake;\n  the streaming harness gains a limiter-taking form.\n- docs: FAILURE-MODES R1 (the gate), R2/R2b historical with dated notes, the settle-discard bullet\n  and a double-settle known gap; TELEMETRY, metrics-and-logs, the how-to, config.md, caching,\n  glossary, troubleshooting, data-plane-api; Implemented notes on the distributed-rate-limiting\n  and TOCTOU RFCs (+ a Corrected note on the stale addendum); ARCHITECTURE; BENCHMARKS 2026-10-17\n  record; the round-5 report's item-10 note; changelog Changed; DECISIONS; LOGS.\n\nVerification: red first, then green; 15 one-at-a-time mutants, all caught; gofmt, vet,\ngolangci-lint, go-arch-lint; the redislimiter package through Docker under -race; the real-Redis\ncmd/gateway integration tests; the whole non-Docker suite; compat module; check-doc-paths; lychee;\na live run of the real recipe against a scratch Redis (S1a/S1r/S4) recorded in BENCHMARKS.md;\nreview lanes and a re-read as recorded in docs/agents/LOGS.md.\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>",
          "timestamp": "2026-10-17T07:35:49Z",
          "url": "https://github.com/kelvran/gateway/commit/5c7fa0ac034b91fc70aa1b26b9f9d79a42cccb62"
        },
        "date": 1792402413018,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 1.409599,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 2.892219,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 4.121246,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p50",
            "value": 0,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p99",
            "value": 3,
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
            "value": 202.358812,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 204.038099,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 205.379048,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p99",
            "value": 3,
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
            "value": 568.635528,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 573.083238,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 575.734383,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 52.231775,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 54.777145,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.587448,
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
            "value": 0.43535,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.743863,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.836024,
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
            "name": "S1r latency p50",
            "value": 2.72263,
            "unit": "ms"
          },
          {
            "name": "S1r latency p95",
            "value": 4.419695,
            "unit": "ms"
          },
          {
            "name": "S1r latency p99",
            "value": 5.563759,
            "unit": "ms"
          },
          {
            "name": "S1r overhead p50",
            "value": 2,
            "unit": "ms"
          },
          {
            "name": "S1r overhead p99",
            "value": 4,
            "unit": "ms"
          },
          {
            "name": "S1r redis round trips per request",
            "value": 5.001326699834163,
            "unit": "rtt/req"
          },
          {
            "name": "S1r redis commands per request",
            "value": 5.001990049751244,
            "unit": "cmd/req"
          },
          {
            "name": "S1r error rate",
            "value": 0,
            "unit": "%"
          },
          {
            "name": "S1r rps shortfall",
            "value": 0,
            "unit": "req/s"
          },
          {
            "name": "S4 latency p50",
            "value": 52.929138,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 53.782271,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 54.420503,
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
            "name": "S4 redis round trips per request",
            "value": 5.000663349917081,
            "unit": "rtt/req"
          },
          {
            "name": "S4 redis commands per request",
            "value": 5.000995024875622,
            "unit": "cmd/req"
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
      },
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
          "id": "5c7fa0ac034b91fc70aa1b26b9f9d79a42cccb62",
          "message": "perf(ratelimit): one gate script for RPM, TPM and tokens-per-day and one settlement script for both token dimensions -- 8.0 -> 5.0 Redis round trips per billable request; a refused request consumes nothing (round-5 item 10 phase 2b)\n\nAfter phase 2a a billable Redis-mode request still made three pre-call rate-limit calls, each\nconditional on the previous one, a fourth releasing the minute reservation on a tokens-per-day\nrefusal, and two settle calls after the response. The three become one script that reserves nothing\nunless all three dimensions admit, the two settles one script.\n\n- redislimiter: admitLuaSrc (KEYS: the RPM hash, the TPM TAT and history, the day TAT and\n  history) evaluates RPM, then TPM, then tokens-per-day -- the day cold cap lowered to the minute\n  cost it has just sized, dayColdStartCap's rule -- and reserves every dimension only on a full\n  admit; the RPM state is still persisted on every call (TTL refresh, Allow's own write), the token\n  taken only on an admit; the RPM part keeps the client clock and the GCRA parts Redis TIME, as before. Returns\n  {allowed, refused_by, retry_after, tpm_cost, tpd_cost, tpd_cold}, floats tostring()'d.\n  settlePairLuaSrc settles up to two dimensions in one EVAL (not a pipeline: go-redis's NOSCRIPT\n  fallback is inert inside one and a retried pipeline would settle twice).\n- ratelimit: RedisBackend gains Admit and SettleTokens over the new leaf package\n  ratelimit/admission (redislimiter keeps satisfying the interface structurally, without\n  importing ratelimit); KeyLimiter.Admit builds the request (per-model RPM/TPM keys, the day key\n  with the 1/24 cold cap) and maps the result with the cold-refusal tolerance; in memory it runs\n  the three buckets and, for parity, refunds the RPM token (TokenBucket.Credit) and releases the\n  minute reservation when a later dimension refuses. KeyLimiter.SettleTokens keeps finalize's\n  per-dimension gates and skips the call when nothing is configured. .go-arch-lint.yml declares the\n  leaf as the ratelimit-admission component the ratelimit component may depend on.\n- dataplane: checkRateLimit is one Admit call -- a backend error fails the whole gate open once\n  (one ratelimit_backend_unavailable line, one kelvran.ratelimit.fail_open increment, nothing\n  reserved); a day refusal keeps its wait; RPM and TPM refusals keep the backoff alone. finalize\n  settles both token dimensions with one SettleTokens call.\n- tests: redislimiter admit_test.go (real Redis: a fresh key reserves all three; nothing written\n  on a TPM, day or RPM refusal; off dimensions; the paired settle teaches the mean), ratelimit\n  admit_test.go (in-memory refund and release; the Redis request's keys and cold cap; refusal\n  mapping incl. the %.14g cold case; settle entries and gates), dataplane admit_gate_test.go (one\n  gate call, a day refusal reserving nothing, one fail-open for all dimensions, one paired settle,\n  two streaming twins); the three Redis-mode tokens-per-day tests and the fail-open table\n  re-pinned to the gate; four fakes extended (a fifth inherits by embedding) and one new gate fake;\n  the streaming harness gains a limiter-taking form.\n- docs: FAILURE-MODES R1 (the gate), R2/R2b historical with dated notes, the settle-discard bullet\n  and a double-settle known gap; TELEMETRY, metrics-and-logs, the how-to, config.md, caching,\n  glossary, troubleshooting, data-plane-api; Implemented notes on the distributed-rate-limiting\n  and TOCTOU RFCs (+ a Corrected note on the stale addendum); ARCHITECTURE; BENCHMARKS 2026-10-17\n  record; the round-5 report's item-10 note; changelog Changed; DECISIONS; LOGS.\n\nVerification: red first, then green; 15 one-at-a-time mutants, all caught; gofmt, vet,\ngolangci-lint, go-arch-lint; the redislimiter package through Docker under -race; the real-Redis\ncmd/gateway integration tests; the whole non-Docker suite; compat module; check-doc-paths; lychee;\na live run of the real recipe against a scratch Redis (S1a/S1r/S4) recorded in BENCHMARKS.md;\nreview lanes and a re-read as recorded in docs/agents/LOGS.md.\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>",
          "timestamp": "2026-10-17T07:35:49Z",
          "url": "https://github.com/kelvran/gateway/commit/5c7fa0ac034b91fc70aa1b26b9f9d79a42cccb62"
        },
        "date": 1792487257887,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 1.552713,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 3.002956,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 4.213143,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p99",
            "value": 3,
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
            "value": 202.071835,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 203.413204,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 204.656206,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p50",
            "value": 0,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p99",
            "value": 3,
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
            "value": 570.981214,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 577.609721,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 582.073001,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 52.019654,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 54.473039,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.635143,
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
            "value": 0.400151,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.6606,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.649893,
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
            "name": "S1r latency p50",
            "value": 2.492819,
            "unit": "ms"
          },
          {
            "name": "S1r latency p95",
            "value": 4.435031,
            "unit": "ms"
          },
          {
            "name": "S1r latency p99",
            "value": 5.62474,
            "unit": "ms"
          },
          {
            "name": "S1r overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S1r overhead p99",
            "value": 4,
            "unit": "ms"
          },
          {
            "name": "S1r redis round trips per request",
            "value": 5.000663349917081,
            "unit": "rtt/req"
          },
          {
            "name": "S1r redis commands per request",
            "value": 5.000995024875622,
            "unit": "cmd/req"
          },
          {
            "name": "S1r error rate",
            "value": 0,
            "unit": "%"
          },
          {
            "name": "S1r rps shortfall",
            "value": 0,
            "unit": "req/s"
          },
          {
            "name": "S4 latency p50",
            "value": 52.825374,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 53.817647,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 54.440461,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p99",
            "value": 3,
            "unit": "ms"
          },
          {
            "name": "S4 redis round trips per request",
            "value": 5.000995024875622,
            "unit": "rtt/req"
          },
          {
            "name": "S4 redis commands per request",
            "value": 5.001326699834163,
            "unit": "cmd/req"
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
      },
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
          "id": "5c7fa0ac034b91fc70aa1b26b9f9d79a42cccb62",
          "message": "perf(ratelimit): one gate script for RPM, TPM and tokens-per-day and one settlement script for both token dimensions -- 8.0 -> 5.0 Redis round trips per billable request; a refused request consumes nothing (round-5 item 10 phase 2b)\n\nAfter phase 2a a billable Redis-mode request still made three pre-call rate-limit calls, each\nconditional on the previous one, a fourth releasing the minute reservation on a tokens-per-day\nrefusal, and two settle calls after the response. The three become one script that reserves nothing\nunless all three dimensions admit, the two settles one script.\n\n- redislimiter: admitLuaSrc (KEYS: the RPM hash, the TPM TAT and history, the day TAT and\n  history) evaluates RPM, then TPM, then tokens-per-day -- the day cold cap lowered to the minute\n  cost it has just sized, dayColdStartCap's rule -- and reserves every dimension only on a full\n  admit; the RPM state is still persisted on every call (TTL refresh, Allow's own write), the token\n  taken only on an admit; the RPM part keeps the client clock and the GCRA parts Redis TIME, as before. Returns\n  {allowed, refused_by, retry_after, tpm_cost, tpd_cost, tpd_cold}, floats tostring()'d.\n  settlePairLuaSrc settles up to two dimensions in one EVAL (not a pipeline: go-redis's NOSCRIPT\n  fallback is inert inside one and a retried pipeline would settle twice).\n- ratelimit: RedisBackend gains Admit and SettleTokens over the new leaf package\n  ratelimit/admission (redislimiter keeps satisfying the interface structurally, without\n  importing ratelimit); KeyLimiter.Admit builds the request (per-model RPM/TPM keys, the day key\n  with the 1/24 cold cap) and maps the result with the cold-refusal tolerance; in memory it runs\n  the three buckets and, for parity, refunds the RPM token (TokenBucket.Credit) and releases the\n  minute reservation when a later dimension refuses. KeyLimiter.SettleTokens keeps finalize's\n  per-dimension gates and skips the call when nothing is configured. .go-arch-lint.yml declares the\n  leaf as the ratelimit-admission component the ratelimit component may depend on.\n- dataplane: checkRateLimit is one Admit call -- a backend error fails the whole gate open once\n  (one ratelimit_backend_unavailable line, one kelvran.ratelimit.fail_open increment, nothing\n  reserved); a day refusal keeps its wait; RPM and TPM refusals keep the backoff alone. finalize\n  settles both token dimensions with one SettleTokens call.\n- tests: redislimiter admit_test.go (real Redis: a fresh key reserves all three; nothing written\n  on a TPM, day or RPM refusal; off dimensions; the paired settle teaches the mean), ratelimit\n  admit_test.go (in-memory refund and release; the Redis request's keys and cold cap; refusal\n  mapping incl. the %.14g cold case; settle entries and gates), dataplane admit_gate_test.go (one\n  gate call, a day refusal reserving nothing, one fail-open for all dimensions, one paired settle,\n  two streaming twins); the three Redis-mode tokens-per-day tests and the fail-open table\n  re-pinned to the gate; four fakes extended (a fifth inherits by embedding) and one new gate fake;\n  the streaming harness gains a limiter-taking form.\n- docs: FAILURE-MODES R1 (the gate), R2/R2b historical with dated notes, the settle-discard bullet\n  and a double-settle known gap; TELEMETRY, metrics-and-logs, the how-to, config.md, caching,\n  glossary, troubleshooting, data-plane-api; Implemented notes on the distributed-rate-limiting\n  and TOCTOU RFCs (+ a Corrected note on the stale addendum); ARCHITECTURE; BENCHMARKS 2026-10-17\n  record; the round-5 report's item-10 note; changelog Changed; DECISIONS; LOGS.\n\nVerification: red first, then green; 15 one-at-a-time mutants, all caught; gofmt, vet,\ngolangci-lint, go-arch-lint; the redislimiter package through Docker under -race; the real-Redis\ncmd/gateway integration tests; the whole non-Docker suite; compat module; check-doc-paths; lychee;\na live run of the real recipe against a scratch Redis (S1a/S1r/S4) recorded in BENCHMARKS.md;\nreview lanes and a re-read as recorded in docs/agents/LOGS.md.\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>",
          "timestamp": "2026-10-17T07:35:49Z",
          "url": "https://github.com/kelvran/gateway/commit/5c7fa0ac034b91fc70aa1b26b9f9d79a42cccb62"
        },
        "date": 1792573580288,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 2.034111,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 3.955434,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 5.477965,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p99",
            "value": 4,
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
            "value": 202.776326,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 204.619515,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 206.603562,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p99",
            "value": 5,
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
            "value": 570.472408,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 577.270336,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 580.711272,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 52.39629,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 55.089777,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.749185,
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
            "value": 0.54687,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.972747,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.912716,
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
            "name": "S1r latency p50",
            "value": 3.450699,
            "unit": "ms"
          },
          {
            "name": "S1r latency p95",
            "value": 6.180179,
            "unit": "ms"
          },
          {
            "name": "S1r latency p99",
            "value": 8.712081,
            "unit": "ms"
          },
          {
            "name": "S1r overhead p50",
            "value": 2,
            "unit": "ms"
          },
          {
            "name": "S1r overhead p99",
            "value": 7,
            "unit": "ms"
          },
          {
            "name": "S1r redis round trips per request",
            "value": 5.001990049751244,
            "unit": "rtt/req"
          },
          {
            "name": "S1r redis commands per request",
            "value": 5.002985074626865,
            "unit": "cmd/req"
          },
          {
            "name": "S1r error rate",
            "value": 0,
            "unit": "%"
          },
          {
            "name": "S1r rps shortfall",
            "value": 0,
            "unit": "req/s"
          },
          {
            "name": "S4 latency p50",
            "value": 53.613266,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 55.098413,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 57.140826,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p50",
            "value": 2,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p99",
            "value": 5,
            "unit": "ms"
          },
          {
            "name": "S4 redis round trips per request",
            "value": 5.002321724709784,
            "unit": "rtt/req"
          },
          {
            "name": "S4 redis commands per request",
            "value": 5.003316749585406,
            "unit": "cmd/req"
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
      },
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
          "id": "5c7fa0ac034b91fc70aa1b26b9f9d79a42cccb62",
          "message": "perf(ratelimit): one gate script for RPM, TPM and tokens-per-day and one settlement script for both token dimensions -- 8.0 -> 5.0 Redis round trips per billable request; a refused request consumes nothing (round-5 item 10 phase 2b)\n\nAfter phase 2a a billable Redis-mode request still made three pre-call rate-limit calls, each\nconditional on the previous one, a fourth releasing the minute reservation on a tokens-per-day\nrefusal, and two settle calls after the response. The three become one script that reserves nothing\nunless all three dimensions admit, the two settles one script.\n\n- redislimiter: admitLuaSrc (KEYS: the RPM hash, the TPM TAT and history, the day TAT and\n  history) evaluates RPM, then TPM, then tokens-per-day -- the day cold cap lowered to the minute\n  cost it has just sized, dayColdStartCap's rule -- and reserves every dimension only on a full\n  admit; the RPM state is still persisted on every call (TTL refresh, Allow's own write), the token\n  taken only on an admit; the RPM part keeps the client clock and the GCRA parts Redis TIME, as before. Returns\n  {allowed, refused_by, retry_after, tpm_cost, tpd_cost, tpd_cold}, floats tostring()'d.\n  settlePairLuaSrc settles up to two dimensions in one EVAL (not a pipeline: go-redis's NOSCRIPT\n  fallback is inert inside one and a retried pipeline would settle twice).\n- ratelimit: RedisBackend gains Admit and SettleTokens over the new leaf package\n  ratelimit/admission (redislimiter keeps satisfying the interface structurally, without\n  importing ratelimit); KeyLimiter.Admit builds the request (per-model RPM/TPM keys, the day key\n  with the 1/24 cold cap) and maps the result with the cold-refusal tolerance; in memory it runs\n  the three buckets and, for parity, refunds the RPM token (TokenBucket.Credit) and releases the\n  minute reservation when a later dimension refuses. KeyLimiter.SettleTokens keeps finalize's\n  per-dimension gates and skips the call when nothing is configured. .go-arch-lint.yml declares the\n  leaf as the ratelimit-admission component the ratelimit component may depend on.\n- dataplane: checkRateLimit is one Admit call -- a backend error fails the whole gate open once\n  (one ratelimit_backend_unavailable line, one kelvran.ratelimit.fail_open increment, nothing\n  reserved); a day refusal keeps its wait; RPM and TPM refusals keep the backoff alone. finalize\n  settles both token dimensions with one SettleTokens call.\n- tests: redislimiter admit_test.go (real Redis: a fresh key reserves all three; nothing written\n  on a TPM, day or RPM refusal; off dimensions; the paired settle teaches the mean), ratelimit\n  admit_test.go (in-memory refund and release; the Redis request's keys and cold cap; refusal\n  mapping incl. the %.14g cold case; settle entries and gates), dataplane admit_gate_test.go (one\n  gate call, a day refusal reserving nothing, one fail-open for all dimensions, one paired settle,\n  two streaming twins); the three Redis-mode tokens-per-day tests and the fail-open table\n  re-pinned to the gate; four fakes extended (a fifth inherits by embedding) and one new gate fake;\n  the streaming harness gains a limiter-taking form.\n- docs: FAILURE-MODES R1 (the gate), R2/R2b historical with dated notes, the settle-discard bullet\n  and a double-settle known gap; TELEMETRY, metrics-and-logs, the how-to, config.md, caching,\n  glossary, troubleshooting, data-plane-api; Implemented notes on the distributed-rate-limiting\n  and TOCTOU RFCs (+ a Corrected note on the stale addendum); ARCHITECTURE; BENCHMARKS 2026-10-17\n  record; the round-5 report's item-10 note; changelog Changed; DECISIONS; LOGS.\n\nVerification: red first, then green; 15 one-at-a-time mutants, all caught; gofmt, vet,\ngolangci-lint, go-arch-lint; the redislimiter package through Docker under -race; the real-Redis\ncmd/gateway integration tests; the whole non-Docker suite; compat module; check-doc-paths; lychee;\na live run of the real recipe against a scratch Redis (S1a/S1r/S4) recorded in BENCHMARKS.md;\nreview lanes and a re-read as recorded in docs/agents/LOGS.md.\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>",
          "timestamp": "2026-10-17T07:35:49Z",
          "url": "https://github.com/kelvran/gateway/commit/5c7fa0ac034b91fc70aa1b26b9f9d79a42cccb62"
        },
        "date": 1792660124958,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 1.663448,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 3.083944,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 4.432333,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p99",
            "value": 3,
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
            "value": 202.352827,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 204.037191,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 205.357146,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p99",
            "value": 3,
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
            "value": 569.366057,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 574.170114,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 576.055013,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 52.312324,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 55.662377,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.75887,
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
            "value": 0.435421,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.758499,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.813468,
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
            "name": "S1r latency p50",
            "value": 3.003267,
            "unit": "ms"
          },
          {
            "name": "S1r latency p95",
            "value": 4.982337,
            "unit": "ms"
          },
          {
            "name": "S1r latency p99",
            "value": 6.309004,
            "unit": "ms"
          },
          {
            "name": "S1r overhead p50",
            "value": 2,
            "unit": "ms"
          },
          {
            "name": "S1r overhead p99",
            "value": 5,
            "unit": "ms"
          },
          {
            "name": "S1r redis round trips per request",
            "value": 5.001326699834163,
            "unit": "rtt/req"
          },
          {
            "name": "S1r redis commands per request",
            "value": 5.001990049751244,
            "unit": "cmd/req"
          },
          {
            "name": "S1r error rate",
            "value": 0,
            "unit": "%"
          },
          {
            "name": "S1r rps shortfall",
            "value": 0,
            "unit": "req/s"
          },
          {
            "name": "S4 latency p50",
            "value": 53.18236,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 54.553389,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 55.459927,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p99",
            "value": 3,
            "unit": "ms"
          },
          {
            "name": "S4 redis round trips per request",
            "value": 5.001326699834163,
            "unit": "rtt/req"
          },
          {
            "name": "S4 redis commands per request",
            "value": 5.001990049751244,
            "unit": "cmd/req"
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
      },
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
          "id": "5c7fa0ac034b91fc70aa1b26b9f9d79a42cccb62",
          "message": "perf(ratelimit): one gate script for RPM, TPM and tokens-per-day and one settlement script for both token dimensions -- 8.0 -> 5.0 Redis round trips per billable request; a refused request consumes nothing (round-5 item 10 phase 2b)\n\nAfter phase 2a a billable Redis-mode request still made three pre-call rate-limit calls, each\nconditional on the previous one, a fourth releasing the minute reservation on a tokens-per-day\nrefusal, and two settle calls after the response. The three become one script that reserves nothing\nunless all three dimensions admit, the two settles one script.\n\n- redislimiter: admitLuaSrc (KEYS: the RPM hash, the TPM TAT and history, the day TAT and\n  history) evaluates RPM, then TPM, then tokens-per-day -- the day cold cap lowered to the minute\n  cost it has just sized, dayColdStartCap's rule -- and reserves every dimension only on a full\n  admit; the RPM state is still persisted on every call (TTL refresh, Allow's own write), the token\n  taken only on an admit; the RPM part keeps the client clock and the GCRA parts Redis TIME, as before. Returns\n  {allowed, refused_by, retry_after, tpm_cost, tpd_cost, tpd_cold}, floats tostring()'d.\n  settlePairLuaSrc settles up to two dimensions in one EVAL (not a pipeline: go-redis's NOSCRIPT\n  fallback is inert inside one and a retried pipeline would settle twice).\n- ratelimit: RedisBackend gains Admit and SettleTokens over the new leaf package\n  ratelimit/admission (redislimiter keeps satisfying the interface structurally, without\n  importing ratelimit); KeyLimiter.Admit builds the request (per-model RPM/TPM keys, the day key\n  with the 1/24 cold cap) and maps the result with the cold-refusal tolerance; in memory it runs\n  the three buckets and, for parity, refunds the RPM token (TokenBucket.Credit) and releases the\n  minute reservation when a later dimension refuses. KeyLimiter.SettleTokens keeps finalize's\n  per-dimension gates and skips the call when nothing is configured. .go-arch-lint.yml declares the\n  leaf as the ratelimit-admission component the ratelimit component may depend on.\n- dataplane: checkRateLimit is one Admit call -- a backend error fails the whole gate open once\n  (one ratelimit_backend_unavailable line, one kelvran.ratelimit.fail_open increment, nothing\n  reserved); a day refusal keeps its wait; RPM and TPM refusals keep the backoff alone. finalize\n  settles both token dimensions with one SettleTokens call.\n- tests: redislimiter admit_test.go (real Redis: a fresh key reserves all three; nothing written\n  on a TPM, day or RPM refusal; off dimensions; the paired settle teaches the mean), ratelimit\n  admit_test.go (in-memory refund and release; the Redis request's keys and cold cap; refusal\n  mapping incl. the %.14g cold case; settle entries and gates), dataplane admit_gate_test.go (one\n  gate call, a day refusal reserving nothing, one fail-open for all dimensions, one paired settle,\n  two streaming twins); the three Redis-mode tokens-per-day tests and the fail-open table\n  re-pinned to the gate; four fakes extended (a fifth inherits by embedding) and one new gate fake;\n  the streaming harness gains a limiter-taking form.\n- docs: FAILURE-MODES R1 (the gate), R2/R2b historical with dated notes, the settle-discard bullet\n  and a double-settle known gap; TELEMETRY, metrics-and-logs, the how-to, config.md, caching,\n  glossary, troubleshooting, data-plane-api; Implemented notes on the distributed-rate-limiting\n  and TOCTOU RFCs (+ a Corrected note on the stale addendum); ARCHITECTURE; BENCHMARKS 2026-10-17\n  record; the round-5 report's item-10 note; changelog Changed; DECISIONS; LOGS.\n\nVerification: red first, then green; 15 one-at-a-time mutants, all caught; gofmt, vet,\ngolangci-lint, go-arch-lint; the redislimiter package through Docker under -race; the real-Redis\ncmd/gateway integration tests; the whole non-Docker suite; compat module; check-doc-paths; lychee;\na live run of the real recipe against a scratch Redis (S1a/S1r/S4) recorded in BENCHMARKS.md;\nreview lanes and a re-read as recorded in docs/agents/LOGS.md.\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>",
          "timestamp": "2026-10-17T07:35:49Z",
          "url": "https://github.com/kelvran/gateway/commit/5c7fa0ac034b91fc70aa1b26b9f9d79a42cccb62"
        },
        "date": 1792746162555,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 1.582486,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 3.343465,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 4.905716,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p99",
            "value": 4,
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
            "value": 202.337775,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 204.022416,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 205.275775,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p99",
            "value": 4,
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
            "value": 572.472574,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 578.051169,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 580.212132,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 52.271393,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 55.575849,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.580764,
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
            "value": 0.38192,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.714939,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.794972,
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
            "name": "S1r latency p50",
            "value": 2.192879,
            "unit": "ms"
          },
          {
            "name": "S1r latency p95",
            "value": 4.099959,
            "unit": "ms"
          },
          {
            "name": "S1r latency p99",
            "value": 6.338701,
            "unit": "ms"
          },
          {
            "name": "S1r overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S1r overhead p99",
            "value": 5,
            "unit": "ms"
          },
          {
            "name": "S1r redis round trips per request",
            "value": 5.001990049751244,
            "unit": "rtt/req"
          },
          {
            "name": "S1r redis commands per request",
            "value": 5.002985074626865,
            "unit": "cmd/req"
          },
          {
            "name": "S1r error rate",
            "value": 0,
            "unit": "%"
          },
          {
            "name": "S1r rps shortfall",
            "value": 0,
            "unit": "req/s"
          },
          {
            "name": "S4 latency p50",
            "value": 52.491847,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 53.850988,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 57.134332,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p99",
            "value": 5,
            "unit": "ms"
          },
          {
            "name": "S4 redis round trips per request",
            "value": 5.001326699834163,
            "unit": "rtt/req"
          },
          {
            "name": "S4 redis commands per request",
            "value": 5.001990049751244,
            "unit": "cmd/req"
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
      },
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
          "id": "96915141ab7f232ff26f650aaf5960db7d1f8393",
          "message": "refactor(redis): one go-redis client for the rate limiter, the budget and the ledger when they share a Redis, and the request builders their Redis paths use (round-5 item 10 phase 2c-1)\n\nPhase 2c reaches the report's 2.0 round trips by fusing the pre-call scripts\ninto one EVAL and the post-call scripts into another, which needs ONE client\nunder the three stores. This first commit lays that groundwork without\nchanging what a request does: it still costs 5.0 round trips.\n\n- cmd/gateway: when rate_limit.redis_addr and budget.redis_addr name the same\n  Redis (same address, username, resolved password, TLS; the ledger follows\n  the budget) openSharedRedis builds ONE client with exactly ONE hook labelled\n  \"shared\" (go-redis appends hooks, so one per store would count every command\n  three times). The match is conservative: two spellings of one host keep a\n  client per store and their own labels. sharedRedis hands each store a\n  release; the client closes when the last one is released, so Pipeline.Close\n  closes it once without an ErrClosed.\n- redislimiter / redisbudget / redisledger: NewOn constructors over a client\n  somebody else owns, with a closeFn (client.Close for Open, the release for\n  NewOn); Open is unchanged for callers.\n- Request builders and result mappers extracted from the paths that used them,\n  which now call them: ratelimit.KeyLimiter.PlanAdmit / PlanSettle and the\n  package-level ratelimit.MapAdmission; budget.PlanReserve / MapReservation / PlanReconcile and\n  Tracker.NoteAdjustFailure; redisledger.Store.PlanRecord; the dataplane's\n  ledgerEntry / noteLedgerFailure. A fused path reuses them instead of\n  copying the arithmetic.\n- tests: shared_redis_test.go (the match table, eligibility from config, the\n  refcounted close, one count per command under \"shared\", and two real-Redis\n  integration tests: 20 round trips over four billable requests all under\n  \"shared\" and the pool's connections gone from the server after\n  Pipeline.Close, and per-store labels unchanged when the addresses differ), NewOn\n  tests in all three stores, table tests for every extracted function.\n- docs: config.md, config.example.yaml, metrics-and-logs.md, TELEMETRY.md,\n  FAILURE-MODES.md (the one shared pool), the virtual-keys how-to, ARCHITECTURE.md, changelog, the round-5 report note,\n  BENCHMARKS.md record, DECISIONS (the owner's four answers: 2c in three\n  commits, single fused EVALs instead of pipelines, concurrency check first,\n  a budget refusal keeps today's behaviour), LOGS.\n\nVerification: red first, then green; 24 one-at-a-time mutants all caught (two\nfirst tripped the compiler and were re-run compile-safe, one survived a\ntautological test until a test pinned the explicit values, and the Docker\nones were re-run with green baselines and real failure text);\ngofmt, vet, golangci-lint, go-arch-lint; the three stores through Docker under\n-race; the whole non-Docker suite; compat module; check-doc-paths; lychee; a\nlive run of the real recipe (S1a/S1r/S4) at 5.0; review lanes as recorded in\ndocs/agents/LOGS.md.\n\nSigned-off-by: Sairam Ugge <uggesairam0000@gmail.com>\nCo-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>",
          "timestamp": "2026-10-23T11:06:44Z",
          "url": "https://github.com/kelvran/gateway/commit/96915141ab7f232ff26f650aaf5960db7d1f8393"
        },
        "date": 1792831017168,
        "tool": "customSmallerIsBetter",
        "benches": [
          {
            "name": "S1a latency p50",
            "value": 1.45974,
            "unit": "ms"
          },
          {
            "name": "S1a latency p95",
            "value": 2.865693,
            "unit": "ms"
          },
          {
            "name": "S1a latency p99",
            "value": 3.999642,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p50",
            "value": 0,
            "unit": "ms"
          },
          {
            "name": "S1a overhead p99",
            "value": 3,
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
            "value": 202.311761,
            "unit": "ms"
          },
          {
            "name": "S1b latency p95",
            "value": 204.101455,
            "unit": "ms"
          },
          {
            "name": "S1b latency p99",
            "value": 205.18058,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p50",
            "value": 1,
            "unit": "ms"
          },
          {
            "name": "S1b overhead p99",
            "value": 3,
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
            "value": 569.316609,
            "unit": "ms"
          },
          {
            "name": "S2 latency p95",
            "value": 574.357139,
            "unit": "ms"
          },
          {
            "name": "S2 latency p99",
            "value": 576.205158,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p50",
            "value": 52.400685,
            "unit": "ms"
          },
          {
            "name": "S2 ttft p99",
            "value": 55.237773,
            "unit": "ms"
          },
          {
            "name": "S2 inter-chunk p99",
            "value": 11.743976,
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
            "value": 0.483726,
            "unit": "ms"
          },
          {
            "name": "S3 latency p95",
            "value": 0.751484,
            "unit": "ms"
          },
          {
            "name": "S3 latency p99",
            "value": 201.995369,
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
            "name": "S1r latency p50",
            "value": 3.443485,
            "unit": "ms"
          },
          {
            "name": "S1r latency p95",
            "value": 6.017434,
            "unit": "ms"
          },
          {
            "name": "S1r latency p99",
            "value": 7.913488,
            "unit": "ms"
          },
          {
            "name": "S1r overhead p50",
            "value": 2,
            "unit": "ms"
          },
          {
            "name": "S1r overhead p99",
            "value": 6,
            "unit": "ms"
          },
          {
            "name": "S1r redis round trips per request",
            "value": 5.001326699834163,
            "unit": "rtt/req"
          },
          {
            "name": "S1r redis commands per request",
            "value": 5.001990049751244,
            "unit": "cmd/req"
          },
          {
            "name": "S1r error rate",
            "value": 0,
            "unit": "%"
          },
          {
            "name": "S1r rps shortfall",
            "value": 0,
            "unit": "req/s"
          },
          {
            "name": "S4 latency p50",
            "value": 53.595949,
            "unit": "ms"
          },
          {
            "name": "S4 latency p95",
            "value": 54.750813,
            "unit": "ms"
          },
          {
            "name": "S4 latency p99",
            "value": 55.493403,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p50",
            "value": 2,
            "unit": "ms"
          },
          {
            "name": "S4 overhead p99",
            "value": 3,
            "unit": "ms"
          },
          {
            "name": "S4 redis round trips per request",
            "value": 5.000663349917081,
            "unit": "rtt/req"
          },
          {
            "name": "S4 redis commands per request",
            "value": 5.000663349917081,
            "unit": "cmd/req"
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