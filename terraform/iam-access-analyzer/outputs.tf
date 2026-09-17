output "analyzer_arn" {
  description = "ARN of the unused-access analyzer."
  value       = aws_accessanalyzer_analyzer.unused_access.arn
}
