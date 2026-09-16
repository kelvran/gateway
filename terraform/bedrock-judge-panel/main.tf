# Three-statement-per-model IAM pattern for Bedrock global cross-Region
# inference, per AWS's own documented "IAM policy requirements for global
# cross-Region inference" (docs.aws.amazon.com/bedrock/latest/userguide/
# global-cross-region-inference.html, live-verified 2026-09-16 -- not
# assumed from a geo-scoped (us./eu./au./jp.) cross-Region inference
# profile's own, structurally different requirement):
#
#   1. The Regional inference-profile ARN itself (account-scoped),
#      gated to this module's own aws_region via aws:RequestedRegion.
#   2. The Regional foundation-model ARN (NOT account-scoped -- an
#      account ID here causes authorization failures, per that same
#      doc), gated to aws_region AND the exact inference-profile ARN via
#      bedrock:InferenceProfileArn.
#   3. The GLOBAL foundation-model ARN (no region, no account --
#      `arn:aws:bedrock:::foundation-model/...`, intentionally), gated to
#      aws:RequestedRegion == "unspecified" (what Bedrock actually sets
#      for this one, Region-agnostic evaluation) AND the same
#      bedrock:InferenceProfileArn condition. This third statement is the
#      one that actually enables cross-Region routing; omitting it is
#      the single most common real-world misconfiguration this pattern
#      guards against.
#
# All three grant BOTH bedrock:InvokeModel and
# bedrock:InvokeModelWithResponseStream, since the Bedrock Converse API
# (what evals/evals/judge/providers.py's make_bedrock_call_model actually
# calls) uses streaming internally regardless of whether the CALLER asked
# for a streamed response.
locals {
  bedrock_statements = flatten([
    for model_idx, model in var.bedrock_model_names : [
      {
        sid       = "BedrockJudgePanelModel${model_idx}InferenceProfile"
        actions   = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"]
        resources = ["arn:aws:bedrock:${var.aws_region}:${var.account_id}:inference-profile/global.${model}"]
        conditions = [
          { test = "StringEquals", variable = "aws:RequestedRegion", values = [var.aws_region] },
        ]
      },
      {
        sid       = "BedrockJudgePanelModel${model_idx}RegionalFoundationModel"
        actions   = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"]
        resources = ["arn:aws:bedrock:${var.aws_region}::foundation-model/${model}"]
        conditions = [
          { test = "StringEquals", variable = "aws:RequestedRegion", values = [var.aws_region] },
          {
            test     = "StringEquals"
            variable = "bedrock:InferenceProfileArn"
            values   = ["arn:aws:bedrock:${var.aws_region}:${var.account_id}:inference-profile/global.${model}"]
          },
        ]
      },
      {
        sid       = "BedrockJudgePanelModel${model_idx}GlobalFoundationModel"
        actions   = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"]
        resources = ["arn:aws:bedrock:::foundation-model/${model}"]
        conditions = [
          { test = "StringEquals", variable = "aws:RequestedRegion", values = ["unspecified"] },
          {
            test     = "StringEquals"
            variable = "bedrock:InferenceProfileArn"
            values   = ["arn:aws:bedrock:${var.aws_region}:${var.account_id}:inference-profile/global.${model}"]
          },
        ]
      },
    ]
  ])
}

data "aws_iam_policy_document" "bedrock_judge_panel" {
  dynamic "statement" {
    for_each = { for s in local.bedrock_statements : s.sid => s }
    content {
      sid       = statement.value.sid
      effect    = "Allow"
      actions   = statement.value.actions
      resources = statement.value.resources

      dynamic "condition" {
        for_each = statement.value.conditions
        content {
          test     = condition.value.test
          variable = condition.value.variable
          values   = condition.value.values
        }
      }
    }
  }
}

resource "aws_iam_policy" "bedrock_judge_panel" {
  name        = "${var.name_prefix}-policy"
  description = "Scoped Bedrock InvokeModel/InvokeModelWithResponseStream access for evals' judge panel (Claude Sonnet 5 + Haiku 4.5 via global cross-Region inference profiles)."
  policy      = data.aws_iam_policy_document.bedrock_judge_panel.json
  tags        = var.tags
}

data "aws_iam_policy_document" "assume_role" {
  count = var.create_role ? 1 : 0

  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "AWS"
      identifiers = var.trusted_principal_arns
    }
  }
}

resource "aws_iam_role" "bedrock_judge_panel" {
  count              = var.create_role ? 1 : 0
  name               = "${var.name_prefix}-role"
  assume_role_policy = data.aws_iam_policy_document.assume_role[0].json
  tags               = var.tags
}

resource "aws_iam_role_policy_attachment" "bedrock_judge_panel" {
  count      = var.create_role ? 1 : 0
  role       = aws_iam_role.bedrock_judge_panel[0].name
  policy_arn = aws_iam_policy.bedrock_judge_panel.arn
}
