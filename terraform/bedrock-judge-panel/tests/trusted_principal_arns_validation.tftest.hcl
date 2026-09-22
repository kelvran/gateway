# Direct regression proof for a real HIGH-severity finding from a fresh
# audit sweep: this module's assume_role trust-policy statement grants
# `principals { type = "AWS", identifiers = var.trusted_principal_arns }`
# with no Condition block at all -- a bare "*" or an account-root ARN
# (arn:aws:iam::<account>:root) in that list therefore trusts literally
# any AWS principal, or every principal in that account, respectively.
# These are `plan`-only cases: the validation blocks in variables.tf run
# before any AWS API call, so no real credentials or provider network
# access are needed.

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
  account_id  = "123456789012"
  create_role = true
}

run "wildcard_principal_is_rejected" {
  command = plan

  variables {
    trusted_principal_arns = ["*"]
  }

  expect_failures = [
    var.trusted_principal_arns,
  ]
}

run "account_root_principal_is_rejected" {
  command = plan

  variables {
    trusted_principal_arns = ["arn:aws:iam::123456789012:root"]
  }

  expect_failures = [
    var.trusted_principal_arns,
  ]
}

run "a_specific_role_arn_is_accepted" {
  command = plan

  variables {
    trusted_principal_arns = ["arn:aws:iam::123456789012:role/ci-runner"]
  }
}
