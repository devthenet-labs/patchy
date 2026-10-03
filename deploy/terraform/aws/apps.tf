# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

# One module per application repository. The module works on its own too:
# instantiate deploy/terraform/aws/modules/app directly to add an app to an
# account whose platform half is managed elsewhere.
module "app" {
  source   = "./modules/app"
  for_each = var.apps

  slug                     = each.key
  github                   = each.value.github
  preview                  = each.value.preview
  github_oidc_provider_arn = local.github_oidc_provider_arn
  agent_path_prefix        = var.agent_path_prefix
  role_name_prefix         = local.app_role_name_prefix
  tags                     = var.tags
}
