# Deploy the gateway to Kubernetes with Kustomize

This page takes an operator from a source checkout to a running `gateway` Deployment on a Kubernetes cluster, using the plain Kustomize base at `deploy/k8s/base/` and, on EKS, the `deploy/k8s/overlays/eks-irsa/` overlay. It is for platform or SRE engineers who already run a cluster and have `kubectl` access to it.

**Use this when** you want the gateway on Kubernetes and are willing to supply your own `config.yaml`, provider credentials, and (for more than one replica) a Redis endpoint.

## Prerequisites

- A Kubernetes cluster and a `kubectl` that supports `kubectl apply -k`.
- A checkout of this repository. The manifests ship only in the source tree; there is no published chart (see [Not available today](#not-available-today)).
- A gateway `config.yaml`. Start from [`gateway/config.example.yaml`](../../../gateway/config.example.yaml); every key is described in the [configuration reference](../../reference/config.md).
- Provider credentials for every deployment your config names (`api_key_env`, `access_key_id_env`, `secret_access_key_env`). See [Provider credentials](../provider-credentials.md).
- For `replicas: 2` (the base default): a reachable Redis. The base ships no Redis.
- For the EKS overlay only: External Secrets Operator already installed in the cluster, Terraform, and the cluster's OIDC provider registered in IAM.
- Optional: `cosign` and `gh` to verify the image before you pin it.

## What the base applies

`deploy/k8s/base/kustomization.yaml` sets `namespace: kelvran` and lists seven resources:

| File | Object | Key settings |
|---|---|---|
| `namespace.yaml` | Namespace `kelvran` | |
| `deployment.yaml` | Deployment `gateway` | `replicas: 2`, `terminationGracePeriodSeconds: 60`, preferred anti-affinity on `kubernetes.io/hostname`, `automountServiceAccountToken: false`, runs as UID/GID 65532, `readOnlyRootFilesystem: true`, all capabilities dropped, `preStop.sleep.seconds: 5`, container port 8080 named `http` |
| `service.yaml` | Service `gateway` | `ClusterIP`, port 8080 to target port `http` |
| `secret-placeholder.yaml` | Secret `gateway-upstream-credentials` | Opaque, `stringData: {}` (empty) |
| `serviceaccount.yaml` | ServiceAccount `kelvran-gateway` | Placeholder annotations `eks.amazonaws.com/role-arn` and `iam.amazonaws.com/role` |
| `networkpolicy.yaml` | NetworkPolicy `kelvran-gateway` | Ingress TCP 8080 from anywhere; egress to anywhere |
| `poddisruptionbudget.yaml` | PodDisruptionBudget `gateway` | `minAvailable: 1`, `unhealthyPodEvictionPolicy: AlwaysAllow` |

A `configMapGenerator` builds ConfigMap `gateway-config` from a file named `config.yaml` in the same directory as `kustomization.yaml`. The Deployment mounts it read-only at `/config.yaml`, which matches the image's `CMD ["-config", "/config.yaml"]`. Credentials reach the container through `envFrom.secretRef.name: gateway-upstream-credentials`. Both the liveness and the readiness probe are `GET /healthz` on port `http`. Resources are requests `cpu: 500m` / `memory: 256Mi` and limits `cpu: 1` / `memory: 512Mi`; the memory limit matters because the gateway sets `GOMEMLIMIT` to 90% of the cgroup limit at startup. The sizing is a starting point, not a measurement.

The admin API binds `127.0.0.1:8081` inside the container by default and only when `admin.token_env` is set. No Service exposes it. See the [admin API reference](../../reference/admin-api.md).

## Steps (any cluster type)

Run these from the repository root.

1. **Create the config file next to `kustomization.yaml`.** The generator resolves `config.yaml` relative to `deploy/k8s/base/`, not to your shell. The file is gitignored and absent in a fresh checkout; `kubectl apply -k` fails with a "no such file" error until it exists.

   ```bash
   cp gateway/config.example.yaml deploy/k8s/base/config.yaml
   ```

   Edit it. Keep only the deployments and virtual keys you want; the example's virtual keys are public and must not reach a real cluster.

2. **Decide on replicas.** With `replicas: 2` and no Redis, each replica tracks budgets on its own (a cap of N becomes up to 2N in effect), and an admin mutation (a virtual-key upsert, rotate or delete) made on one replica never reaches the other and is lost when the replica that made it restarts. Either set the Redis keys below in `config.yaml`, or change `replicas` to `1` in `deploy/k8s/base/deployment.yaml`.

   ```yaml
   budget:
     redis_addr: "<redis-host>:6379"
   admin:
     token_env: "KELVRAN_ADMIN_TOKEN"
     redis_addr: "<redis-host>:6379"
   config_propagation:
     redis_addr: "<redis-host>:6379"
     signing_secret_env: "KELVRAN_CONFIG_PROPAGATION_SIGNING_SECRET"
   rate_limit:
     redis_addr: "<redis-host>:6379"
   ```

   `config_propagation.signing_secret_env` is required once `config_propagation.redis_addr` is set; the gateway refuses to start without it. Each section also accepts `redis_password_env`, `redis_username` and `redis_tls`. The environment variables these keys name (`KELVRAN_ADMIN_TOKEN`, `KELVRAN_CONFIG_PROPAGATION_SIGNING_SECRET`, any `redis_password_env`) are `*_env`-only and must be present in the container environment. The base reads environment variables from one Secret only, `gateway-upstream-credentials`, so put them in that Secret in step 5 or add a second `envFrom` entry in your own overlay.

   `budget.persist_path`, `prompt.persist_path`, `admin.persist_path` and `admin.backup_dir` are single-process file paths. The base mounts no writable volume and sets `readOnlyRootFilesystem: true`, so leave them unset unless you add a volume and drop to one replica.

3. **Re-pin the image.** `deploy/k8s/base/deployment.yaml` pins `ghcr.io/kelvran/gateway:v0.17.0@sha256:f649d74d13bb17ebc132af6c5a21be936528955ac7ee14152421f3267ff03b3a`. That digest is a single-platform (`linux/amd64`) manifest digest and is stale from `gateway/v0.18.0` on. Multi-platform images (`linux/amd64` and `linux/arm64` as one index) ship since gateway/v0.18.0; gateway/v0.17.0 is amd64-only. For gateway/v0.18.0 and any later tag pin the index digest:

   ```bash
   docker buildx imagetools inspect ghcr.io/kelvran/gateway:v<X.Y.Z>   # copy the top-level Digest: line
   # deployment.yaml: image: ghcr.io/kelvran/gateway:v<X.Y.Z>@sha256:<index-digest>
   ```

   To check the signature and attestations first:

   ```bash
   cosign verify ghcr.io/kelvran/gateway:v<X.Y.Z> \
     --certificate-identity-regexp '^https://github.com/kelvran/gateway/' \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com
   gh attestation verify oci://ghcr.io/kelvran/gateway:v<X.Y.Z> -R kelvran/gateway
   ```

   Tag forms and the full recipe (including `cosign verify-attestation --type cyclonedx|slsaprovenance1`) are in the [container image reference](../../reference/container-image.md) and [`RELEASE.md`](../../../RELEASE.md).

4. **Apply the base.**

   ```bash
   kubectl apply -k deploy/k8s/base/
   ```

   This creates the Namespace, the ServiceAccount, the Deployment, the Service, the NetworkPolicy, the PodDisruptionBudget, the generated ConfigMap and the empty placeholder Secret. Pods start, but every upstream call fails authentication until step 5.

5. **Create the real Secret under the same name.** Put one `KEY=value` per line in a `.env` file inside `deploy/k8s/base/` (gitignored; `.env.example` at the repository root lists the provider variable names). Add the admin and signing variables from step 2 if you set them.

   ```bash
   kubectl create secret generic gateway-upstream-credentials \
     --namespace kelvran --from-env-file=deploy/k8s/base/.env \
     --dry-run=client -o yaml | kubectl apply -f -
   ```

6. **Restart so the pods read the new values.** `envFrom` is read once at container start.

   ```bash
   kubectl rollout restart deployment/gateway -n kelvran
   ```

**Re-apply resets the Secret.** Every later `kubectl apply -k deploy/k8s/base/` applies `secret-placeholder.yaml` again and its `stringData: {}` wins, emptying the Secret you created in step 5. After any re-apply, repeat steps 5 and 6. Once you manage the Secret elsewhere (ESO, Vault, a script), remove `secret-placeholder.yaml` from the `resources:` list in `deploy/k8s/base/kustomization.yaml` instead.

## Variant: EKS with External Secrets Operator (`overlays/eks-irsa`)

The overlay includes `../../base`, adds a `SecretStore` and an `ExternalSecret` (both `apiVersion: external-secrets.io/v1`, both in namespace `kelvran`), and deletes the base's placeholder Secret with a `$patch: delete`. The `ExternalSecret` creates Secret `gateway-upstream-credentials` itself (`creationPolicy: Owner`, `refreshInterval: 1h`), so steps 5 and 6 above and the re-apply footgun do not apply. The `SecretStore` authenticates to AWS Secrets Manager through `auth.jwt.serviceAccountRef.name: kelvran-gateway` (IRSA), so no static AWS key appears anywhere.

1. **Provision the IAM role** with `terraform/irsa-trust/`. It creates one role whose trust policy allows `sts:AssumeRoleWithWebIdentity` from your cluster's OIDC provider when `sub` equals `system:serviceaccount:<namespace>:<service_account_name>` and `aud` equals `sts.amazonaws.com`, attaches a policy allowing only `secretsmanager:GetSecretValue` on the ARNs you pass, and outputs `role_arn`.

   ```bash
   cd terraform/irsa-trust
   terraform init
   terraform apply \
     -var account_id=123456789012 \
     -var oidc_provider_url=oidc.eks.us-east-1.amazonaws.com/id/EXAMPLED539D4633E53DE1B71EXAMPLE \
     -var 'secrets_manager_secret_arns=["arn:aws:secretsmanager:us-east-1:123456789012:secret:kelvran/gateway/openai-api-key-AbCdEf"]'
   terraform output role_arn
   ```

   `account_id` must be 12 digits. `oidc_provider_url` must not start with `https://` (strip the scheme from `aws eks describe-cluster`'s `oidc.issuer` value). `secrets_manager_secret_arns` must be non-empty. The defaults `namespace = "kelvran"` and `service_account_name = "kelvran-gateway"` match the base manifests; `aws_region` defaults to `us-east-1` and `name_prefix` to `kelvran-irsa`.

2. **Edit the placeholders.**
   - `deploy/k8s/base/serviceaccount.yaml`: replace `REPLACE_WITH_IRSA_ROLE_ARN` in `eks.amazonaws.com/role-arn` with the `role_arn` output. The `iam.amazonaws.com/role` annotation is ignored on EKS; remove it or leave it.
   - `deploy/k8s/overlays/eks-irsa/secretstore.yaml`: `region: us-east-1` must match the Terraform `aws_region`.
   - `deploy/k8s/overlays/eks-irsa/externalsecret.yaml`: the five `remoteRef.key` values (`kelvran/gateway/openai-api-key`, `kelvran/gateway/anthropic-api-key`, `kelvran/gateway/gemini-api-key`, `kelvran/gateway/aws-access-key-id`, `kelvran/gateway/aws-secret-access-key`) are placeholders. Replace them with your Secrets Manager names and keep only the entries your `config.yaml` uses. The `secretKey` names are `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`; add any admin or Redis variables from step 2 of the base procedure here too.

3. **Apply the overlay** (after steps 1 to 3 of the base procedure).

   ```bash
   kubectl apply -k deploy/k8s/overlays/eks-irsa/
   ```

A rotated value in Secrets Manager reaches the Kubernetes Secret within `refreshInterval`, but a running pod still holds the environment it started with. Restart the Deployment, or use the file-based variant below.

## Variant: credentials as mounted files

Each deployment accepts `api_key_file`, `access_key_id_file`, `secret_access_key_file` and `session_token_file` as alternatives to the `*_env` keys (example paths under `/var/run/secrets/kelvran/` are commented in `gateway/config.example.yaml`). When a `*_file` key is set, the gateway reads the file and re-reads it periodically, so a projected Secret volume rotation reaches a running pod without a restart. The base has no such volume: add a Secret volume and a `volumeMounts` entry in your own overlay (a Secret volume is its own mount that the kubelet keeps updated; `readOnlyRootFilesystem: true` applies only to the container's root filesystem and does not block it). Admin tokens, `redis_password_env` and `signing_secret_env` have no file variant. See [Rotate credentials](../rotate-credentials.md).

## Variant: self-managed Kubernetes with kiam or kube2iam

Apply `deploy/k8s/base/` alone and bring your own Secret (steps 5 and 6). Set `iam.amazonaws.com/role` on the ServiceAccount to the role your kiam/kube2iam installation allows; `terraform/irsa-trust/` does not provision that role, because its trust principal is the OIDC provider, not an instance profile. `eks.amazonaws.com/role-arn` is ignored on such clusters.

## Verify it worked

```bash
kubectl -n kelvran rollout status deployment/gateway
# deployment "gateway" successfully rolled out

kubectl -n kelvran get secret gateway-upstream-credentials \
  -o go-template='{{range $k, $v := .data}}{{$k}}{{"\n"}}{{end}}'
# prints the key names you supplied (for example OPENAI_API_KEY); no output means the Secret is still the empty placeholder

kubectl -n kelvran port-forward svc/gateway 8080:8080 &
curl -s http://127.0.0.1:8080/healthz
# {"status":"ok"}

curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/readyz
# 200 when every configured model has at least one healthy deployment; 503 otherwise. Without a
# `health_probe:` section in config.yaml nothing is ever probed and this is 200 even with an empty
# or wrong Secret, so finish with a real request:

curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer <the raw secret of a virtual key in your config.yaml>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"<a model from your config.yaml>","messages":[{"role":"user","content":"ping"}],"max_tokens":5}'
# a completion object means the credential in the Secret reached the pod; a 502 whose body has
# "code":"upstream_error" and "upstream provider returned status 401" (or 403) means it did not
# (repeat steps 5 and 6)
```

To see which build each replica runs, read the `build_info` log record (the image has no shell, so `kubectl exec` is not an option). `build_info` and `-version` first shipped in gateway/v0.18.0; a gateway/v0.17.0 pod logs no such record.

```bash
kubectl -n kelvran logs deploy/gateway | grep build_info
# ... "msg":"build_info","version":"<X.Y.Z>","commit":"<sha>","date":"<date>","go_version":"go<N>","platform":"linux/amd64" ...
```

## Probes and external monitors

Keep both kubelet probes on `/healthz`. It checks only local process state, so a provider or Redis outage never restarts or de-registers every replica at once. `/readyz` reports per-model health and returns `503` when any configured model has no healthy deployment; point an external monitor at it, but never make it the pod `readinessProbe`, because one model losing all of its deployments would then empty the Service endpoints for every other model. In gateway/v0.17.0 and earlier, `/readyz` reports an embedding deployment unhealthy when `health_probe` is on; the kind-aware probe fix ships in gateway/v0.18.0. Details are in [`docs/operations/DEPLOY.md`](../../operations/DEPLOY.md).

CI runs `kubeconform` over six of the seven raw files in `deploy/k8s/base/` (every file except `networkpolicy.yaml`) plus the two `overlays/eks-irsa/` files, and Checkov over the `deploy/k8s/base/` directory, never over a rendered `kustomize build`, so edits to `config.yaml` are never checked by CI. Validate your config before you apply. Using the image you pinned in step 3 (arguments replace the image's `CMD`, so pass `-config` again):

```bash
docker run --rm -v "$PWD/deploy/k8s/base/config.yaml:/config.yaml:ro" \
  ghcr.io/kelvran/gateway:v<X.Y.Z> -config /config.yaml -validate
# prints "config is valid" and exits 0; otherwise prints "config error: <first error>" to stderr and exits 1
```

From a source build: `cd gateway && go run ./cmd/gateway -config ../deploy/k8s/base/config.yaml -validate`.

## Not available today

- **No Helm chart.** Kustomize is the recorded choice for Kelvran's own manifests; see "Why plain Kustomize, not Helm" in [`deploy/k8s/README.md`](../../../deploy/k8s/README.md). A chart published as OCI is a plan item awaiting an owner decision.
- **No Redis manifest.** No StatefulSet, PVC or Deployment for Redis exists under `deploy/k8s/`; the multi-replica Redis is yours to provide.
- **No PVC or StatefulSet** for a bbolt `persist_path`. Add one yourself and drop to `replicas: 1` with `strategy: { type: Recreate }`.
- **No Ingress, Gateway API route or TLS termination.** The Service is `ClusterIP` only.
- **No HorizontalPodAutoscaler.**
- **No evals Deployment.** Only the gateway has manifests.
- **Only one overlay** (`eks-irsa`). There is no overlay for kiam/kube2iam or for other secret stores.
- **The overlay does not install External Secrets Operator.** Its CRDs must exist before you apply.
- **Not applied to a production or EKS cluster.** `deploy/k8s/base/` was applied end to end to a local kind cluster on 2026-09-21; `overlays/eks-irsa/` has not been applied to any cluster.

## Related

- [`deploy/k8s/README.md`](../../../deploy/k8s/README.md): the in-tree operator notes these steps follow.
- [Configuration reference](../../reference/config.md) for every key named above.
- [Container image reference](../../reference/container-image.md) and [`RELEASE.md`](../../../RELEASE.md) for tags, digests and verification.
- [`docs/operations/DEPLOY.md`](../../operations/DEPLOY.md) for probes, Redis targets and rollout order.
- [`SECURITY.md`](../../../SECURITY.md) and the [security model](../../explanation/security-model.md) for why the admin API stays off the Service.
- [Upgrade](../upgrade.md), [Backup and restore](../backup-and-restore.md), [Troubleshooting](../troubleshooting.md).
- Other targets: [Docker Compose](docker-compose.md), [ECS Fargate](ecs-fargate.md), [systemd package](systemd-package.md).
