output "bucket_name" {
  description = "The bucket's own name -- set as KELVRAN_GATEWAYEVENTS_BUCKET in the accompanying Compose deployment's .env.vector."
  value       = aws_s3_bucket.gatewayevents.id
}

output "bucket_arn" {
  description = "ARN of the gatewayevents_v1 destination bucket."
  value       = aws_s3_bucket.gatewayevents.arn
}

output "shipper_access_key_id" {
  description = "Access key ID for the write-only shipper IAM user -- set as AWS_ACCESS_KEY_ID in .env.vector."
  value       = aws_iam_access_key.shipper.id
}

output "shipper_secret_access_key" {
  description = "Secret access key for the write-only shipper IAM user -- set as AWS_SECRET_ACCESS_KEY in .env.vector. Sensitive: never logged, only ever readable from state or a deliberate `terraform output -raw` call."
  value       = aws_iam_access_key.shipper.secret
  sensitive   = true
}
