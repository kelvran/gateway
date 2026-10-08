# Deploy the gateway on ECS Fargate

This page registers the Kelvran gateway as an ECS Fargate task definition with native Secrets Manager injection, using the two Terraform modules in the repository, and spells out what you must still build yourself. It is for operators who run AWS without Kubernetes and already manage ECS clusters, VPCs and load balancers with their own tooling.

Use this when you want Fargate to inject upstream provider credentials from Secrets Manager into the gateway container, and you are prepared to supply the cluster, service, networking, load balancer and config-file delivery yourself.

## What the repository ships for this target

Two root modules, applied in order:

| Module | Creates | Does not create |
|---|---|---|
| `terraform/ecs-task-role/` | A task execution role (`<name_prefix>-execution-role`) with `AmazonECSTaskExecutionRolePolicy` plus a `secretsmanager:GetSecretValue` policy scoped to the ARNs you pass, and a task role (`<name_prefix>-task-role`) that gets a `bedrock:InvokeModel`/`bedrock:InvokeModelWithResponseStream` policy only when `bedrock_model_arns` is non-empty. Both trust `ecs-tasks.amazonaws.com`. Default `name_prefix` is `kelvran-ecs`. | Anything else |
| `deploy/ecs/` | One `aws_cloudwatch_log_group` (default name `/ecs/kelvran-gateway`, 30-day retention) and one `aws_ecs_task_definition` (family `kelvran-gateway`, `FARGATE`, `awsvpc`, task cpu `512`, task memory `1024`, one `gateway` container on port 8080 with a 512 MiB hard memory limit, `awslogs` logging with stream prefix `gateway`). | ECS cluster, ECS service, VPC/subnets/security groups, load balancer, target group, health check, config-file delivery, autoscaling |

Both modules require Terraform `>= 1.5` and `hashicorp/aws ~> 5.0` (`deploy/ecs/versions.tf`, `terraform/ecs-task-role/versions.tf`). Each module's `.terraform.lock.hcl` is committed and pins `hashicorp/aws` `5.100.0`. State is local by default: neither module has a `backend` block. `terraform/README.md` describes the two remote-state options (`terraform/backend-bootstrap/` for S3 and DynamoDB, or Terraform Cloud); neither is wired in.

The repository records no apply of either module against a live ECS cluster. CI (`.github/workflows/iac-scan.yml`) runs `terraform fmt -check -recursive`, `terraform init -backend=false -input=false`, `terraform validate` and a structural `terraform plan` with fake static credentials for both modules. There is no apply job.

## Prerequisites

- AWS credentials with permission to create IAM roles and policies, CloudWatch Logs groups and ECS task definitions.
- Terraform `>= 1.5`.
- One Secrets Manager secret per upstream credential that your `config.yaml` names by environment variable: `api_key_env` for `openai`, `anthropic`, `gemini` and `openaicompat` deployments (every provider except `bedrock`); `access_key_id_env`, `secret_access_key_env` and the optional `session_token_env` for `bedrock` deployments. Creating the secrets is outside both modules; see [Provider credentials](../provider-credentials.md). Store each credential as the secret's plaintext string, not as a key/value (JSON) secret: Fargate injects the full secret contents and the gateway uses the variable verbatim. If a secret must stay JSON, append `:<json-key>::` to its ARN in the `secrets` map in step 2; the ARN you list in step 1 stays the bare secret ARN.
- A finished `config.yaml`. The container starts with `-config /config.yaml` and nothing in the image provides that file. See the [configuration reference](../../reference/config.md) and [gateway/config.example.yaml](../../../gateway/config.example.yaml). The file holds environment-variable names and SHA-256 virtual-key hashes, never secret values.
- An image reference pinned by digest. `var.image` defaults to `ghcr.io/kelvran/gateway:latest`, a moving tag that the variable's own description tells you to replace. The [container image reference](../../reference/container-image.md) explains how to read the index digest for a release tag.

## Steps

### 1. Create the IAM roles

```sh
cd terraform/ecs-task-role
terraform init
terraform apply \
  -var='secrets_manager_secret_arns=["arn:aws:secretsmanager:us-east-1:123456789012:secret:kelvran/gateway/openai-api-key-AbCdEf"]'
terraform output execution_role_arn
terraform output task_role_arn
```

`secrets_manager_secret_arns` must be non-empty; the module's validation rejects an empty list. List every secret ARN the task definition will reference in step 2, because the execution role can read only the ARNs listed here. The role grants `secretsmanager:GetSecretValue` only, so each secret must use the default `aws/secretsmanager` KMS key; a secret encrypted with a customer-managed key also needs `kms:Decrypt` on that key, which this module does not add.

