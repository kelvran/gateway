# Kubernetes manifests for `gateway`

Added 2026-09-14, per `docs/upgrade-research/kubernetes-production-deployment-2026-09-14.md`.
Previously `docs/operations/DEPLOY.md`'s Kubernetes section was explicitly
"(Intended shape.)" only — no manifests existed anywhere in this repo. This
directory closes that gap for a single-service, single-maintainer deployment
shape; it has never been applied against a real, live cluster (no cluster
exists yet to verify against) — treat it as a real starting point, not a
tested-in-production artifact.

**Updated 2026-09-17/18**, per the backlog Phase 7 infra round: `base/`
gained a dedicated `serviceaccount.yaml` (needed for IRSA to have anything
to attach a trust policy to), and a real `overlays/eks-irsa/` now exists —
see "Which annotation for which cluster type" and "Secrets management"
below.

## Which annotation for which cluster type

`base/serviceaccount.yaml` carries BOTH `eks.amazonaws.com/role-arn` and
`iam.amazonaws.com/role` annotations, always — this is safe on every cluster
type, since only the mechanism actually running in-cluster reads its own
annotation:

- **EKS**: use `eks.amazonaws.com/role-arn`, set to
  `terraform/irsa-trust/`'s own `role_arn` output. Apply
  `overlays/eks-irsa/` (not `base/` alone) to also get a real ESO
  `SecretStore`/`ExternalSecret` pulling from AWS Secrets Manager via IRSA
  — see that overlay's own manifests for the exact shape.
- **Self-managed Kubernetes (kiam/kube2iam)**: use
  `iam.amazonaws.com/role`, set to whatever role ARN your own
  kiam/kube2iam deployment is configured to allow this annotation to
  assume — kiam/kube2iam typically assumes via the EC2 instance profile
  the proxy itself runs under, a different trust principal than IRSA's
  OIDC provider, so `terraform/irsa-trust/` does not cover this path;
  provision that role yourself, matching your own kiam/kube2iam
  installation's documented trust requirement. Apply `base/` alone (bring
  your own Secret, same as before this section existed — ESO is optional
  here, not required).
- **ECS/Fargate**: neither annotation applies — see `../ecs/` instead,
  which uses Fargate's own native task-role secrets mechanism, no
  IRSA/kiam/kube2iam/ESO equivalent needed at all.

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
  Secret objects) — EXCEPT on EKS, where a real ESO wiring now exists.**
  Plain Kubernetes Secrets are only base64-encoded, not encrypted, and
  readable by anyone with pod-create authorization in the namespace.
  [External Secrets Operator](https://external-secrets.io/) (CRD-based,
  supports AWS Secrets Manager, HashiCorp Vault, GCP/Azure/IBM secret
  managers) is the vendor-neutral answer this repo picked. As of the
  backlog Phase 7 infra round, `overlays/eks-irsa/` is a REAL, schema-
  validated (via `kubeconform` against the live `external-secrets.io`
  CRD schemas, not just eyeballed) `SecretStore`+`ExternalSecret` pair —
  authenticating via IRSA (no static AWS key anywhere), targeting AWS
  Secrets Manager, producing a real `gateway-upstream-credentials` Secret
  under the exact name `deployment.yaml`'s `envFrom` already references.
  It has still never been applied against a real cluster (same
  disclosure as everything else in this directory) — the schema
  validation proves the manifests are well-formed, not that they behave
  correctly against a live ESO controller. On any OTHER cluster type
  (self-managed K8s without ESO, or ECS — see "Which annotation for
  which cluster type" above), `base/secret-placeholder.yaml` is still
  the plain `Secret` stub to replace with your own process.
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
