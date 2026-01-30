output "cluster_name" {
  value = aws_ecs_cluster.this.name
}

output "cluster_arn" {
  value = aws_ecs_cluster.this.arn
}

output "execution_role_arn" {
  value = aws_iam_role.build_agent_execution_role.arn
}

output "task_role_arn" {
  value = try(aws_iam_role.build_agent_task_role["this"].arn, null)
}

output "security_group_id" {
  value = aws_security_group.build_agent.id
}