#!/bin/sh
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# The gate for the reference AWS terraform module (deploy/terraform/aws and its
# modules/app): format check, validate, tflint, the offline mock-provider
# tests, and the terraform-docs drift check on both READMEs. Nothing here
# needs AWS credentials; `terraform init` downloads the AWS provider only for
# its schema, cached across both modules (and across runs) in
# TF_PLUGIN_CACHE_DIR.
set -eu

root=deploy/terraform/aws
: "${TF_PLUGIN_CACHE_DIR:=${XDG_CACHE_HOME:-$HOME/.cache}/patchy/terraform-plugins}"
export TF_PLUGIN_CACHE_DIR
mkdir -p "$TF_PLUGIN_CACHE_DIR"

terraform fmt -check -recursive -diff "$root"

for dir in "$root" "$root/modules/app"; do
  echo "--- $dir"
  terraform -chdir="$dir" init -backend=false -input=false -no-color >/dev/null
  terraform -chdir="$dir" validate -no-color
  tflint --chdir="$dir" --config="$PWD/$root/.tflint.hcl" --no-color
  terraform -chdir="$dir" test -no-color
done

# terraform-docs renders the inputs/outputs tables into both READMEs (the
# root's .terraform-docs.yml recurses into modules/); --output-check fails
# when either is stale. Regenerate with: terraform-docs deploy/terraform/aws
terraform-docs --output-check "$root"
