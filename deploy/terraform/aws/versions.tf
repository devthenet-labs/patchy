# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

terraform {
  # 1.9: variable validations that read other variables (the IAM name lengths,
  # the preview ALB name and the subnet sets are checked together).
  required_version = ">= 1.9"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.0"
    }
  }
}
