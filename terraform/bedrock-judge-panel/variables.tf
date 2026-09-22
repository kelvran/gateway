# Scoped IAM role/policy for evals' judge panel, per
# evals/evals/judge/providers.py's own BEDROCK_SONNET_5_MODEL_ID/
# BEDROCK_HAIKU_4_5_MODEL_ID constants (Claude Sonnet 5 and Haiku 4.5,
# both invoked via their "global." cross-region inference profile, per
# that file's own doc comment on why: neither model supports on-demand
# invocation by bare model ID on this account, confirmed by a real live
# call, and "global." avoids needing to track whatever region AWS_REGION
# happens to be set to).

variable "account_id" {
  description = "AWS account ID the judge panel's Bedrock inference profiles are registered in. Required -- IAM ARNs for the inference-profile resource (unlike the foundation-model resource, which is never account-scoped) must name a real account, and there is no safe cross-account default."
  type        = string

  validation {
    condition     = can(regex("^[0-9]{12}$", var.account_id))
    error_message = "account_id must be a 12-digit AWS account ID."
  }
}

variable "aws_region" {
  description = "The judge panel's own source/requesting AWS Region -- i.e. the Region judge-panel calls actually originate from, matching config.yaml's real Bedrock deployments (region: \"us-east-1\") today. Global cross-Region inference then fans this request out to whichever supported Region Bedrock routes it to; this variable is about the CALLER's Region, never the destination."
  type        = string
  default     = "us-east-1"
}

variable "bedrock_model_names" {
  description = "Foundation-model names (the part of the inference-profile ID after the \"global.\" prefix) the judge panel is scoped to invoke -- see evals/evals/judge/providers.py's own BEDROCK_SONNET_5_MODEL_ID/BEDROCK_HAIKU_4_5_MODEL_ID constants for where these come from. Defaults to exactly those two, live-reverified against this account per that file's own doc comment -- add a model here only once evals' judge panel itself is actually configured to call it."
  type        = list(string)
  default = [
    "anthropic.claude-sonnet-5",
    "anthropic.claude-haiku-4-5-20251001-v1:0",
  ]
}

variable "create_role" {
  description = "Whether to create an assumable IAM role wrapping the policy below, in addition to the policy itself. false yields only the standalone aws_iam_policy (e.g. for a caller that wants to attach it to a role/user it manages elsewhere)."
  type        = bool
  default     = true
}

variable "trusted_principal_arns" {
  description = "Principal ARNs (an IAM role, an EC2/ECS/Lambda execution role, a CI runner's role, etc.) allowed to assume the judge-panel role. Required and must be non-empty when create_role is true -- an empty trust policy would create a real but unusable role, which is worse than failing plan outright."
  type        = list(string)
  default     = []

  validation {
    condition     = !var.create_role || length(var.trusted_principal_arns) > 0
    error_message = "trusted_principal_arns must be non-empty when create_role is true."
  }

  # A bare "*" or an account-root ARN in a condition-free Principal list
  # (this module's assume_role statement has no Condition block) grants
  # trust to literally any AWS principal, or to every principal in that
  # entire account, respectively -- a real, AWS-documented anti-pattern
  # for exactly this shape of trust policy, found by a fresh audit sweep.
  validation {
    condition = alltrue([
      for arn in var.trusted_principal_arns : arn != "*" && !can(regex("^arn:aws:iam::[0-9]{12}:root$", arn))
    ])
    error_message = "trusted_principal_arns must not contain \"*\" or an account-root ARN (arn:aws:iam::<account>:root) -- both trust every principal in scope, not just the intended one. Name the specific role/user ARN instead."
  }
}

variable "name_prefix" {
  description = "Prefix for the IAM policy/role names this module creates, so multiple instances (e.g. per environment) don't collide."
  type        = string
  default     = "kelvran-bedrock-judge-panel"
}

variable "tags" {
  description = "Common tags applied to every resource this module creates."
  type        = map(string)
  default     = {}
}
