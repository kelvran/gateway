provider "aws" {
  region = var.aws_region
}

locals {
  oidc_provider_arn = "arn:aws:iam::${var.account_id}:oidc-provider/${var.oidc_provider_url}"
}

# Standard IRSA trust-policy shape (AWS's own documented pattern): a
# Federated principal naming the cluster's OIDC provider, restricted via
# two StringEquals conditions on that provider's own "sub"/"aud" claims
# -- "sub" pins this role to exactly one namespace/ServiceAccount pair
# (not every ServiceAccount in the cluster), "aud" pins it to the STS
# audience EKS's own webhook always sets. Omitting either condition is
# the standard IRSA misconfiguration this pattern guards against (a bare
# Federated trust with no condition lets ANY ServiceAccount in the
# cluster assume the role via its own projected token).
data "aws_iam_policy_document" "irsa_trust" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [local.oidc_provider_arn]
    }

    condition {
      test     = "StringEquals"
      variable = "${var.oidc_provider_url}:sub"
      values   = ["system:serviceaccount:${var.namespace}:${var.service_account_name}"]
    }

    condition {
      test     = "StringEquals"
      variable = "${var.oidc_provider_url}:aud"
      values   = ["sts.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "irsa" {
  name               = "${var.name_prefix}-role"
  assume_role_policy = data.aws_iam_policy_document.irsa_trust.json
  tags               = var.tags
}

# Read-only, scoped to the exact secret ARNs passed in -- no
# secretsmanager:PutSecretValue/DeleteSecret/ListSecrets anywhere. The
# gateway only ever needs to READ its own upstream provider credentials
# at startup, per deploy/k8s/base/secret-placeholder.yaml's own
# "external consumer, not owner" framing of this data.
data "aws_iam_policy_document" "secrets_access" {
  statement {
    sid       = "IRSAReadUpstreamCredentials"
    effect    = "Allow"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = var.secrets_manager_secret_arns
  }
}

resource "aws_iam_policy" "secrets_access" {
  name        = "${var.name_prefix}-secrets-policy"
  description = "Read-only Secrets Manager access for the IRSA-bound gateway ServiceAccount, scoped to exactly the secrets it needs."
  policy      = data.aws_iam_policy_document.secrets_access.json
  tags        = var.tags
}

resource "aws_iam_role_policy_attachment" "secrets_access" {
  role       = aws_iam_role.irsa.name
  policy_arn = aws_iam_policy.secrets_access.arn
}
