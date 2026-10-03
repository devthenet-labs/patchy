#!/usr/bin/env bash
# Copy a validated OCI layout to this image kind's immutable ECR tags:
#   runtime, PR head:  sha-<40 hex>
#   runtime, main: sha-<40 hex> and main-<40 hex> (the main- tag is
#                      what ECR lifecycle keeps, so unchanged repositories
#                      can still preview the default branch long after the
#                      sha- expiry)
#   agent, main only: AGENT_TAG, the toolchain-v<N> tag
#                      .patchy/agent.yaml declares at the built commit
# Every tag is checked on its own, so a re-run still adds main-<sha> when
# sha-<sha> already exists. A different image at an existing tag is a hard
# failure, never an overwrite.
set -euo pipefail

fail() {
  echo "copy-image: $1" >&2
  exit 1
}

[[ "${IMAGE_SHA:-}" =~ ^[a-f0-9]{40}$ ]] || fail "IMAGE_SHA must be 40 hex characters"
[[ "${IMAGE_DIGEST:-}" =~ ^sha256:[a-f0-9]{64}$ ]] || fail "IMAGE_DIGEST must be a sha256 digest"
bash "$(dirname -- "${BASH_SOURCE[0]}")/check-config.sh" > /dev/null
registry=$ECR_REGISTRY
repository=$IMAGE_REPOSITORY
case "$IMAGE_KIND/${IMAGE_SOURCE:-}" in
  runtime/pull_request) tags=("sha-$IMAGE_SHA") ;;
  runtime/main) tags=("sha-$IMAGE_SHA" "main-$IMAGE_SHA") ;;
  agent/main)
    [[ "${AGENT_TAG:-}" =~ ^toolchain-v[1-9][0-9]{0,5}$ ]] || fail "AGENT_TAG must be toolchain-v<N>"
    tags=("$AGENT_TAG") ;;
  *) fail "refusing a $IMAGE_KIND image from ${IMAGE_SOURCE:-an unknown source}" ;;
esac

# Prints the digest at a tag, nothing when the tag does not exist, and fails
# on any other error.
existing_digest() {
  local out
  if out=$(aws ecr describe-images --repository-name "$repository" --image-ids "imageTag=$1" \
      --query 'imageDetails[0].imageDigest' --output text 2> "$RUNNER_TEMP/ecr-describe-error"); then
    printf '%s\n' "$out"
  else
    grep -q '(ImageNotFoundException)' "$RUNNER_TEMP/ecr-describe-error" || { cat "$RUNNER_TEMP/ecr-describe-error" >&2; return 1; }
  fi
}

missing=()
for tag in "${tags[@]}"; do
  existing=$(existing_digest "$tag")
  if [[ -z "$existing" ]]; then
    missing+=("$tag")
  elif [[ "$existing" != "$IMAGE_DIGEST" ]]; then
    hint=""
    if [[ "$IMAGE_KIND" == agent ]]; then
      hint="; tags are immutable, so bump the toolchain-v<N> tag in .patchy/agent.yaml to publish a changed toolchain"
    fi
    fail "$repository:$tag already holds $existing, not $IMAGE_DIGEST$hint"
  else
    printf 'Already published: %s/%s:%s@%s\n' "$registry" "$repository" "$tag" "$existing"
  fi
done
if [[ ${#missing[@]} -eq 0 ]]; then
  exit 0
fi

umask 077
authfile="$RUNNER_TEMP/ecr-auth.json"
trap 'rm -f -- "$authfile"' EXIT
aws ecr get-login-password | skopeo login --authfile "$authfile" --username AWS --password-stdin "$registry"
for tag in "${missing[@]}"; do
  skopeo copy --preserve-digests --authfile "$authfile" "oci:$RUNNER_TEMP/validated" "docker://$registry/$repository:$tag"
  actual=$(existing_digest "$tag")
  [[ "$actual" == "$IMAGE_DIGEST" ]] || fail "$repository:$tag reads back as ${actual:-nothing}, not $IMAGE_DIGEST"
  printf 'Published %s/%s:%s@%s\n' "$registry" "$repository" "$tag" "$actual"
done
