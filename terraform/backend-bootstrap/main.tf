provider "aws" {
  region = var.aws_region
}

# Versioned + SSE-encrypted, same posture as vector-s3-shipper's own
# gatewayevents bucket -- Terraform state can contain sensitive values
# (resource IDs, occasionally plaintext secrets a provider echoes back),
# so this bucket gets the same encryption-at-rest/no-public-access
# treatment as any other Kelvran-managed bucket, not a lighter one just
# because its own contents are "just state."
resource "aws_s3_bucket" "state" {
  bucket = var.bucket_name
  tags   = var.tags
}

resource "aws_s3_bucket_versioning" "state" {
  bucket = aws_s3_bucket.state.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "state" {
  bucket = aws_s3_bucket.state.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "state" {
  bucket = aws_s3_bucket.state.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# A DynamoDB lock table, not the S3-native `use_lockfile` locking
# Terraform 1.10+ added, on purpose: `required_version = ">= 1.5"` across
# this whole tree (versions.tf, every module) predates that feature, and
# a DynamoDB table works with every Terraform version already in use
# here -- switching the CONSUMING modules' own backend blocks to
# `use_lockfile = true` instead of this table is a real, disclosed,
# available future simplification once the minimum version is raised,
# not something this module needs to anticipate today.
resource "aws_dynamodb_table" "locks" {
  name         = var.dynamodb_table_name
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "LockID"

  attribute {
    name = "LockID"
    type = "S"
  }

  tags = var.tags
}
