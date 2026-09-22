# Standalone S3 bucket + write-only IAM user for the already-blocked
# vector-s3 Compose profile, per
# docs/operations/vector-gatewayevents-s3-compose.yaml's own real
# bucket/key-prefix/credential shape (KELVRAN_GATEWAYEVENTS_BUCKET,
# AWS_REGION env vars; Vector's default AWS credential-provider chain
# resolves an access key + secret from the container's own environment,
# no auth: block) and
# docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md's §4
# retention design.

variable "bucket_name" {
  description = "Name of the S3 bucket gatewayevents_v1 objects ship to -- must be globally unique, per S3's own naming rules. No safe generated default (a random suffix would fight KELVRAN_GATEWAYEVENTS_BUCKET's own static env-var configuration in the Compose file), so this is required."
  type        = string
}

variable "aws_region" {
  description = "AWS Region the bucket is created in -- matches the AWS_REGION env var the accompanying vector-gatewayevents-s3-compose.yaml sink config already reads."
  type        = string
  default     = "us-east-1"
}

variable "key_prefix" {
  description = "The one S3 key prefix Vector ever writes under and the only prefix the shipper IAM user's write-only policy is scoped to -- matches vector-gatewayevents-s3-compose.yaml's own aws_s3 sink key_prefix (\"gatewayevents/v1/dt=...\") down to its static leading segment."
  type        = string
  default     = "gatewayevents/v1/"

  # An empty (or whitespace-only) value collapses
  # shipper_write_only's resource pattern
  # ("${bucket_arn}/${key_prefix}*") to "${bucket_arn}/*" -- bucket-wide
  # PutObject access instead of the one intended prefix. Found by a
  # fresh audit sweep.
  validation {
    condition     = length(trimspace(var.key_prefix)) > 0
    error_message = "key_prefix must not be empty or whitespace-only -- an empty value collapses the shipper IAM policy's resource pattern to bucket-wide access (\"<bucket_arn>/*\") instead of the intended prefix."
  }
}

variable "retention_days" {
  description = "Days after which an object under key_prefix expires, per docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md §4: a single S3 Lifecycle rule, no storage-class transition -- explicitly reasoned-but-uncalibrated (see that RFC's own §4/§8), not a compliance-derived number. Revisit alongside that RFC if it changes; do not silently drift this default out of sync with it."
  type        = number
  default     = 90
}

variable "shipper_user_name" {
  description = "Name of the write-only IAM user Vector authenticates as."
  type        = string
  default     = "kelvran-gatewayevents-shipper"
}

variable "tags" {
  description = "Common tags applied to every resource this module creates."
  type        = map(string)
  default     = {}
}
