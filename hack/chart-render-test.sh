#!/bin/sh
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Render assertions for charts/patchy: what hack/helm-lint.sh's plain renders
# cannot see. Which PATCHY_* keys land in which controller's ConfigMap, which
# Deployment mounts what, that each render-time guard fails with its message
# and passes once its value is fixed, and that the feature-off render carries
# none of it. Fixtures live in hack/testdata/chart-render/; yq (pinned in
# mise.toml) reads the rendered documents. Runs as part of `mise run
# helm-lint`. Every assertion runs; the exit status is the failure count.
set -eu

chart=charts/patchy
fixtures=hack/testdata/chart-render
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
failures=0

fail() {
  echo "chart-render-test: FAIL: $*" >&2
  failures=$((failures + 1))
}

# render NAME [helm args...]: render into $out/NAME.yaml; a failed render is
# itself a failure (and leaves an empty file, so later assertions fail too).
render() {
  name=$1
  shift
  if ! helm template patchy "$chart" --namespace patchy "$@" >"$out/$name.yaml" 2>"$out/$name.err"; then
    fail "$name: render failed: $(cat "$out/$name.err")"
    : >"$out/$name.yaml"
  fi
}

# get NAME EXPR: evaluate a yq expression against every document of a render.
get() {
  yq eval "$2" "$out/$1.yaml" | grep -v '^---$' || true
}

# expect NAME EXPR WANT: the expression yields exactly WANT ("null" for an
# absent key, "" for no matching document).
expect() {
  got=$(get "$1" "$2")
  if [ "$got" != "$3" ]; then
    fail "$1: $2 = '$got', want '$3'"
  fi
}

# cm NAME CONFIGMAP KEY WANT: one ConfigMap data key.
cm() {
  expect "$1" "select(.kind == \"ConfigMap\" and .metadata.name == \"patchy-$2-config\") | .data.$3" "$4"
}

# expect_fail DESC NEEDLE [helm args...]: the render fails and its error
# contains NEEDLE verbatim.
expect_fail() {
  desc=$1
  needle=$2
  shift 2
  if helm template patchy "$chart" --namespace patchy "$@" >/dev/null 2>"$out/guard.err"; then
    fail "guard $desc: render succeeded, want a failure containing: $needle"
  elif ! grep -qF -- "$needle" "$out/guard.err"; then
    fail "guard $desc: error lacks '$needle': $(cat "$out/guard.err")"
  fi
}

# ---- feature off: the default render carries none of it --------------------
render default
for key in PATCHY_REPOSITORY_IMAGES PATCHY_REPOSITORY_IMAGE_REGISTRIES PATCHY_REPOSITORY_IMAGE_ON_REJECT \
  PATCHY_REPOSITORY_IMAGE_COSIGN_KEY_FILE PATCHY_AGENT_EPHEMERAL_STORAGE PATCHY_CHANGESET_MAX_ENTRIES DOCKER_CONFIG; do
  expect default "select(.kind == \"ConfigMap\") | .data.$key | select(. != null)" ""
done
expect default 'select(.metadata.name == "patchy-repository-image-key") | .kind' ""
expect default 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-agent") | .imagePullSecrets' "null"
expect default 'select(.kind == "Secret") | .metadata.name' ""
expect default 'select(.kind == "Deployment") | .spec.template.spec.volumes[].name | select(. == "registry" or . == "repository-image-key")' ""
expect default 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-source-controller") | .spec.egress[].to[].ipBlock.cidr | select(. != null)' ""
render default-cilium --set agent.networkPolicy.mode=cilium
expect default-cilium 'select(.metadata.name == "patchy-source-controller-cloud-credentials") | .kind' ""

# ---- feature on: keys on the right controllers ------------------------------
render on -f "$fixtures/repository-images.yaml"
cm on source-controller PATCHY_REPOSITORY_IMAGES true
cm on source-controller PATCHY_REPOSITORY_IMAGE_REGISTRIES \
  123456789012.dkr.ecr.us-east-1.amazonaws.com/patchy/,ghcr.io/example/agent-images/
cm on source-controller PATCHY_REPOSITORY_IMAGE_MAX_BYTES 4294967296
cm on source-controller PATCHY_REPOSITORY_IMAGE_ON_REJECT default
cm on source-controller PATCHY_REPOSITORY_IMAGE_ALLOW_UNSIGNED false
cm on source-controller PATCHY_REPOSITORY_IMAGE_COSIGN_KEY_FILE /etc/patchy/repository-image/cosign.pub
cm on source-controller DOCKER_CONFIG /etc/patchy/registry
cm on source-controller PATCHY_AGENT_EPHEMERAL_STORAGE null
for c in investigation-controller remediation-controller; do
  cm on "$c" PATCHY_REPOSITORY_IMAGES true
  cm on "$c" PATCHY_AGENT_EPHEMERAL_STORAGE 8Gi
  cm on "$c" PATCHY_REPOSITORY_IMAGE_REGISTRIES null
  cm on "$c" DOCKER_CONFIG null
