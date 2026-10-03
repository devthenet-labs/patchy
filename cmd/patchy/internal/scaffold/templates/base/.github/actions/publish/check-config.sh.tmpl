#!/usr/bin/env bash
# Check a trusted publisher's repository variables before it asks for any
# credential, and print the AWS account ID for configure-aws-credentials'
# allowed-account-ids. An unset or malformed variable fails closed. The IAM
# trust policy (repository ID, owner ID, branch and job_workflow_ref) stays
# the real boundary; this only turns a misconfiguration into a clear error.
set -euo pipefail

fail() {
  echo "publisher configuration: $1" >&2
  exit 1
}

[[ "${IMAGE_KIND:-}" =~ ^(runtime|agent)$ ]] || fail "IMAGE_KIND must be runtime or agent"
[[ "${AWS_REGION:-}" =~ ^[a-z]{2}(-[a-z]+)+-[0-9]+$ ]] || fail "AWS_REGION is not an AWS region"
[[ "${ECR_REGISTRY:-}" =~ ^([0-9]{12})\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com$ ]] ||
  fail "ECR_REGISTRY must be <account>.dkr.ecr.<region>.amazonaws.com"
account="${BASH_REMATCH[1]}"
[[ "${BASH_REMATCH[2]}" == "$AWS_REGION" ]] || fail "ECR_REGISTRY is not in AWS_REGION"
[[ "${IMAGE_REPOSITORY:-}" =~ ^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)+$ ]] ||
  fail "the $IMAGE_KIND image repository must be a path such as team/images/app"
[[ "${ROLE_ARN:-}" =~ ^arn:aws:iam::([0-9]{12}):role/[A-Za-z0-9+=,.@_-]{1,64}$ ]] ||
  fail "the $IMAGE_KIND role must be an IAM role ARN"
[[ "${BASH_REMATCH[1]}" == "$account" ]] || fail "the $IMAGE_KIND role is not in the registry's account"

printf '%s\n' "$account"
