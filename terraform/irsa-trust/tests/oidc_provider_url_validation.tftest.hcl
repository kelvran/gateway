# Direct regression proof for a real MEDIUM-severity finding from a
# fresh audit sweep: main.tf builds the trust-policy condition keys as
# "${var.oidc_provider_url}:sub"/":aud" -- IAM's own OIDC condition keys
# are always scheme-free, but `aws eks describe-cluster`'s real
# oidc.issuer value DOES include a leading "https://". Passing that
# value verbatim silently builds a condition key IAM can never match.

provider "aws" {
  region                      = "us-east-1"
  access_key                  = "test"
  secret_key                  = "test"
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  skip_region_validation      = true
}

variables {
  account_id                  = "123456789012"
  secrets_manager_secret_arns = ["arn:aws:secretsmanager:us-east-1:123456789012:secret:example-abc123"]
}

run "https_scheme_is_rejected" {
  command = plan

  variables {
    oidc_provider_url = "https://oidc.eks.us-east-1.amazonaws.com/id/EXAMPLED539D4633E53DE1B71EXAMPLE"
  }

  expect_failures = [
    var.oidc_provider_url,
  ]
}

run "http_scheme_is_rejected" {
  command = plan

  variables {
    oidc_provider_url = "http://oidc.eks.us-east-1.amazonaws.com/id/EXAMPLED539D4633E53DE1B71EXAMPLE"
  }

  expect_failures = [
    var.oidc_provider_url,
  ]
}

run "a_scheme_free_url_is_accepted" {
  command = plan

  variables {
    oidc_provider_url = "oidc.eks.us-east-1.amazonaws.com/id/EXAMPLED539D4633E53DE1B71EXAMPLE"
  }
}
