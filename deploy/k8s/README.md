# Kubernetes manifests for `gateway`

Added 2026-09-14, per `docs/upgrade-research/kubernetes-production-deployment-2026-09-14.md`.
Previously `docs/operations/DEPLOY.md`'s Kubernetes section was explicitly
"(Intended shape.)" only — no manifests existed anywhere in this repo. This
directory closes that gap for a single-service, single-maintainer deployment
shape; it has never been applied against a real, live cluster (no cluster
exists yet to verify against) — treat it as a real starting point, not a
tested-in-production artifact.

## Why plain Kustomize, not Helm

For a 1-service, single-maintainer project, `kubectl apply -k` / Kustomize is
the right starting complexity — Helm is reserved for consuming third-party
charts, not for templating this project's own single Deployment+Service.
Revisit only if a genuine multi-environment templating need appears.

## Usage

```sh
# 1. Create the (gitignored, never committed) files kustomization.yaml
#    generates a ConfigMap/Secret from:
cp ../../gateway/config.example.yaml config.yaml   # fill in real values
cp ../../.env.example .env                          # fill in real API keys

# 2. Review base/deployment.yaml's resource requests/limits against your
#    own real traffic before applying anything to a live cluster.

kubectl apply -k base/
```

## Real, disclosed limitations — read before applying to a live cluster

- **`replicas: 1` is deliberate, not a placeholder.** `gateway` is otherwise
  stateless per request, except for one file: `budget.persist_path`
  (bbolt), when configured. A Deployment bound to a single ReadWriteOnce
  PVC forces `replicas: 1` and `strategy: Recreate` — a rolling update
  can't work with an RWO volume mounted to only one Pod at a time. This
  base manifest does **not** mount a PVC at all (`budget.persist_path`
  is left unset in `config.example.yaml`, so budget state is in-memory-only
  and resets on every restart) — add one yourself, matching the
  `Recreate` strategy already set here, if you need persistence. Scaling
  to 2+ replicas with real cross-replica budget consistency needs a
  shared store (e.g. extending the existing Redis-backed rate limiter to
  budgets too) — not built here; a StatefulSet with per-pod
  `volumeClaimTemplates` gives each replica its own **separate** bbolt
  file, not a merged/consistent one.
- **Secrets management: bring your own ExternalSecrets (or your own
  Secret objects).** Plain Kubernetes Secrets are only base64-encoded,
  not encrypted, and readable by anyone with pod-create authorization in
  the namespace. The vendor-neutral, matches-this-project's-own-
  self-hosted-deploy-anywhere-positioning answer is
  [External Secrets Operator](https://external-secrets.io/) (CRD-based,
  supports AWS Secrets Manager, HashiCorp Vault, GCP/Azure/IBM secret
  managers) — a documented decision, not a live deployment: no
  `ExternalSecret`/`SecretStore` manifest is included here, since which
  backend to target is a real choice tied to where you actually run.
  `base/secret-placeholder.yaml` is a plain `Secret` stub, meant to be
  replaced (by ESO, Sealed Secrets, or your own process), never applied
  as-is with real credentials committed alongside it.
- **CPU/memory sizing numbers in `base/deployment.yaml` are illustrative,
  not measured.** No real streaming-load data exists yet to calibrate
  against — treat them as a starting point to load-test against your own
  traffic, not a validated recommendation.
- **`GOMEMLIMIT` is already handled in code** (`cmd/gateway/main.go`,
  via `github.com/KimMachineGun/automemlimit`) — it reads the real
  cgroup memory limit at startup and sets `GOMEMLIMIT` to 90% of it
  automatically. This manifest's own `resources.limits.memory` is what
  that mechanism reads; setting a memory limit here is required for the
  in-code mechanism to do anything at all (outside a cgroup limit, Go's
  GC runs unbounded, per stdlib default).
