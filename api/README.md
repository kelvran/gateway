# api/

The only shared surface between `gateway` (Go) and `evals` (Python). No shared database, no shared library — this directory and nothing else.

- `otel/` — OTel GenAI semantic-convention schema, as protobuf.
- `gatewayevents/` — cost/usage/decision event schema (the `gatewayevents` contract Evals ingests from Gateway's production telemetry), as protobuf.

Both languages generate their bindings from the same `.proto` source with [`buf`](https://buf.build/); neither side imports the other's source code. Once real `.proto` files exist here, `buf breaking` runs in CI against this directory specifically — an incompatible schema change fails the build automatically rather than drifting silently, which is the whole reason this boundary is a versioned contract and not a shared library. See `docs/decisions/0003-go-python-split.md` for the reasoning.

No `.proto` files exist yet — this directory is a placeholder created ahead of code so `gateway/ARCHITECTURE.md`, `evals/ARCHITECTURE.md`, and the root `ARCHITECTURE.md` have a real path to point at. The actual schema definitions are written when Gateway's Phase 0 MVP (see `gateway/ARCHITECTURE.md`) needs its first OTel span shape.
