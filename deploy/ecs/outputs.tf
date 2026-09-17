output "task_definition_arn" {
  description = "ARN of the registered task definition (includes revision number) -- use as the `task_definition` argument for an aws_ecs_service resource."
  value       = aws_ecs_task_definition.gateway.arn
}

output "log_group_name" {
  description = "CloudWatch Logs group the container's logs ship to."
  value       = aws_cloudwatch_log_group.gateway.name
}
