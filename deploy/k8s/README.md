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

# 3. Turn step 1's .env into the REAL gateway-upstream-credentials Secret
#    -- base/secret-placeholder.yaml (what step 2 just applied) is an
#    EMPTY stringData stub kept only so kustomize has something to
#    reference without erroring; on its own it ships an empty Secret,
#    and every upstream provider call fails auth. On every cluster type
#    OTHER than EKS (see "Secrets management" below for the
#    EKS/ExternalSecrets path):
kubectl create secret generic gateway-upstream-credentials \
  --namespace kelvran --from-env-file=.env \
  --dry-run=client -o yaml | kubectl apply -f -

# 4. envFrom is read only at container START -- the Pods `kubectl apply
#    -k base/` already created came up against the still-empty
#    placeholder, so they need a restart to pick up step 3's real values.
kubectl rollout restart deployment/gateway -n kelvran
```

**Confirmed by direct testing against a real cluster, not assumed: re-running
`kubectl apply -k base/` AFTER step 3 silently wipes the real Secret's data
back to empty again** — `secret-placeholder.yaml`'s own `stringData: {}`
always wins the 3-way merge against whatever step 3 wrote, regardless of
which command ran more recently. There is no ordering that makes this safe
once both objects share the same name — every time you re-apply `base/`
(a config change, an upgrade, CI re-running the same command), redo step 3
immediately afterward, then step 4. If you already have real credentials
under management (ESO, Vault, a script), delete `secret-placeholder.yaml`
from `kustomization.yaml`'s `resources:` list entirely rather than fighting
this footgun on every re-apply.

## Real, disclosed limitations — read before applying to a live cluster

- **Multi-replica deployment (`replicas: 2`, this manifest's own
  default) needs Redis for budget AND identity to stay correct — set
  BOTH knobs, not just one.** `gateway` is otherwise stateless per
  request. Three subsystems have their own hot state, each independently
  gated by its own config knob:
  - `budget.redis_addr` (`internal/budget/redisbudget`) — a virtual
    key's cumulative spend/cap enforcement runs atomically inside Redis
    via Lua, genuinely consistent across every replica sharing that
    Redis instance. Unset (the default): budget is in-memory-only per
    replica, or bbolt-backed at `budget.persist_path` (single-process
    restart durability ONLY — a second replica never sees the first
    one's spend at all; see below).
  - `admin.redis_addr` (`internal/identity/redisstore`) — persists
    admin-API-created/rotated virtual keys to a shared Redis Hash rather
    than a single-process bbolt file, so a freshly-started (or
    restarted) replica loads the same key set every other replica
    already has.
  - `config_propagation.redis_addr` (`internal/configpropagation`) —
    a SEPARATE, complementary mechanism: a Redis pub/sub channel that
    pushes a LIVE admin mutation (virtual-key upsert/delete/rotate,
    deployment-weight change) to every OTHER already-running replica,
    closing the gap `admin.redis_addr` alone cannot: two replicas each
    holding their own already-built in-memory `Verifier`/router state
    never re-consult a Store mid-flight, so without this, a stale
    replica keeps authenticating a revoked/rotated credential (or
    rejecting a brand-new one) until its own restart. Set this
    alongside `admin.redis_addr`, not instead of it.

  **If you scale past `replicas: 1` without setting all three:** budget
  enforcement silently degrades to per-replica-independent tracking (a
  virtual key's real cap becomes `N × BudgetUSD` in effect, split
  unevenly across whichever replica each request happens to land on),
  and a live admin mutation on one replica is invisible to the others
  until they each individually restart. This base manifest ships
  `replicas: 2` as its own default specifically to make this a decision
  you have to engage with (via `config.example.yaml`'s commented-out
  `redis_addr` lines) rather than something 1-replica silently masked.

  `budget.persist_path`/`admin.persist_path` (bbolt) remain
  single-process-only by design — this base manifest does not mount a
  PVC for either at all. If you add one, drop `deployment.yaml`'s
  `replicas` back to 1 and restore `strategy: { type: Recreate }` (an
  RWO volume can only ever be mounted by one Pod at a time — a rolling
  update would deadlock waiting for the old Pod to release it); a
  StatefulSet with per-pod `volumeClaimTemplates` instead gives each
  replica its own **separate** bbolt file, still not a merged/consistent
  one, so it does not substitute for the Redis knobs above if your goal
  is genuine cross-replica consistency.
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
- **Added 2026-09-18, a real Checkov hardening pass**: `base/` now sets
  a real `namespace: kelvran` — change it to match your own cluster's
  naming convention. **Corrected same day**: the first pass only set
  this via `kustomization.yaml`'s own top-level `namespace:` transform,
  which is invisible to Checkov's own `kubernetes-checkov` CI job — that
  job scans these raw `.yaml` files directly, never through
  `kustomize build`, so the transform-only version left `CKV_K8S_21`
  open despite CI going green. Fixed by additionally setting
  `namespace: kelvran` directly in each raw file's own `metadata:`
  block; the kustomize-level transform stays too (harmless, and still
  what makes `overlays/eks-irsa`'s own `secretstore.yaml`/
  `externalsecret.yaml` — listed directly in that overlay's own
  `resources:`, not pulled in via `../../base` — need the same explicit
  per-file `namespace:` to actually match, since a referencing
  kustomization's `namespace:` field doesn't cascade to siblings listed
  directly in its own `resources:` list).
  `base/networkpolicy.yaml` gives the gateway Pod a real NetworkPolicy
  (ingress on 8080 from anywhere, matching its own client-facing role;
  egress to anywhere, since upstream LLM providers have no stable,
  documented IP/CIDR range to scope a tighter rule to — a real,
  disclosed limitation, not silently treated as solved); the container
  spec gained its own `securityContext` (`allowPrivilegeEscalation:
  false`, every Linux capability dropped, `readOnlyRootFilesystem:
  true` — safe for this manifest's own default in-memory-only
  configuration; adding `persist_path` persistence later needs its own
  writable volume mount despite this setting) and `seccompProfile:
  RuntimeDefault`; `automountServiceAccountToken: false` at the pod
  spec level (the gateway process itself never calls the Kubernetes
  API — safe alongside IRSA, whose own credential injection is a
  distinct mechanism from this token, per the field's own comment in
  `deployment.yaml`); and the image reference is pinned to the real,
  currently-published `gateway/v0.12.0` release by digest — re-pin this
  yourself before applying, it goes stale the moment a newer version
  ships, same as any other pinned dependency. **Deliberately NOT
  changed**, matching this repo's own established "accepted risk, not
  silently fixed" posture for a Checkov finding that contradicts an
  already-reasoned-about design choice: `envFrom.secretRef` (Checkov
  prefers file-mounted secrets over environment variables) stays as-is
  — `gateway`'s own config model reads every credential via
  `os.Getenv`, and switching to file-mounted secrets would need a real
  code change to the gateway itself, not just a manifest edit.
