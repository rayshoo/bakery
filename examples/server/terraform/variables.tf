variable  "profile" {
  description = "credential profile"
  type        = any
  default     = {}
}
variable "shared_credentials_files" {
  description = "shared credentials files"
  type        = any
  default     = {}
}
variable "region" {
  description = "Region"
  type        = any
  default     = {}
}
variable "vpc_id" {
  description = "VPC ID where security group will be created"
  type        = string
}

variable "cluster_name" {
  description = "ECS cluster name"
  type        = string
}

variable "build_agent_secret_arn" {
  description = "Secrets Manager secret ARN for build agent pull/registry credentials (optional). If set, execution role gets GetSecretValue permission."
  type        = string
  default     = null
}

variable "build_agent_s3_bucket_name" {
  description = "S3 bucket name for build context (optional). If set, task role/policy will be created with S3 read permissions."
  type        = string
  default     = null
}

variable "build_agent_execution_role_name" {
  description = "IAM execution role name for build agent (optional). Default: build-agent-execution"
  type        = string
  default     = null
}

variable "build_agent_execution_policy_name" {
  description = "IAM inline policy name attached to execution role (optional). Default: build-agent-execution"
  type        = string
  default     = null
}

variable "build_agent_task_role_name" {
  description = "IAM task role name (and inline policy name) for build agent (optional). Default: build-agent-task"
  type        = string
  default     = null
}

variable "build_agent_security_group_name" {
  description = "Security group name for build agent (optional). Default: build-agent"
  type        = string
  default     = null
}