done
cm on remediation-controller PATCHY_CHANGESET_MAX_ENTRIES 500
cm on investigation-controller PATCHY_CHANGESET_MAX_ENTRIES null
for c in integration-controller context-controller; do
  cm on "$c" PATCHY_REPOSITORY_IMAGES null
done

# ---- feature on: source-controller's mounts, and only its -------------------
src='select(.kind == "Deployment" and .metadata.name == "patchy-source-controller") | .spec.template.spec'
expect on "$src | .containers[0].volumeMounts[] | select(.name == \"repository-image-key\") | .mountPath + \" \" + (.readOnly | tostring)" \
  "/etc/patchy/repository-image true"
expect on "$src | .containers[0].volumeMounts[] | select(.name == \"registry\") | .mountPath + \" \" + (.readOnly | tostring)" \
  "/etc/patchy/registry true"
expect on "$src | .volumes[] | select(.name == \"repository-image-key\") | .configMap.name" patchy-repository-image-key
expect on "$src | .volumes[] | select(.name == \"registry\") | .secret.secretName" patchy-registry
expect on "$src | .volumes[] | select(.name == \"registry\") | .secret.items[] | .key + \" -> \" + .path" \
  ".dockerconfigjson -> config.json"
expect on 'select(.kind == "Deployment" and .metadata.name == "patchy-source-controller") | .spec.template.metadata.annotations["checksum/repository-image-key"] | length' 64
expect on 'select(.kind == "Deployment" and .metadata.name != "patchy-source-controller") | .spec.template.spec.volumes[].name | select(. == "registry" or . == "repository-image-key")' ""
expect on 'select(.kind == "ConfigMap" and .metadata.name == "patchy-repository-image-key") | .data["cosign.pub"]' \
  "$(yq eval '.agent.repositoryImages.cosignPublicKey' "$fixtures/repository-images.yaml")"

# ---- feature on: the agent namespace's pull credential ----------------------
expect on 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-agent") | .imagePullSecrets[].name' patchy-registry
expect on 'select(.kind == "Secret" and .metadata.name == "patchy-registry") | .metadata.namespace + " " + .type' \
  "patchy-agents kubernetes.io/dockerconfigjson"
expect on 'select(.kind == "Secret" and .metadata.name == "patchy-registry") | .data[".dockerconfigjson"] | @base64d' \
  '{"auths":{"ghcr.io":{"auth":"cGxhY2Vob2xkZXI6cGxhY2Vob2xkZXI="}}}'
render on-no-data -f "$fixtures/repository-images.yaml" --set agent.repositoryImages.pullSecretData=
expect on-no-data 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-agent") | .imagePullSecrets[].name' patchy-registry
expect on-no-data 'select(.kind == "Secret") | .metadata.name' ""
render on-no-secret -f "$fixtures/repository-images.yaml" \
  --set agent.repositoryImages.pullSecretData= --set agent.repositoryImages.pullSecret=
expect on-no-secret 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-agent") | .imagePullSecrets' null
cm on-no-secret source-controller DOCKER_CONFIG null
expect on-no-secret "$src | .volumes[] | select(.name == \"registry\") | .name" ""

# ---- feature on: the EKS Pod Identity agent for source-controller -----------
srcnp='select(.kind == "NetworkPolicy" and .metadata.name == "patchy-source-controller") | .spec.egress[] | select(.to[].ipBlock.cidr == "169.254.170.23/32")'
expect on "$srcnp | .to[].ipBlock.cidr" "169.254.170.23/32
fd00:ec2::23/128"
expect on "$srcnp | .ports[] | .protocol + \"/\" + (.port | tostring)" "TCP/80"
render on-cilium -f "$fixtures/repository-images.yaml" --set agent.networkPolicy.mode=cilium --set agent.networkPolicy.broadEgress=auto
expect on-cilium 'select(.kind == "CiliumNetworkPolicy" and .metadata.name == "patchy-source-controller-cloud-credentials") | .spec.egress[0].toEntities[0] + " " + .spec.egress[0].toPorts[0].ports[0].port' \
  "host 80"
expect on-cilium 'select(.kind == "CiliumNetworkPolicy" and .metadata.name == "patchy-source-controller-cloud-credentials") | .spec.endpointSelector.matchLabels["app.kubernetes.io/name"]' \
  source-controller

