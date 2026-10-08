# ECS Fargate deployment for `gateway`

Added as part of the backlog Phase 7 infra round, per
`docs/upgrade-research/terraform-iac-deployment-automation-2026-09-15.md`'s
Finding 1/7. The ECS-native alternative to `../k8s/` for operators who'd
rather not run a Kubernetes cluster at all — Fargate injects Secrets
Manager/SSM Parameter Store values into containers natively via the task
definition's own `secrets` block, no External Secrets Operator or IRSA
equivalent needed for this target.

**Corrected 2026-10-08**: "Secrets Manager/SSM Parameter Store" above
describes Fargate in general, not this repo's roles. The execution role
`../../terraform/ecs-task-role/` provisions grants only
`secretsmanager:GetSecretValue` (its `execution_secrets` policy, scoped to
`var.secrets_manager_secret_arns`) plus the managed
`AmazonECSTaskExecutionRolePolicy` (ECR pull + CloudWatch Logs); it has no
`ssm:GetParameters` grant, so an SSM parameter ARN in this module's
`secrets` map registers fine but the task fails at launch. Read the
sentence above as "Secrets Manager values" for this directory as shipped —
`variables.tf` already documents `secrets` as Secrets Manager ARNs.

## What this module provisions, and what it deliberately does not

**Provisions:** one `aws_ecs_task_definition` (`main.tf`) plus its
CloudWatch Logs group. That's it.

**Does NOT provision** (bring your own, same "never applied against a live
cluster" disclosure `../k8s/README.md` already uses for its own gaps):

- An ECS **cluster** (`aws_ecs_cluster`) or **service**
  (`aws_ecs_service`) — this module only registers the task definition;
  running it requires a cluster + service (or a one-off `aws ecs run-task`)
  you provision separately.
- **Networking** (VPC, subnets, security groups) — `awsvpc` network mode
  requires real subnet/security-group IDs at service-creation time, which
  this module has no opinion about.
- A **load balancer** / health check from outside the task — see
  `main.tf`'s own comment on why no container-level `healthCheck` is set
  (the image is `FROM scratch`, no shell to exec a command in) and why an
  ALB target-group health check against `/healthz` is the correct
  ECS-native equivalent instead.
- **Config-file delivery** — Fargate has no ConfigMap equivalent;
  `variables.tf`'s own header comment names the two real options (bake
  `config.yaml` into a custom image layer, or an EFS volume mount) without
  picking one, since neither has a demonstrated need yet.

## Usage

```sh
# 1. Apply ../../terraform/ecs-task-role/ first, to get real
#    execution_role_arn/task_role_arn values.
cd terraform/ecs-task-role
terraform apply -var='secrets_manager_secret_arns=["arn:aws:secretsmanager:...:OPENAI_API_KEY-abc123"]'

# 2. Apply this module, wiring in those two ARNs plus the same secret
#    ARNs mapped to the env var names config.yaml's api_key_env values
#    actually reference.
cd ../../deploy/ecs
terraform apply \
  -var='execution_role_arn=<from step 1>' \
  -var='task_role_arn=<from step 1>' \
  -var='secrets={"OPENAI_API_KEY"="arn:aws:secretsmanager:...:OPENAI_API_KEY-abc123"}'
```

See `../../docs/operations/DEPLOY.md` for this alongside the Docker
Compose and Kubernetes deployment paths.
