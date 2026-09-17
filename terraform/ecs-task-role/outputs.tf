output "execution_role_arn" {
  description = "ARN of the task execution role -- use as the `execution_role_arn` variable in ../../deploy/ecs/."
  value       = aws_iam_role.execution.arn
}

output "task_role_arn" {
  description = "ARN of the task role -- use as the `task_role_arn` variable in ../../deploy/ecs/."
  value       = aws_iam_role.task.arn
}
