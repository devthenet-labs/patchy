# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# tflint for the reference AWS module and its modules/app (hack/tf-lint.sh
# runs it in both with this file). Only the bundled terraform ruleset: the
# AWS ruleset is a separate plugin download, and the module's inputs are
# validated in HCL anyway. "all" adds documented and typed variables and
# outputs, naming conventions and the standard module layout to the default
# "recommended" preset.

plugin "terraform" {
  enabled = true
  preset  = "all"
}
