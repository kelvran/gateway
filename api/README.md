# api/

The only shared surface between `gateway` (Go) and `evals` (Python). No shared database, no shared library — this directory and nothing else.

- `gatewayevents/v1/` — **real**, per `docs/rfcs/2026-09-03-api-gatewayevents-contract.md`: `GatewayDecisionEvent`, the structured outcome of one gateway request (auth/rate-limit/budget/model-allow decisions), joinable to the OTel span carrying the same request's `gen_ai.*`/cost/cache data via `trace_id`/`span_id`.
- `otel/` — **still a placeholder, deliberately**. Gateway already exports real OTLP spans (`gateway/internal/telemetry`); a custom protobuf schema for that data would duplicate a wire format that already exists, per the OTel tracing RFC's own reasoning (`docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md`) and reaffirmed by the `gatewayevents` contract's own RFC. If `evals` ever needs to consume gateway's spans, the mechanism is a real OTel Collector fan-out, not this directory.

Both languages generate their bindings from the same `.proto` source with [`buf`](https://buf.build/) (`buf.yaml`/`buf.gen.yaml` in this directory); neither side imports the other's source code. `buf breaking` runs in CI against this directory — an incompatible schema change fails the build automatically rather than drifting silently, which is the whole reason this boundary is a versioned contract and not a shared library. See `docs/decisions/0003-go-python-split.md` for the reasoning.

Generated code is committed (`gateway/api/gatewayevents/v1/*.pb.go`, `evals/evals/contracts/gatewayevents/v1/*_pb2.py`), not generated-in-CI-only — `make gen-proto` regenerates it locally after a schema change; `make check-proto` (part of `make verify`) fails the build if committed generated code has drifted from its `.proto` source.
