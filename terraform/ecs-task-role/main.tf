# Both roles trust the same principal (ecs-tasks.amazonaws.com) -- ECS
# Fargate's own documented requirement for both the execution role and
# the task role, unlike the two structurally different trust policies
# IRSA/kiam-kube2iam each need on the Kubernetes side.
data "aws_iam_policy_document" "ecs_tasks_trust" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

# --- Task execution role: the ECS agent itself, before the container starts ---

resource "aws_iam_role" "execution" {
  name               = "${var.name_prefix}-execution-role"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_trust.json
  tags               = var.tags
}

# AWS's own managed policy covers ECR image pull + CloudWatch Logs --
# it does NOT include secretsmanager:GetSecretValue, so the scoped
# policy below is required in addition to it, not instead of it.
resource "aws_iam_role_policy_attachment" "execution_managed" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

data "aws_iam_policy_document" "execution_secrets" {
  statement {
    sid       = "ECSExecutionReadUpstreamCredentials"
    effect    = "Allow"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = var.secrets_manager_secret_arns
  }
}

resource "aws_iam_policy" "execution_secrets" {
  name        = "${var.name_prefix}-execution-secrets-policy"
  description = "Read-only Secrets Manager access for the ECS task execution role to inject secrets into the task definition's own `secrets` block at launch."
  policy      = data.aws_iam_policy_document.execution_secrets.json
  tags        = var.tags
}

resource "aws_iam_role_policy_attachment" "execution_secrets" {
  role       = aws_iam_role.execution.name
  policy_arn = aws_iam_policy.execution_secrets.arn
}

# --- Task role: the gateway process itself, once running ---

resource "aws_iam_role" "task" {
  name               = "${var.name_prefix}-task-role"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_trust.json
  tags               = var.tags
}

# Only created when bedrock_model_arns is non-empty -- a task role with
# zero attached policies is a real, harmless no-op (the gateway process
# itself has no AWS API calls to make when every deployment in
# config.yaml is a non-Bedrock provider), not an error.
data "aws_iam_policy_document" "task_bedrock" {
  count = length(var.bedrock_model_arns) > 0 ? 1 : 0

  statement {
    sid       = "ECSTaskInvokeBedrock"
    effect    = "Allow"
    actions   = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"]
    resources = var.bedrock_model_arns
  }
}

resource "aws_iam_policy" "task_bedrock" {
  count       = length(var.bedrock_model_arns) > 0 ? 1 : 0
  name        = "${var.name_prefix}-task-bedrock-policy"
  description = "Scoped Bedrock InvokeModel access for the gateway process itself, when config.yaml configures a Bedrock deployment."
  policy      = data.aws_iam_policy_document.task_bedrock[0].json
  tags        = var.tags
}

resource "aws_iam_role_policy_attachment" "task_bedrock" {
  count      = length(var.bedrock_model_arns) > 0 ? 1 : 0
  role       = aws_iam_role.task.name
  policy_arn = aws_iam_policy.task_bedrock[0].arn
}
