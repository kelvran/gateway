provider "aws" {
  region = var.aws_region
}

resource "aws_accessanalyzer_analyzer" "unused_access" {
  analyzer_name = var.analyzer_name
  type          = "ACCOUNT_UNUSED_ACCESS"
  tags          = var.tags

  configuration {
    unused_access {
      unused_access_age = var.unused_access_age
    }
  }
}
