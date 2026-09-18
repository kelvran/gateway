# provider block, per a live 2026-09-16 adversarial-audit finding: this
# module declared var.aws_region with a doc comment claiming it controls
# "the AWS Region the bucket is created in," but nothing anywhere
# actually referenced it -- the bucket's real region was silently
# whatever the ambient AWS_REGION/AWS_DEFAULT_REGION env var or
# ~/.aws/config happened to be, with no error if a caller overrode the
# variable expecting it to matter. This module is a standalone root
# module (per this Terraform tree's own "no remote-state coupling, no
# pre-existing VPC/cluster assumption" design), so a self-contained
# provider block wiring the variable in for real is the correct fix, not
# just documentation.
provider "aws" {
  region = var.aws_region
}

resource "aws_s3_bucket" "gatewayevents" {
  bucket = var.bucket_name
  tags   = var.tags
}

# Standard hygiene, not scope creep: this bucket has no legitimate reason
# for any object inside it to ever be publicly readable/writable --
# gatewayevents_v1 objects are metadata-only (no prompt/completion
# content, per the RFC's own Context section) but are still an internal
# operational data class, never meant for public exposure.
resource "aws_s3_bucket_public_access_block" "gatewayevents" {
  bucket = aws_s3_bucket.gatewayevents.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# Explicit, auditable-in-Terraform encryption-at-rest declaration, per a
# live 2026-09-16 adversarial-audit finding: AWS has applied a mandatory
# SSE-S3 baseline to every new bucket by default since January 2023, so
# this bucket was never actually storing plaintext -- but nothing in this
# module declared or enforced that standard itself, leaving it entirely
# to an implicit account-level default with no Terraform-managed
# guarantee. SSE-S3 (AES256), not SSE-KMS with a customer-managed key --
# gatewayevents_v1 objects are metadata-only (no prompt/completion
# content) with no documented compliance driver for a CMK's extra
# operational cost (key rotation, IAM key-policy grants for both this
# shipper and evals' own ingest tooling), matching this same file's own
# KISS/YAGNI reasoning for the lifecycle rule below.
resource "aws_s3_bucket_server_side_encryption_configuration" "gatewayevents" {
  bucket = aws_s3_bucket.gatewayevents.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# Per the same audit finding: this bucket had no versioning, MFA-delete,
# or Object Lock anywhere. The shipper's own IAM policy below is
# genuinely write-only (no s3:DeleteObject grant), but nothing protected
# against a PutObject retry silently overwriting a prior object under the
# same key, or deletion by any other, broader-permissioned principal.
resource "aws_s3_bucket_versioning" "gatewayevents" {
  bucket = aws_s3_bucket.gatewayevents.id

  versioning_configuration {
    status = "Enabled"
  }
}

# One S3 Lifecycle rule, one prefix, expire-only -- per
# docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md §4's own
# explicit "kept to one rule, not a multi-tier lifecycle" KISS/YAGNI
# reasoning (no Glacier/Infrequent-Access transition).
resource "aws_s3_bucket_lifecycle_configuration" "gatewayevents" {
  bucket = aws_s3_bucket.gatewayevents.id

  rule {
    id     = "expire-gatewayevents-v1"
    status = "Enabled"

    filter {
      prefix = var.key_prefix
    }

    expiration {
      days = var.retention_days
    }

    # Closes a real Checkov finding (CKV_AWS_300) -- Vector's own
    # aws_s3 sink only ever calls a single PutObject per object (per
    # this module's own shipper-policy comment above), so this
    # shouldn't fire in normal operation, but a real network partition
    # mid-upload could otherwise leave an orphaned, indefinitely-billed
    # incomplete multipart upload with nothing in this module to ever
    # clean it up. 7 days matches AWS's own documented example for this
    # exact lifecycle action.
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}

resource "aws_iam_user" "shipper" {
  name = var.shipper_user_name
  tags = var.tags
}

# Deliberately NOT built, per a 2026-09-18 Checkov triage -- disclosed
# accepted risk, matching this module's own established SSE-S3-not-KMS
# precedent below, not an oversight:
# - CKV_AWS_145/CKV_AWS_144/CKV2_AWS_62/CKV_AWS_18: same reasoning as
#   terraform/backend-bootstrap's own identical disclosure (KMS/cross-
#   region-replication/event-notifications/access-logging).
# - CKV_AWS_40 ("IAM policies attached only to groups or roles") and
#   CKV_AWS_273 ("access controlled through SSO, not IAM users"): both
#   flag a real, structural constraint of the tool this module exists
#   to support, not a fixable oversight -- Vector's own aws_s3 sink
#   authenticates via a static access-key-ID/secret pair resolved from
#   its container's environment (see this file's own comment below),
#   which fundamentally requires an IAM USER, not a role or SSO
#   identity (roles/SSO issue temporary, assumed credentials Vector has
#   no built-in mechanism to assume). This is the exact same "eliminate
#   the long-lived key" gap
#   docs/upgrade-research/secrets-management-lifecycle-2026-09-15.md
#   Finding 1 named for gateway's own Bedrock credential, applied to a
#   DIFFERENT credential here -- closing it for Vector specifically
#   would need real research into whether Vector supports IRSA/task-
#   role-style credential assumption, which this pass didn't do; named
#   as real, disclosed future work, not silently treated as solved.

# Write-only, scoped to key_prefix's own object path -- deliberately NOT
# the bucket ARN itself (no s3:ListBucket, no s3:GetObject, no
# s3:DeleteObject anywhere in this policy): Vector's aws_s3 sink only
# ever calls PutObject, per its own documented behavior, and a shipper
# that can list or read back what it already wrote has no real need this
# feature was built to serve -- narrower than "read/write to the bucket"
# on purpose.
data "aws_iam_policy_document" "shipper_write_only" {
  statement {
    sid       = "GatewayEventsWriteOnly"
    effect    = "Allow"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.gatewayevents.arn}/${var.key_prefix}*"]
  }
}

resource "aws_iam_user_policy" "shipper_write_only" {
  name   = "${var.shipper_user_name}-write-only"
  user   = aws_iam_user.shipper.name
  policy = data.aws_iam_policy_document.shipper_write_only.json
}

# Vector's own default AWS credential-provider chain resolves an access
# key + secret from the container's environment (env vars first, per
# Vector's own "AWS authentication" docs -- see
# vector-gatewayevents-s3-compose.yaml's own sink comment, "No auth:
# block"), so a real access key is what the accompanying Compose
# deployment's .env.vector actually needs -- not just an IAM user with no
# usable credential.
resource "aws_iam_access_key" "shipper" {
  user = aws_iam_user.shipper.name
}
