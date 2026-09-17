# IAM role with an OIDC-federated trust policy for IRSA (IAM Roles for
# Service Accounts), per docs/upgrade-research/terraform-iac-deployment-
# automation-2026-09-15.md's Finding 6. Attaches to an EXISTING EKS
# cluster's OIDC provider -- this module never creates a cluster, only
# the IAM side of the trust relationship External Secrets Operator's
# "zero-configuration" AWS auth method (that same Finding) needs.

variable "account_id" {
  description = "AWS account ID this role is created in -- required for the OIDC provider ARN and the trust-policy Principal, both of which must name a real account."
  type        = string

  validation {
    condition     = can(regex("^[0-9]{12}$", var.account_id))
    error_message = "account_id must be a 12-digit AWS account ID."
  }
}

variable "oidc_provider_url" {
  description = "The EXISTING EKS cluster's own OIDC issuer URL, without the leading \"https://\" (e.g. \"oidc.eks.us-east-1.amazonaws.com/id/EXAMPLED539D4633E53DE1B71EXAMPLE\") -- find it via `aws eks describe-cluster --name <cluster> --query \"cluster.identity.oidc.issuer\"`. This module attaches to that provider; it does not create the cluster or register the provider itself (that's a one-time `aws eks` / `aws iam create-open-id-connect-provider` step, or the `enable_irsa` flag on the terraform-aws-modules/eks module, if the cluster itself is Terraform-managed elsewhere)."
  type        = string
}

variable "aws_region" {
  description = "Region the IAM role is created in and the Secrets Manager/Parameter Store paths below live in."
  type        = string
  default     = "us-east-1"
}

variable "namespace" {
  description = "Kubernetes namespace the trusting ServiceAccount lives in -- matches deploy/k8s/base/serviceaccount.yaml's own namespace (default namespace unless overridden)."
  type        = string
  default     = "default"
}

variable "service_account_name" {
  description = "Name of the Kubernetes ServiceAccount allowed to assume this role -- matches deploy/k8s/base/serviceaccount.yaml's own metadata.name."
  type        = string
  default     = "kelvran-gateway"
}

variable "secrets_manager_secret_arns" {
  description = "Secrets Manager secret ARNs the role is scoped to read -- e.g. the ARNs backing gateway-upstream-credentials's real keys (OPENAI_API_KEY, ANTHROPIC_API_KEY, etc., per deploy/k8s/base/secret-placeholder.yaml). Required and must be non-empty -- a wildcard `secretsmanager:GetSecretValue` grant defeats the entire point of moving off a static long-lived key."
  type        = list(string)

  validation {
    condition     = length(var.secrets_manager_secret_arns) > 0
    error_message = "secrets_manager_secret_arns must be non-empty -- this role grants no other access, so an empty list would create a real but useless role."
  }
}

variable "name_prefix" {
  description = "Prefix for the IAM role/policy names this module creates."
  type        = string
  default     = "kelvran-irsa"
}

variable "tags" {
  description = "Common tags applied to every resource this module creates."
  type        = map(string)
  default     = {}
}
