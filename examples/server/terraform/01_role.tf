locals {
  bakery_agent_secret_enabled = (
    var.bakery_agent_secret_arn != null && var.bakery_agent_secret_arn != ""
  )
  bakery_agent_execution_secret_statement = [{
    Sid      = "AllowGetBuildAgentPullSecret"
    Effect   = "Allow"
    Action   = ["secretsmanager:GetSecretValue"]
    Resource = var.bakery_agent_secret_arn
  }]

  bakery_agent_task_enabled = (
    var.bakery_agent_s3_bucket_name != null && var.bakery_agent_s3_bucket_name != ""
  )
  bakery_agent_task_s3_policy = {
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
          "arn:aws:s3:::${var.bakery_agent_s3_bucket_name}",
          "arn:aws:s3:::${var.bakery_agent_s3_bucket_name}/*"
        ]
      }
    ]
  }
}

resource "aws_iam_role" "bakery_agent_execution_role" {
  name = coalesce(var.bakery_agent_execution_role_name, "bakery-agent-execution")

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

resource "aws_iam_role_policy_attachment" "bakery_agent_execution_attach" {
  role       = aws_iam_role.bakery_agent_execution_role.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_iam_role_policy" "bakery_agent_execution_policy" {
  for_each = local.bakery_agent_secret_enabled ? { this = true } : {}

  name = coalesce(var.bakery_agent_execution_policy_name, "bakery-agent-execution")
  role = aws_iam_role.bakery_agent_execution_role.id

  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = local.bakery_agent_execution_secret_statement
  })
}

resource "aws_iam_role" "bakery_agent_task_role" {
  for_each = local.bakery_agent_task_enabled ? { this = true } : {}

  name = coalesce(var.bakery_agent_task_role_name, "bakery-agent-task")

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

resource "aws_iam_role_policy" "bakery_agent_task_policy" {
  for_each = local.bakery_agent_task_enabled ? { this = true } : {}

  name = coalesce(var.bakery_agent_task_role_name, "bakery-agent-task")
  role = aws_iam_role.bakery_agent_task_role[each.key].id

  policy = jsonencode(local.bakery_agent_task_s3_policy)
}