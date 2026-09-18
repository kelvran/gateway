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

# Deliberately NOT built, per a 2026-09-18 Checkov triage -- disclosed
# accepted risk, matching this module's own established SSE-S3-not-KMS
# precedent above, not an oversight:
# - CKV_AWS_145 (KMS-encrypted by default): SSE-S3 is already real,
#   unconditional encryption at rest; a customer-managed KMS key adds
#   real operational cost (key policy grants, rotation) with no
#   documented compliance driver for Terraform STATE specifically.
# - CKV2_AWS_61 (lifecycle configuration): a Terraform state bucket
#   must NOT auto-expire its own objects -- unlike vector-s3-shipper's
#   event data, state is exactly the data you never want a lifecycle
#   rule silently deleting.
# - CKV_AWS_144 (cross-region replication): real, ongoing cost for a
#   single-operator state bucket with no named availability
#   requirement beyond what S3's own 11-nines durability already gives.
# - CKV2_AWS_62 (event notifications): no real downstream consumer
#   (SNS/SQS/Lambda) exists to notify -- wiring this with nothing on
#   the other end adds nothing.
# - CKV_AWS_18 (access logging): needs a real, separate log-destination
#   bucket (S3 disallows a bucket logging to itself) -- a genuine
#   design decision (shared across every bucket this tree creates, or
#   one per bucket) deferred as real, larger-scoped future work, not a
#   quick manifest edit.

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

  # Closes a real Checkov finding (CKV_AWS_28) -- this table's own
  # content (a lock record naming whichever module currently holds the
  # lock) is trivially reconstructible if lost, but point-in-time
  # recovery is a real, cheap safety net against an accidental
  # DeleteItem/TTL misconfiguration wiping every lock at once, which
  # would otherwise force every consuming module to manually recreate
  # its own lock record before Terraform could run again.
  point_in_time_recovery {
    enabled = true
  }

  tags = var.tags
}

# Deliberately NOT built, per the same 2026-09-18 Checkov triage as
# above -- disclosed accepted risk, not an oversight:
# - CKV_AWS_119 (DynamoDB encrypted with a customer-managed KMS CMK):
#   DynamoDB has encrypted every table at rest by default (AWS-owned
#   key) since 2018 with no opt-out -- this table is never actually
#   unencrypted. Same SSE-S3-not-KMS reasoning as this file's own S3
#   bucket above: a customer-managed CMK adds real operational cost
#   (key policy grants, rotation) with no documented compliance driver,
#   for a table whose own content is even more disposable than the
#   state bucket's -- a lock record naming whichever module currently
#   holds the lock, already covered by point_in_time_recovery above.