### 2. Register the task definition

```sh
cd deploy/ecs
terraform init
terraform apply \
  -var='execution_role_arn=<execution_role_arn from step 1>' \
  -var='task_role_arn=<task_role_arn from step 1>' \
  -var='image=ghcr.io/kelvran/gateway:v<X.Y.Z>@sha256:<index-digest>' \
  -var='secrets={"OPENAI_API_KEY"="arn:aws:secretsmanager:us-east-1:123456789012:secret:kelvran/gateway/openai-api-key-AbCdEf"}'
terraform output task_definition_arn
```

`secrets` is a map of container environment-variable name to secret ARN. Each key must equal the `api_key_env` (or `access_key_id_env`, `secret_access_key_env`, `session_token_env`) value in `config.yaml`; the gateway resolves those names from the environment at startup (`gateway/cmd/gateway/main.go`). Fargate resolves each ARN through the execution role and injects the value as that variable before the container starts. One map entry per credential.

Because the values are resolved at task launch and read by the gateway at startup, a rotated secret reaches the gateway only when a new task starts. For a service, `aws ecs update-service --cluster <cluster> --service <service> --force-new-deployment` is enough; no new task-definition revision is needed. See [Rotate credentials](../rotate-credentials.md).

Optional overrides, shown with their defaults:

```sh
-var=aws_region=us-east-1 -var=family=kelvran-gateway \
-var=cpu=512 -var=memory=1024 -var=container_memory=512 \
-var=container_port=8080 \
-var=log_group_name=/ecs/kelvran-gateway -var=log_retention_days=30 \
-var='tags={}'
```

`container_memory` is the cgroup limit the gateway's `automemlimit` integration reads at startup to set `GOMEMLIMIT` (at a 0.9 ratio). The task-level `memory` must also cover Fargate's own platform overhead, which is why the two defaults differ (512 and 1024).

### 3. Deliver config.yaml to the container

Neither module does this. The repository names two options in `deploy/ecs/variables.tf` and `deploy/ecs/README.md` and has not chosen between them. Pick one:

- **Derived image.** Build an image from the pinned gateway image that adds your file at `/config.yaml`, push it to your own registry, and pass that reference as `-var=image=...` in step 2.

  ```dockerfile
  FROM ghcr.io/kelvran/gateway:v<X.Y.Z>@sha256:<index-digest>
  COPY config.yaml /config.yaml
  ```

  The gateway image is `FROM scratch`; the derived layer adds only your one file. Since `config.yaml` carries environment-variable names and key hashes rather than secret values, this does not bake a secret into an image.

- **EFS volume.** Add a `volume` block with `efs_volume_configuration` to the task definition and a `mountPoints` entry whose `containerPath` is a directory, for example `/config`; EFS mounts a directory, never a single file. Because the image's command is `-config /config.yaml` (`gateway/Dockerfile`), also set `command = ["-config", "/config/config.yaml"]` in the container definition. `deploy/ecs/main.tf` declares no volumes, mount points or `command` override and exposes no variable for them, so all three edits go in your own copy of the module.

### 4. Run it

Everything below is yours to provision; `deploy/ecs/` has no opinion about it:

- An ECS cluster and an `aws_ecs_service` whose `task_definition` is the `task_definition_arn` output (the ARN includes the revision). `deploy/ecs/README.md` also names a one-off `aws ecs run-task` as a way to launch the task definition without a service.
- Subnets and security groups. `awsvpc` network mode requires real IDs at service-creation time.
- A load balancer and target group. Use target type `ip`, container port `8080` and a health check on `GET /healthz`. The gateway answers HTTP 200 with body `{"status":"ok"}`, with no authentication and independent of upstream reachability. Do not expect a container-level `healthCheck`: ECS runs that command inside the container, the image has no shell, and `deploy/ecs/main.tf` deliberately sets none.
- Autoscaling, if you want it.

## Variants

### Bedrock deployments

Passing `bedrock_model_arns` in step 1 attaches an invoke policy to the task role:

```sh
-var='bedrock_model_arns=["arn:aws:bedrock:us-east-1::foundation-model/<model-id>"]'
```

The gateway as shipped never exercises that policy. Today the gateway signs Bedrock requests only with the static credential pair named in `config.yaml` (`access_key_id_env`/`secret_access_key_env` plus the optional `session_token_env`, or their `*_file` counterparts) (`gateway/internal/gateway/dataplane/dataplane.go`). It never reads the task role's own credentials. You can omit the variable today; inject the static pair through the `secrets` map in step 2 like any other credential, with the same ARNs listed in step 1.

