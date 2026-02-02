resource "aws_security_group" "build_agent" {
  name = coalesce(var.build_agent_security_group_name, "build-agent")
  description = "build agent security group"
  vpc_id      = var.vpc_id

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "build-agent"
  }
}