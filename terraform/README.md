# Terraform for `kelvran`

Provisions the cloud infrastructure layer neither Docker Compose nor the
Kustomize manifests at `../deploy/k8s/` touch: IAM, S3, and (for a real EKS
deployment) the IRSA trust relationship ESO needs. Per
`docs/upgrade-research/terraform-iac-deployment-automation-2026-09-15.md`'s
own scope finding — Terraform owns cloud infrastructure (IAM/networking/
managed services); workload-layer resources stay in Kustomize/Compose.

## Modules

| Module | What it provisions | Applied against |
|---|---|---|
| `bedrock-judge-panel/` | Scoped IAM role/policy for `evals`' Bedrock judge panel | An existing AWS account, no cluster needed |
| `vector-s3-shipper/` | S3 bucket + write-only IAM user for the `vector-s3` Compose profile | An existing AWS account, no cluster needed |
| `backend-bootstrap/` | The S3 bucket + DynamoDB table every OTHER module's own remote state can live in | Applied once, by hand — see its own README |
| `irsa-trust/` | IAM role with an OIDC-federated trust policy for an EXISTING EKS cluster's ServiceAccount (IRSA) | An existing EKS cluster with an OIDC provider already registered |
| `ecs-task-role/` | ECS task execution role + task role, scoped to Secrets Manager ARNs | An existing AWS account, no cluster needed |
| `iam-access-analyzer/` | IAM Access Analyzer, unused-access finding type (catches forgotten, not leaked, credentials) | An existing AWS account, no cluster needed |

Every module is a standalone root module — no module in this tree reads
another module's remote state via a `terraform_remote_state` data source.
Cross-module wiring (e.g. `irsa-trust`'s output role ARN going into a
`ServiceAccount` annotation) is always a manual, documented copy, per
`docs/upgrade-research/terraform-iac-deployment-automation-2026-09-15.md`'s
own Finding 5 ("cloud-resource-provisioning state kept separate from any
in-cluster-resource state, never merged into one workspace").

Each module follows the same flat, one-file-per-concern convention:
`main.tf`/`variables.tf`/`outputs.tf`/`versions.tf`, no nested `modules/`
subfolder — matching the LiteLLM/AWS-reference-architecture precedent that
research doc's own Finding 5 surveyed.

## Terraform state: two real, buildable paths — pick one

Every module above defaults to **local state** (no `backend` block at all).
Local state is fine for a single-maintainer, single-machine workflow, but has
no locking and no durability if that machine is lost. Two real alternatives
exist; this repo does not choose one for you:

### Path 1 — self-hosted S3 + DynamoDB (`backend-bootstrap/`)

Apply `terraform/backend-bootstrap/` once, by hand (see its own README), then
add a `backend "s3" { ... }` block to each consuming module pointing at the
bucket/table it created. You own and pay for the bucket/table directly; no
third-party account needed beyond AWS itself.

### Path 2 — Terraform Cloud / HCP (free tier)

No bootstrap module needed — [Terraform Cloud's free tier](https://developer.hashicorp.com/terraform/cloud-docs)
provides remote state storage, locking, and a run history UI with zero
self-hosted infrastructure. Add this block to a module's own `versions.tf`
(replacing the bare `terraform { ... }` block, not alongside a `backend "s3"`
block — a module has exactly one backend):

```hcl
terraform {
  required_version = ">= 1.5"

  cloud {
    organization = "<your-tfc-organization>"
    workspaces {
      name = "kelvran-bedrock-judge-panel" # one workspace name per module
    }
  }

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}
```

Then `terraform login` once locally (or configure `TF_TOKEN_<hostname>` in
CI) and run `terraform init` — no AWS credentials for the backend itself are
needed, since state lives in Terraform Cloud, not S3.

**Neither path is wired into any module by default.** Every module in this
tree still applies with local state exactly as before both paths existed —
switching is a deliberate, disclosed, per-operator decision.

## CI

`.github/workflows/iac-scan.yml` runs `terraform fmt -check`/`init
-backend=false`/`validate` and a structural-only `terraform plan` (fake
credentials, no real AWS account touched) against every module in this
directory on every push/PR — see that workflow's own comments for why a real
`plan`/`apply` gate against a live account isn't wired yet (no backend is
actually configured in any module today, and CI has no real AWS credentials).
A Checkov IaC-security scan (report-only, `soft_fail: true`) also covers this
whole directory.
