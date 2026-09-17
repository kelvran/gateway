# IAM Access Analyzer, unused-access finding type -- per
# docs/upgrade-research/secrets-management-lifecycle-2026-09-15.md
# Finding 5: "Enable IAM Access Analyzer's unused-access finding type
# (low effort, native AWS console/API feature, catches a different
# problem than [the leaked-key] incident)". A genuinely different
# signal from "this credential leaked" -- it flags IAM users/roles with
# access keys or permissions that simply haven't been used recently,
# useful for catching FORGOTTEN credentials, not exposed ones.
#
# Confirmed against AWS's own current CreateAnalyzer API reference
# (docs.aws.amazon.com/access-analyzer/latest/APIReference/API_CreateAnalyzer.html),
# not assumed: "ACCOUNT_UNUSED_ACCESS" is a real, valid analyzer type,
# taking a configuration.unusedAccess.unusedAccessAge parameter (the
# same shape this module's aws_accessanalyzer_analyzer resource uses).

variable "aws_region" {
  description = "Region the analyzer is created in -- unused-access analysis is regional, so a multi-region account needs one of these per region it operates in."
  type        = string
  default     = "us-east-1"
}

variable "analyzer_name" {
  description = "Name of the analyzer."
  type        = string
  default     = "kelvran-unused-access-analyzer"
}

variable "unused_access_age" {
  description = "Days of inactivity before an IAM user/role's own unused access generates a finding (1-365, per AWS's own valid range). Defaults to 90 -- the same floor Finding 5's own research names for residual long-lived-key rotation, kept consistent rather than picking an unrelated number."
  type        = number
  default     = 90

  validation {
    condition     = var.unused_access_age >= 1 && var.unused_access_age <= 365
    error_message = "unused_access_age must be between 1 and 365, per AWS's own documented valid range."
  }
}

variable "tags" {
  description = "Common tags applied to the analyzer."
  type        = map(string)
  default     = {}
}
