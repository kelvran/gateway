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
  }
}

resource "aws_iam_user" "shipper" {
  name = var.shipper_user_name
  tags = var.tags
}

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
