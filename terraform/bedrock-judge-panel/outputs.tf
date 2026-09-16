output "policy_arn" {
  description = "ARN of the scoped Bedrock judge-panel IAM policy."
  value       = aws_iam_policy.bedrock_judge_panel.arn
}

output "role_arn" {
  description = "ARN of the assumable judge-panel role, or null when create_role is false."
  value       = var.create_role ? aws_iam_role.bedrock_judge_panel[0].arn : null
}
