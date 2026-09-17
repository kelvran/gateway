# `terraform/backend-bootstrap`

Provisions the one thing every other module in this tree needs before it can
use remote state: an S3 bucket (versioned, SSE-encrypted, public access
blocked) plus a DynamoDB lock table.

## Apply this ONCE, BY HAND. Never from CI.

This module is the classic "what provisions the state bucket" chicken-and-egg
exception. It has no remote backend of its own — it deliberately keeps LOCAL
state (whatever is in `terraform.tfstate` after you run it). Do not add a
`backend "s3" {}` block here pointing at the very bucket it creates.

```sh
cd terraform/backend-bootstrap
terraform init
terraform apply -var="bucket_name=<your-globally-unique-bucket-name>"
```

Keep the resulting `terraform.tfstate` file somewhere safe (it is the only
record of this bucket/table's own Terraform-managed identity) — this is the
one piece of state in this whole tree that is *not* itself stored in the
bucket it creates.

## After applying: point every other module at it

Add a real backend block to each of `terraform/bedrock-judge-panel`,
`terraform/vector-s3-shipper`, and any future module in this tree, using this
module's own outputs:

```hcl
terraform {
  backend "s3" {
    bucket         = "<bucket_name output>"
    key            = "bedrock-judge-panel/terraform.tfstate" # unique per module
    region         = "<aws_region output>"
    dynamodb_table = "<dynamodb_table_name output>"
    encrypt        = true
  }
}
```

Then run `terraform init -migrate-state` in that module to move its existing
local state into the new backend.

**This repo does not do this by default** — every module in `terraform/`
still uses local state today, exactly as before this module existed. Wiring
the block above into a consuming module is a deliberate, disclosed,
per-operator decision, made only after this bootstrap has actually been
applied against a real AWS account — never assumed or pre-wired against a
bucket that doesn't exist yet.

## Alternative: skip this module entirely

See `../README.md` for the equivalent Terraform Cloud/HCP `cloud {}` block —
a real, buildable alternative to self-managing an S3 bucket and DynamoDB
table at all. Pick one path, not both, per module.
