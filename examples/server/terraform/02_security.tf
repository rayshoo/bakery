resource "aws_security_group" "bakery_agent" {
  name = coalesce(var.bakery_agent_security_group_name, "bakery-agent")
  description = "bakery agent security group"
  vpc_id      = var.vpc_id

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "bakery-agent"
  }
}