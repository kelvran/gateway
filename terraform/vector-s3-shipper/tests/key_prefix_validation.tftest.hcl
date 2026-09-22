# Direct regression proof for a real MEDIUM-severity finding from a
# fresh audit sweep: shipper_write_only's resource pattern is
# "${bucket_arn}/${var.key_prefix}*" -- an empty (or whitespace-only)
# key_prefix collapses that to "${bucket_arn}/*", granting the shipper
# IAM user PutObject on every key in the bucket instead of the one
# intended prefix.

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
  bucket_name = "kelvran-ci-dummy-bucket"
}

run "empty_key_prefix_is_rejected" {
  command = plan

  variables {
    key_prefix = ""
  }

  expect_failures = [
    var.key_prefix,
  ]
}

run "whitespace_only_key_prefix_is_rejected" {
  command = plan

  variables {
    key_prefix = "   "
  }

  expect_failures = [
    var.key_prefix,
  ]
}

run "a_real_prefix_is_accepted" {
  command = plan

  variables {
    key_prefix = "gatewayevents/v1/"
  }
}
