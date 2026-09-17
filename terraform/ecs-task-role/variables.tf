# ECS Fargate's own two-role model (distinct from IRSA's single-role EKS
# model, per docs/upgrade-research/terraform-iac-deployment-automation-
# 2026-09-15.md's Finding 7 infra/workload split): a TASK EXECUTION role
# the ECS agent itself assumes to pull the image and resolve `secrets`
# block entries BEFORE the container starts, and a separate TASK role the
# gateway's own running process assumes for any AWS API calls it makes
# itself (Bedrock InvokeModel, if that deployment's provider is
# "bedrock"). Fargate injects Secrets Manager/SSM values natively via the
# execution role -- no External Secrets Operator equivalent needed for
# this target, unlike the EKS path in irsa-trust/.

variable "secrets_manager_secret_arns" {
  description = "Secrets Manager secret ARNs the TASK EXECUTION role is scoped to read and inject into the container at launch via the task definition's own `secrets` block (see ../../deploy/ecs/). Required and must be non-empty, same reasoning as irsa-trust's own equivalent variable."
  type        = list(string)

  validation {
    condition     = length(var.secrets_manager_secret_arns) > 0
    error_message = "secrets_manager_secret_arns must be non-empty -- the execution role grants no other access, so an empty list would create a real but useless role."
  }
}

variable "bedrock_model_arns" {
  description = "Bedrock foundation-model / inference-profile ARNs the TASK role (the running gateway process itself, not the ECS agent) is allowed to invoke -- only needed if this ECS deployment includes a \"bedrock\" provider deployment in config.yaml. Empty by default (no Bedrock deployment assumed) -- see terraform/bedrock-judge-panel/ for the equivalent evals-side pattern this mirrors."
  type        = list(string)
  default     = []
}

variable "name_prefix" {
  description = "Prefix for the IAM role/policy names this module creates."
  type        = string
  default     = "kelvran-ecs"
}

variable "tags" {
  description = "Common tags applied to every resource this module creates."
  type        = map(string)
  default     = {}
}
