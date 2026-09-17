output "bucket_name" {
  description = "Name of the state bucket -- use this as the `bucket` value in every OTHER module's own `backend \"s3\" {}` block once this module has been applied."
  value       = aws_s3_bucket.state.id
}

output "dynamodb_table_name" {
  description = "Name of the lock table -- use this as the `dynamodb_table` value in every OTHER module's own `backend \"s3\" {}` block."
  value       = aws_dynamodb_table.locks.name
}

output "aws_region" {
  description = "Region the bucket/table were created in -- use this as the `region` value in every OTHER module's own `backend \"s3\" {}` block."
  value       = var.aws_region
}