### Graviton (ARM64)

`deploy/ecs/main.tf` sets no `runtime_platform`, so the task definition is X86_64/LINUX. A multi-platform image index (`linux/amd64` and `linux/arm64`) is on main since 2026-10-08, not in gateway/v0.17.0. To run on Graviton you add a `runtime_platform` block with `operating_system_family = "LINUX"` and `cpu_architecture = "ARM64"` yourself and pin an index digest from an image built on or after that date.

### Admin API

When `admin.token_env` is set, the admin server binds `127.0.0.1:8081` unless `admin.listen_addr` says otherwise (`gateway/cmd/gateway/main.go`), so the admin API is unreachable from outside the task as long as you leave `admin.listen_addr` unset. The single port mapping is not what protects it: in `awsvpc` mode any port a container binds on a non-loopback address is reachable at the task ENI, subject only to your security group. If you enable the admin API, the variables named by `admin.token_env` and any `viewer_token_env`, `cost_viewer_token_env` or `operator_token_env` must also arrive through the `secrets` map in step 2 with their ARNs listed in step 1; the task definition passes no plain `environment` variables, and the gateway refuses to start when a configured `*_token_env` resolves empty. The same applies to every other `*_env` your config names (any `redis_password_env`, `config_propagation.signing_secret_env`, guardrail AWS keys). See [Admin API and RBAC](../admin-api-rbac.md) and the [admin API reference](../../reference/admin-api.md).

## Verify it worked

After step 2, from `deploy/ecs/`:

```sh
terraform output task_definition_arn
terraform output log_group_name
```

The first prints the registered task definition's ARN, which names the family (`kelvran-gateway` by default) and ends with the revision number. The second prints `/ecs/kelvran-gateway` unless you overrode it.

After step 4, once a task is running behind your load balancer:

```sh
curl -si http://<your-alb-dns-name>/healthz
```

returns an HTTP 200 status and the body `{"status":"ok"}`. `curl -si http://<your-alb-dns-name>/readyz` returns HTTP 200 with `"ready":true` in the body only when every canonical model in `config.yaml` has at least one healthy deployment. If any model has none, it returns HTTP 503 with `"ready":false`; the `"models"` map in the body names which model is down. Container output appears in the CloudWatch Logs group `/ecs/kelvran-gateway` under streams prefixed `gateway`.

## Not available today

- An ECS cluster or `aws_ecs_service`: bring your own.
- VPC, subnets, security groups: bring your own.
- A load balancer, target group or the `/healthz` health check as a resource. The health check is a recommendation in `deploy/ecs/main.tf`'s comments, not something the module provisions.
- Config-file delivery (EFS volume or derived image): named, not implemented.
- `runtime_platform` for ARM64/Graviton.
- A container-level `healthCheck`: not possible on a `FROM scratch` image.
- Autoscaling resources.
- SSM Parameter Store as a `valueFrom` source. The execution role grants only `secretsmanager:GetSecretValue`, so these roles do not authorize an SSM parameter reference, even though `deploy/ecs/README.md` mentions SSM.
- `kms:Decrypt` for secrets encrypted with a customer-managed KMS key.
- Plain environment variables: the container definition has no `environment` block.
- Remote Terraform state wiring.
- Checkov scanning of `deploy/ecs/` (CI scans `terraform/` only).
- Publication as a Terraform Registry module.
- A gated `terraform apply` in CI.
- Task-definition hardening beyond the image's own `USER 65532:65532`: the task definition sets no `user`, `readonlyRootFilesystem` or `linuxParameters`.

## Related

- [deploy/ecs/README.md](../../../deploy/ecs/README.md): the module's own scope statement and apply order.
- [Deployment guide](../../operations/DEPLOY.md): the ECS/Fargate section alongside Docker Compose and Kubernetes.
- [deploy/k8s/README.md](../../../deploy/k8s/README.md): why ECS uses neither IRSA nor the kiam annotation.
- [Kubernetes (Kustomize)](./kubernetes-kustomize.md), [Docker Compose](./docker-compose.md), [systemd package](./systemd-package.md): the other deployment paths.
- [Container image reference](../../reference/container-image.md): tags, digests, platforms and image contents.
- [Metrics and logs](../../reference/metrics-and-logs.md) and [Telemetry](../../operations/TELEMETRY.md): what the gateway emits once it runs.
- [Security model](../../explanation/security-model.md), [SECURITY.md](../../../SECURITY.md), [THREAT_MODEL.md](../../../THREAT_MODEL.md).
- [Troubleshooting](../troubleshooting.md).