# ---- feature on: the other values reach their keys --------------------------
render on-tuned -f "$fixtures/repository-images.yaml" \
  --set agent.repositoryImages.onReject=handoff --set agent.repositoryImages.maxBytes=1073741824 \
  --set agent.repositoryImages.changesetMaxEntries=2000 --set agent.repositoryImages.ephemeralStorage=20Gi \
  --set agent.repositoryImages.cosignPublicKey= --set agent.repositoryImages.allowUnsigned=true
cm on-tuned source-controller PATCHY_REPOSITORY_IMAGE_ON_REJECT handoff
cm on-tuned source-controller PATCHY_REPOSITORY_IMAGE_MAX_BYTES 1073741824
cm on-tuned source-controller PATCHY_REPOSITORY_IMAGE_ALLOW_UNSIGNED true
cm on-tuned source-controller PATCHY_REPOSITORY_IMAGE_COSIGN_KEY_FILE null
cm on-tuned remediation-controller PATCHY_CHANGESET_MAX_ENTRIES 2000
cm on-tuned investigation-controller PATCHY_AGENT_EPHEMERAL_STORAGE 20Gi
expect on-tuned 'select(.metadata.name == "patchy-repository-image-key") | .kind' ""
expect on-tuned "$src | .volumes[] | select(.name == \"repository-image-key\") | .name" ""
render on-extra -f "$fixtures/repository-images.yaml" \
  --set sourceController.config.extra.PATCHY_REPOSITORY_IMAGE_ON_REJECT=handoff
cm on-extra source-controller PATCHY_REPOSITORY_IMAGE_ON_REJECT handoff

# ---- the render-time guards: each fails, each passes once fixed -------------
f=$fixtures/repository-images.yaml
expect_fail "registries empty" "agent.repositoryImages.enabled requires agent.repositoryImages.registries" \
  -f "$f" --set-json 'agent.repositoryImages.registries=[]'
expect_fail "ephemeralStorage empty" "agent.repositoryImages.enabled requires agent.repositoryImages.ephemeralStorage" \
  -f "$f" --set agent.repositoryImages.ephemeralStorage=
expect_fail "no cosign key" "set agent.repositoryImages.allowUnsigned: true to admit unsigned images instead" \
  -f "$f" --set agent.repositoryImages.cosignPublicKey=
render guard-unsigned-fixed -f "$f" --set agent.repositoryImages.cosignPublicKey= --set agent.repositoryImages.allowUnsigned=true
expect_fail "pullSecretData without pullSecret" "agent.repositoryImages.pullSecretData requires agent.repositoryImages.pullSecret" \
  -f "$f" --set agent.repositoryImages.pullSecret=
expect_fail "no agent NetworkPolicy" "agent.networkPolicy.create is false" \
  -f "$f" --set agent.networkPolicy.create=false
expect_fail "broad egress under none" \
  'agent.networkPolicy.broadEgress ("auto") resolves to broad under agent.networkPolicy.mode "none"' \
  -f "$f" --set agent.networkPolicy.broadEgress=auto
expect_fail "broad egress names the fix" "Set agent.networkPolicy.broadEgress: never (brokered claude runners only)" \
  -f "$f" --set agent.networkPolicy.broadEgress=auto
expect_fail "broad egress under istio" \
  'agent.networkPolicy.broadEgress ("auto") resolves to broad under agent.networkPolicy.mode "istio"' \
  -f "$f" --set agent.networkPolicy.mode=istio --set agent.networkPolicy.broadEgress=auto
expect_fail "broad egress always" \
  'agent.networkPolicy.broadEgress ("always") resolves to broad under agent.networkPolicy.mode "cilium"' \
  -f "$f" --set agent.networkPolicy.mode=cilium --set agent.networkPolicy.broadEgress=always
render guard-egress-fixed-gke -f "$f" --set agent.networkPolicy.mode=gke --set agent.networkPolicy.broadEgress=auto
expect guard-egress-fixed-gke 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-agents-egress") | .spec.egress[].ports[].port | select(. == 443)' ""
render guard-kill-switch -f "$f" --set agent.repositoryImages.enabled=false --set agent.networkPolicy.broadEgress=auto \
  --set-json 'agent.repositoryImages.registries=[]' --set agent.repositoryImages.pullSecret=
cm guard-kill-switch source-controller PATCHY_REPOSITORY_IMAGES null

if [ "$failures" -gt 0 ]; then
  echo "chart-render-test: $failures assertion(s) failed" >&2
  exit 1
fi
echo "chart-render-test: all assertions passed"
