provider "aws" {
  shared_credentials_files  = var.shared_credentials_files
  profile                   = var.profile
  region                    = var.region
}

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}