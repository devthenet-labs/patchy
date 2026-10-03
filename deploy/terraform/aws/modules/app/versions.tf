# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

terraform {
  # 1.9: variable validations that read other variables (the IAM name length
  # check reads the slug and the role name prefix together).
  required_version = ">= 1.9"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.0"
    }
  }
}
