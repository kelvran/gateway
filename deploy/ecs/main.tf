provider "aws" {
  region = var.aws_region
}

resource "aws_cloudwatch_log_group" "gateway" {
  name              = var.log_group_name
  retention_in_days = var.log_retention_days
  tags              = var.tags
}

resource "aws_ecs_task_definition" "gateway" {
  family                   = var.family
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.cpu
  memory                   = var.memory
  execution_role_arn       = var.execution_role_arn
  task_role_arn            = var.task_role_arn
  tags                     = var.tags

  container_definitions = jsonencode([
    {
      name      = "gateway"
      image     = var.image
      essential = true

      portMappings = [
        {
          containerPort = var.container_port
          protocol      = "tcp"
        }
      ]

      # Native Fargate secrets injection -- the ECS equivalent of
      # deploy/k8s/base/secret-placeholder.yaml's envFrom.secretRef, no
      # ExternalSecrets Operator or IRSA needed for this target.
      secrets = [
        for env_name, secret_arn in var.secrets : {
          name      = env_name
          valueFrom = secret_arn
        }
      ]

      # Container-level hard memory limit -- the Fargate equivalent of
      # deploy/k8s/base/deployment.yaml's own resources.limits.memory,
      # same load-bearing reason: cmd/gateway/main.go's automemlimit
      # integration reads this cgroup limit at startup.
      memory = var.container_memory

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.gateway.name
          "awslogs-region"        = var.aws_region
          "awslogs-stream-prefix" = "gateway"
        }
      }

      # No container-level `healthCheck` here, on purpose: ECS's own
      # container healthCheck is always exec-based (it runs `command`
      # INSIDE the container via the container runtime), and
      # gateway/Dockerfile builds `FROM scratch` -- confirmed directly
      # against that file -- with no shell and no `wget`/`curl` at all,
      # so any CMD-SHELL-style healthCheck here would simply never run.
      # The correct ECS-native equivalent of deploy/k8s/base/
      # deployment.yaml's httpGet livenessProbe (which works precisely
      # because kubelet makes the HTTP call from OUTSIDE the container)
      # is an ALB target group health check against /healthz, made from
      # outside the task the same way -- out of scope for this
      # task-definition-only module, since it isn't provisioning a load
      # balancer; add one alongside whatever ALB/NLB fronts this task.
    }
  ])
}
