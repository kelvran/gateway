# Kubernetes / Production Deployment — Research (2026-09-14)

> **Recovery note:** the synthesizing subagent explicitly declined to write this file itself, citing a harness rule against writing report files, and returned the full structured result for the parent session to persist instead.

**Scope:** Real Kubernetes/production deployment for Kelvran — genuinely unstarted territory (`docs/operations/DEPLOY.md`'s K8s section is explicitly "(Intended shape.)" only).

**Already shipped (not re-litigated):** a real, signed, SBOM/SLSA-provenance-attested container image at `ghcr.io/kelvran/gateway`, non-root (`65532:65532`); a real `/healthz` liveness endpoint; OTLP telemetry export (config-flip activated); Docker Compose as the only real deployment shape today.

## Findings

1. **For a single-service, single-maintainer project, start with plain `kubectl apply -f` or Kustomize — reserve Helm for third-party charts only.** This matches the explicit "don't over-engineer" recommendation for the 1-10-app-shape bucket. *(Medium confidence — the specific tooling-choice source was a single non-authoritative blog, 2-1 vote — treat as a reasonable default, not settled fact.)*

2. **Plain Kubernetes Secrets are only base64-encoded, not encrypted, and readable by anyone with pod-create authorization in the namespace — External Secrets Operator (ESO) is the standard vendor-neutral answer.** ESO is a CRD-based, cluster-side controller supporting AWS Secrets Manager, HashiCorp Vault, and GCP/Azure/IBM secret managers — the right fit for Kelvran's AWS creds and virtual-key hashes without vendor lock-in, matching its self-hosted, deploy-anywhere positioning. **`build_now`** as a documented decision; **`not_yet`** for an actual live ESO+backend deployment, pending a real cluster.

3. **The bbolt single-instance budget-persistence constraint forces a real, concrete tradeoff, not a hand-wave.** Kelvran's gateway is otherwise stateless per Kubernetes' own Deployment-vs-StatefulSet guidance — except for that one file. A Deployment bound to a single ReadWriteOnce PVC forces `replicas=1` and `strategy: Recreate` (rolling updates can't work with an RWO volume mounted to only one Pod at a time). Scaling to 2+ replicas with per-pod storage isolation would need a StatefulSet with `volumeClaimTemplates` — but that only gives each replica its **own separate** bbolt file (ordinal-tied); it does **not** create cross-replica budget consistency. True consistency still needs a shared store (e.g. extending the existing Redis-backed rate limiter to budgets too). **`build_now`**: a small, honest `replicas=1` Deployment manifest with the limitation documented. **`not_yet`**: real 2+ replica HA, pending either a StatefulSet-based partial fix or a shared budget store.

4. **Liveness and readiness probes must check fundamentally different things — and the real anti-pattern is dangerous, not cosmetic.** Liveness should check *only* local/process state (`/healthz` already does this correctly) — a liveness probe hitting an external dependency causes simultaneous restarts across every pod on a transient blip, plus a reconnection thundering herd on recovery. Readiness may check dependencies, but must **never** check one identically shared across every replica (a shared Redis, or the upstream LLM provider) — a shared-dependency blip fails every instance's readiness in lockstep, emptying the Service's EndpointSlice and routing zero traffic, which is worse than doing nothing. **`build_now`**: a new, pod-local-only readiness probe, explicitly never checking Redis/upstream reachability.

5. **Go 1.25's container-aware `GOMAXPROCS` only activates on a real CPU *limit* (not just a request), and Go has no native cgroup-memory-aware `GOMEMLIMIT` equivalent at all.** A pod setting only a CPU request (a common burst-friendly pattern) gets zero `GOMAXPROCS` benefit and still defaults to the full node core count. The memory side matters more: exceeding a K8s memory *limit* triggers a hard OOM-kill (process termination), while exceeding a CPU limit only causes throttling (recoverable) — making memory misconfiguration the higher-severity risk for this streaming Go binary. `golang/go#75164` (native cgroup-memory-awareness) remains open/unimplemented — teams must set `GOMEMLIMIT` manually or via the `automemlimit` library (defaults to 90% of the detected cgroup limit). **`build_now`**: pin Go ≥1.25 (verify current pin first) and add `automemlimit` or a manual `GOMEMLIMIT` — a concrete, cluster-independent code change. Final numeric CPU/memory sizing itself is **`not_yet`**, pending real streaming-load data.

## Open Questions

- What Go version does `gateway/go.mod` and the Dockerfile builder stage currently pin to — already ≥1.25, or does the toolchain need a bump first to get free `GOMAXPROCS` container-awareness?
- Which secret backend would a future `ExternalSecrets` `SecretStore` actually target — self-hosted Vault (matching the self-hosted, no-lock-in positioning) vs. a specific cloud secrets manager? Unmade decision, determines the concrete CRD to author.
- Research sub-question 6 (how Envoy AI Gateway, LiteLLM's own Helm chart, or Kong AI Gateway handle this exact shape) returned zero surviving verified claims — a genuine coverage gap, not "no gap found." Worth a dedicated follow-up pass.
- What concrete trigger would justify moving past "`replicas=1` with a documented bbolt limitation" toward either a StatefulSet or a shared budget store — when does 2+ replica HA become a real requirement, not a hypothetical one?

## Caveats

The Kustomize-vs-Helm tooling choice and ESO's multi-cloud-portability claim both rest on 2-1 split votes, not unanimous verification — treat as reasonable defaults, not settled fact. All Go-runtime (`GOMAXPROCS`/`GOMEMLIMIT`) and core Kubernetes-mechanics claims (Secrets encoding, StatefulSet/Deployment semantics, PV/PVC behavior, probe semantics) are high-confidence and unanimous. Go 1.25's container-aware `GOMAXPROCS` shipped Aug 2025 and `GOMEMLIMIT`'s proposal was still open as of this research date — both stable today, but re-check if either slips more than ~1-2 Go release cycles. Several plausible-sounding claims were explicitly refuted under adversarial verification and are excluded above: "Deployment not StatefulSet is Kubernetes' own recommended single-instance pattern" (contested, 1-2), "Go GC caps itself at ~50% CPU bounding worst-case slowdown to 2x" (unsupported, 0-3), a specific 3-dependency readiness-design prescription (0-3), and "Kustomize needs no separate install, built into kubectl since v1.14" (0-3, do not rely on this specific claim without re-checking your kubectl version).

## Synthesis: build_now vs not_yet

| Item | Verdict | Why |
|---|---|---|
| `kubectl apply -f`/Kustomize for Kelvran's own manifests | **build_now** | Right starting complexity for a 1-service, single-maintainer project |
| ESO as the documented secrets-management decision | **build_now** | Vendor-neutral, matches self-hosted positioning |
| Live ESO + secret backend deployment | **not_yet** | Needs a real cluster to deploy into |
| `replicas=1` Deployment manifest, bbolt limitation documented | **build_now** | Small, honest, committable today |
| 2+ replica HA (StatefulSet or shared budget store) | **not_yet** | Needs a real multi-replica requirement to justify |
| Pod-local-only readiness probe | **build_now** | Avoids the correlated-failure anti-pattern |
| Go ≥1.25 pin + `automemlimit`/`GOMEMLIMIT` | **build_now** | Concrete, cluster-independent code change |
| Final CPU/memory sizing numbers | **not_yet** | Needs real streaming-load data to calibrate |
