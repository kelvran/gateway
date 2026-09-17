# ECS Fargate task-definition template -- the ECS-native equivalent of
# deploy/k8s/base/deployment.yaml, mirroring its own securityContext/
# resource-sizing/probe disclosures where an ECS equivalent exists.
#
# Disclosed, deliberate scope limit: this module wires SECRETS only
# (Fargate's own native mechanism, via the `secrets` block below) --
# unlike deploy/k8s/base's ConfigMap-mounted config.yaml, this template
# does NOT solve config-FILE delivery (Fargate has no ConfigMap
# equivalent; the real options are baking config.yaml into a custom
# image layer, or an EFS volume mount, neither of which this repo has a
# demonstrated need for yet). Pick one yourself before using this in
# production -- named here, not silently glossed over.

variable "family" {
  description = "Task definition family name."
  type        = string
  default     = "kelvran-gateway"
}

variable "image" {
  description = "Container image, including tag/digest -- pin to a real digest before production use, matching deploy/k8s/base/deployment.yaml's own \"pin to a real digest/tag\" comment."
  type        = string
  default     = "ghcr.io/kelvran/gateway:latest"
}

variable "aws_region" {
  description = "Region the task definition and its CloudWatch Logs group are created in."
  type        = string
  default     = "us-east-1"
}

variable "cpu" {
  description = "Task-level vCPU units (Fargate CPU/memory combinations are constrained -- see AWS's own Fargate task size table). 512 = 0.5 vCPU, matching deploy/k8s/base/deployment.yaml's own \"500m\" CPU request as the closest ECS-native equivalent."
  type        = string
  default     = "512"
}

variable "memory" {
  description = "Task-level memory (MiB) -- 1024 here (not deployment.yaml's own 512Mi limit) because Fargate's task-level memory must also cover the platform's own overhead on top of the container's own GOMEMLIMIT-governed usage; the container-level limit below stays at deployment.yaml's own 512."
  type        = string
  default     = "1024"
}

variable "container_memory" {
  description = "Container-level hard memory limit (MiB) -- matches deploy/k8s/base/deployment.yaml's own resources.limits.memory (512Mi), the same value cmd/gateway/main.go's automemlimit integration reads via the cgroup limit Fargate enforces for the container."
  type        = number
  default     = 512
}

variable "container_port" {
  description = "Port the gateway listens on -- matches deploy/k8s/base/service.yaml's own containerPort."
  type        = number
  default     = 8080
}

variable "execution_role_arn" {
  description = "ARN of the task execution role -- from terraform/ecs-task-role/'s own execution_role_arn output. Required: no safe default, since a task definition with no execution role can't pull the image or resolve `secrets` at all."
  type        = string
}

variable "task_role_arn" {
  description = "ARN of the task role -- from terraform/ecs-task-role/'s own task_role_arn output."
  type        = string
}

variable "secrets" {
  description = "Map of container environment-variable name to Secrets Manager secret ARN -- e.g. {\"OPENAI_API_KEY\" = \"arn:aws:secretsmanager:...\"}, one entry per api_key_env value config.yaml's deployments actually reference. Fargate resolves each at launch via the execution role, injecting the secret's plaintext value as that environment variable -- the ECS-native equivalent of deploy/k8s/base/secret-placeholder.yaml's whole-Secret envFrom, done per-key instead, since ECS's own `secrets` block has no whole-secret-as-envFrom equivalent."
  type        = map(string)
  default     = {}
}

variable "log_group_name" {
  description = "CloudWatch Logs group name this task definition's container logs ship to -- created by this module."
  type        = string
  default     = "/ecs/kelvran-gateway"
}

variable "log_retention_days" {
  description = "CloudWatch Logs retention, in days."
  type        = number
  default     = 30
}

variable "tags" {
  description = "Common tags applied to every resource this module creates."
  type        = map(string)
  default     = {}
}
