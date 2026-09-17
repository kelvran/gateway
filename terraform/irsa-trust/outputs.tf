output "role_arn" {
  description = "ARN of the IRSA-trusted role -- use this as the value for deploy/k8s/base/serviceaccount.yaml's own eks.amazonaws.com/role-arn annotation."
  value       = aws_iam_role.irsa.arn
}
