# One default self-hosted remote-state path for this Terraform tree's
# own state: an S3 bucket (versioned, encrypted) + a DynamoDB lock table.
# This module is the classic "what provisions the state bucket" chicken-
# and-egg exception — it is applied ONCE, BY HAND, with LOCAL state, per
# this module's own README. Never referenced from any other module's
# `backend "s3" {}` block until an operator has actually run that one
# apply; see terraform/README.md for the documented Terraform Cloud/HCP
# alternative to this module entirely.

variable "bucket_name" {
  description = "Name of the S3 bucket that will hold every OTHER module's Terraform state under this tree — must be globally unique, per S3's own naming rules. No safe generated default, for the same reason vector-s3-shipper's own bucket_name has none: a random suffix would fight this value being hardcoded into every consuming module's backend block."
  type        = string
}

variable "aws_region" {
  description = "AWS Region the state bucket and lock table are created in."
  type        = string
  default     = "us-east-1"
}

variable "dynamodb_table_name" {
  description = "Name of the DynamoDB table used for Terraform state locking (S3 backend's `use_lockfile` native locking, added in Terraform 1.10, is deliberately NOT relied on here instead — see main.tf's own comment on why a table is still provisioned)."
  type        = string
  default     = "kelvran-terraform-locks"
}

variable "tags" {
  description = "Common tags applied to every resource this module creates."
  type        = map(string)
  default     = {}
}
