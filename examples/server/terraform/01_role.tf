locals {
  build_agent_secret_enabled = (
    var.build_agent_secret_arn != null && var.build_agent_secret_arn != ""
  )
  build_agent_execution_secret_statement = [{
    Sid      = "AllowGetBuildAgentPullSecret"
    Effect   = "Allow"
    Action   = ["secretsmanager:GetSecretValue"]
    Resource = var.build_agent_secret_arn
  }]

  build_agent_task_enabled = (
    var.build_agent_s3_bucket_name != null && var.build_agent_s3_bucket_name != ""
  )
  build_agent_task_s3_policy = {
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "AllowReadBuildContext"
        Effect = "Allow"
        Action = [
          "s3:GetObject",
          "s3:ListBucket"
        ]
        Resource = [
          "arn:aws:s3:::${var.build_agent_s3_bucket_name}",
          "arn:aws:s3:::${var.build_agent_s3_bucket_name}/*"
        ]
      }
    ]
  }
}

resource "aws_iam_role" "build_agent_execution_role" {
  name = coalesce(var.build_agent_execution_role_name, "build-agent-execution")

  assume_role_policy = jsonencode({
    Version = "2012-10-17",
    Statement = [
      {
        Effect = "Allow",
        Principal = {
          Service = "ecs-tasks.amazonaws.com"
        },
        Action = "sts:AssumeRole"
      }
    ]
  })
}

resource "aws_iam_role_policy_attachment" "build_agent_execution_attach" {
  role       = aws_iam_role.build_agent_execution_role.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_iam_role_policy" "build_agent_execution_policy" {
  for_each = local.build_agent_secret_enabled ? { this = true } : {}

  name = coalesce(var.build_agent_execution_policy_name, "build-agent-execution")
  role = aws_iam_role.build_agent_execution_role.id

  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = local.build_agent_execution_secret_statement
  })
}

resource "aws_iam_role" "build_agent_task_role" {
  for_each = local.build_agent_task_enabled ? { this = true } : {}

  name = coalesce(var.build_agent_task_role_name, "build-agent-task")

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Principal = { 
          Service = "ecs-tasks.amazonaws.com"
        }
        Action = "sts:AssumeRole"
      }
    ]
  })
}

resource "aws_iam_role_policy" "build_agent_task_policy" {
  for_each = local.build_agent_task_enabled ? { this = true } : {}

  name = coalesce(var.build_agent_task_role_name, "build-agent-task")
  role = aws_iam_role.build_agent_task_role[each.key].id

  policy = jsonencode(local.build_agent_task_s3_policy)
}