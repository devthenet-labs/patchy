#!/bin/sh
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Render assertions for charts/patchy: what hack/helm-lint.sh's plain renders
# cannot see. Which PATCHY_* keys land in which controller's ConfigMap, which
# Deployment mounts what, that each render-time guard fails with its message
# and passes once its value is fixed, and that the feature-off render carries
# none of it. A final section does the same for charts/patchy-config's
# Project CRs and their values schema. Fixtures live in
# hack/testdata/chart-render/; yq (pinned in
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

# notes NAME [helm args...]: render the install NOTES into $out/NAME.notes.
# helm template never renders NOTES.txt; a client-side dry-run install does,
# without a cluster. A failed render is itself a failure.
notes() {
  name=$1
  shift
  if helm install patchy "$chart" --namespace patchy --dry-run=client "$@" >"$out/$name.install" 2>"$out/$name.err"; then
    sed -n '/^NOTES:$/,$p' "$out/$name.install" >"$out/$name.notes"
  else
    fail "$name: dry-run install failed: $(cat "$out/$name.err")"
    : >"$out/$name.notes"
  fi
}

# notes_has NAME NEEDLE WANT: whether the NOTES of a notes render contain
# NEEDLE verbatim is WANT (yes or no).
notes_has() {
  got=no
  if grep -qF -- "$2" "$out/$1.notes"; then
    got=yes
  fi
  if [ "$got" != "$3" ]; then
    fail "$1: NOTES contain '$2': $got, want $3"
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
for key in PATCHY_REQUESTS_PER_POD PATCHY_CONCURRENT_PER_POD PATCHY_CONCURRENCY_WAIT PATCHY_TOKENS_PER_POD \
  PATCHY_TOKENS_PER_HOUR PATCHY_MAX_TOKENS_CEILING PATCHY_MODEL_ALLOWLIST PATCHY_BETA_DENYLIST \
  PATCHY_MAX_ANTHROPIC_REQUEST_BYTES PATCHY_MAX_REQUEST_BYTES PATCHY_PREAUTH_REQUESTS_PER_SECOND \
  PATCHY_PREAUTH_BURST PATCHY_TOKEN_REVIEWS_PER_SECOND; do
  cm default egress-broker "$key" null
done
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
cm on integration-controller PATCHY_REPOSITORY_IMAGES true
cm on context-controller PATCHY_REPOSITORY_IMAGES null
# the runner-image comment reads Repositories: integration-controller's Role
# grants get/list/watch on them (read-only, as in the kustomize base)
for r in default on; do
  expect "$r" 'select(.kind == "Role" and .metadata.name == "patchy-integration-controller") | .rules[] | select((.resources | length) == 1 and .resources[0] == "repositories") | .verbs | join(",")' "get,list,watch"
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
# Optional: a missing Secret, or one without the .dockerconfigjson key, must
# not hold source-controller (and with it the artifact server) in
# ContainerCreating; resolution degrades to anonymous and rejects per image.
expect on "$src | .volumes[] | select(.name == \"registry\") | .secret.optional" true
expect on 'select(.kind == "Deployment" and .metadata.name == "patchy-source-controller") | .spec.template.metadata.annotations["checksum/repository-image-key"] | length' 64
expect on 'select(.kind == "Deployment" and .metadata.name != "patchy-source-controller") | .spec.template.spec.volumes[].name | select(. == "registry" or . == "repository-image-key")' ""
expect on 'select(.kind == "ConfigMap" and .metadata.name == "patchy-repository-image-key") | .data["cosign.pub"]' \
  "$(yq eval '.agent.repositoryImages.cosignPublicKey' "$fixtures/repository-images.yaml")"

# ---- feature on: the agent namespace's pull credential ----------------------
expect on 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-agent") | .imagePullSecrets[].name' patchy-registry
# pullSecretData renders the Secret into both namespaces: agent.namespace for
# the kubelet, the release namespace for source-controller's mount.
expect on 'select(.kind == "Secret" and .metadata.name == "patchy-registry") | .metadata.namespace + " " + .type' \
  "patchy kubernetes.io/dockerconfigjson
patchy-agents kubernetes.io/dockerconfigjson"
expect on 'select(.kind == "Secret" and .metadata.name == "patchy-registry") | .data[".dockerconfigjson"] | @base64d' \
  '{"auths":{"ghcr.io":{"auth":"cGxhY2Vob2xkZXI6cGxhY2Vob2xkZXI="}}}
{"auths":{"ghcr.io":{"auth":"cGxhY2Vob2xkZXI6cGxhY2Vob2xkZXI="}}}'
expect on 'select(.kind == "Secret" and .metadata.namespace == "patchy") | .metadata.labels["app.kubernetes.io/name"]' \
  source-controller
render on-one-namespace -f "$fixtures/repository-images.yaml" --set agent.namespace=patchy
expect on-one-namespace 'select(.kind == "Secret" and .metadata.name == "patchy-registry") | .metadata.namespace' patchy
render on-no-data -f "$fixtures/repository-images.yaml" --set agent.repositoryImages.pullSecretData=
expect on-no-data 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-agent") | .imagePullSecrets[].name' patchy-registry
expect on-no-data 'select(.kind == "Secret") | .metadata.name' ""
expect on-no-data "$src | .volumes[] | select(.name == \"registry\") | .secret.optional" true
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
  --set-json 'agent.repositoryImages.registries=[]' --set agent.repositoryImages.pullSecret= \
  --set agent.repositoryImages.cosignPublicKey=notapem
cm guard-kill-switch source-controller PATCHY_REPOSITORY_IMAGES null

# ---- malformed values fail the render, not the controller -------------------
# Each value below renders a ConfigMap source-controller or a job controller
# refuses at startup (runnerimage.NormalizeEntry, resource.ParseQuantity,
# resolve.ParsePublicKey); under strategy Recreate that is a crash-loop in
# place of the running pod. The schema patterns carry the first two, the
# guard the key's PEM armour.
for entry in ghcr.io ghcr.io/ docker.io/org/* 'ghcr.io/org?/' 'ghcr.io/[ab]/' ghcr.io//x/ ghcr.io/org/app:1/ \
  ghcr.io/org/app@sha256:abc 'ghcr.io/org /' ' ' ghcr.io/org/,ghcr.io; do
  expect_fail "registries entry '$entry'" "agent/repositoryImages/registries/0" \
    -f "$f" --set-json "agent.repositoryImages.registries=[\"$entry\"]"
done
for q in 8GB 8gi '8 Gi' lots; do
  expect_fail "ephemeralStorage '$q'" "agent/repositoryImages/ephemeralStorage" \
    -f "$f" --set-json "agent.repositoryImages.ephemeralStorage=\"$q\""
done
# Stricter than ParseQuantity on purpose: a sign or an exponent parses, but a
# negative size fails every agent Job's creation and nobody sizes a disk as 8e9.
for q in -8Gi +8Gi 8e9; do
  expect_fail "ephemeralStorage '$q'" "agent/repositoryImages/ephemeralStorage" \
    -f "$f" --set-json "agent.repositoryImages.ephemeralStorage=\"$q\""
done
expect_fail "cosign key without PEM armour" "agent.repositoryImages.cosignPublicKey is not a PEM public key" \
  -f "$f" --set agent.repositoryImages.cosignPublicKey=notapem
expect_fail "cosign key alongside allowUnsigned" "agent.repositoryImages.cosignPublicKey is not a PEM public key" \
  -f "$f" --set agent.repositoryImages.cosignPublicKey=notapem --set agent.repositoryImages.allowUnsigned=true
render guard-formats-fixed -f "$f" \
  --set-json 'agent.repositoryImages.registries=["localhost:5000/team","Index.Docker.IO/library/","us-docker.pkg.dev/my-project/agent_images.v2/"]' \
  --set agent.repositoryImages.ephemeralStorage=1.5Gi
cm guard-formats-fixed source-controller PATCHY_REPOSITORY_IMAGE_REGISTRIES \
  localhost:5000/team,Index.Docker.IO/library/,us-docker.pkg.dev/my-project/agent_images.v2/
cm guard-formats-fixed investigation-controller PATCHY_AGENT_EPHEMERAL_STORAGE 1.5Gi

# ---- evaluation controller: off by default, and none of its :9791 plumbing --
# evaluationController.enabled gates its own file AND source-controller's
# internal blob endpoint across four other templates; a half-rendered flag is
# a listener nobody can reach or a client dialling a port nobody opened.
evsrc='select(.kind == "Deployment" and .metadata.name == "patchy-source-controller") | .spec.template.spec'
evsvc='select(.kind == "Service" and .metadata.name == "patchy-source-controller") | .spec.ports[]'
evsrcnp='select(.kind == "NetworkPolicy" and .metadata.name == "patchy-source-controller") | .spec.ingress[]'
ev='select(.kind == "Deployment" and .metadata.name == "patchy-evaluation-controller") | .spec.template'
expect default 'select(.metadata.labels["app.kubernetes.io/name"] == "evaluation-controller") | .kind' ""
cm default source-controller PATCHY_ARTIFACT_INTERNAL_ADDR null
cm default source-controller PATCHY_WORKSPACE_RETENTION null
expect default "$evsrc | .containers[0].ports[] | select(.containerPort == 9791) | .name" ""
expect default "$evsvc | select(.port == 9791) | .name" ""
expect default "$evsrcnp | select(.ports[].port == 9791) | .ports[].port" ""

# ---- evaluation controller on: the Deployment, its identity and its grants --
render eval -f "$fixtures/evaluation-controller.yaml"
expect eval "$ev | .spec.containers[0].image | split(\":\") | .[0]" ghcr.io/devthenet-labs/patchy/evaluation-controller
expect eval "$ev | .spec.serviceAccountName" patchy-evaluation-controller
expect eval 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-evaluation-controller") | .metadata.namespace' patchy
# Every binding names that ServiceAccount, and every Role it references is rendered.
expect eval 'select((.kind == "RoleBinding" or .kind == "ClusterRoleBinding") and .metadata.labels["app.kubernetes.io/name"] == "evaluation-controller") | .roleRef.kind + "/" + .roleRef.name + " <- " + (.subjects[] | .kind + " " + .namespace + "/" + .name)' \
  "ClusterRole/patchy-evaluation-controller-authz <- ServiceAccount patchy/patchy-evaluation-controller
Role/patchy-evaluation-controller <- ServiceAccount patchy/patchy-evaluation-controller
Role/patchy-evaluation-controller-jobs <- ServiceAccount patchy/patchy-evaluation-controller"
expect eval 'select((.kind == "Role" or .kind == "ClusterRole") and .metadata.labels["app.kubernetes.io/name"] == "evaluation-controller") | .kind + "/" + .metadata.name + " " + (.metadata.namespace // "-")' \
  "ClusterRole/patchy-evaluation-controller-authz -
Role/patchy-evaluation-controller patchy
Role/patchy-evaluation-controller-jobs patchy-agents"
evrole='select(.kind == "Role" and .metadata.name == "patchy-evaluation-controller") | .rules[]'
expect eval "$evrole | select(.resources[0] == \"evaluations\") | .verbs | join(\",\")" "create,get,list,watch,delete"
expect eval "$evrole | select(.resources[0] == \"evaluationunits\") | .verbs | join(\",\")" "create,get,list,watch,update,patch"
expect eval "$evrole | select(.resources[0] == \"configmaps\") | .verbs | join(\",\")" "create,get,update"
expect eval 'select(.kind == "Role" and .metadata.name == "patchy-evaluation-controller-jobs") | .rules[] | select(.resources[0] == "jobs") | .verbs | join(",")' \
  "create,get,list,watch,delete"
expect eval 'select(.kind == "ClusterRole" and .metadata.name == "patchy-evaluation-controller-authz") | .rules[] | .resources[0] + " " + (.verbs | join(","))' \
  "subjectaccessreviews create"
expect eval 'select(.kind == "ClusterRole" and .metadata.name == "patchy-evaluations-submitter") | .kind' ""

# ---- evaluation controller on: config and the auth mount agree ---------------
expect eval "$ev | .spec.containers[0].envFrom[0].configMapRef.name" patchy-evaluation-controller-config
cm eval evaluation-controller PATCHY_HARNESSES claude
cm eval evaluation-controller PATCHY_EVOLVE_CLAUDE_IMAGE ghcr.io/bitwise-media-group/evolve-runner-claude:latest
cm eval evaluation-controller PATCHY_BROKER_URL http://patchy-egress-broker.patchy.svc.cluster.local:8080
cm eval evaluation-controller PATCHY_ARTIFACT_UPLOAD_URL http://patchy-source-controller.patchy.svc.cluster.local:9791
cm eval evaluation-controller PATCHY_ARTIFACT_BASE_URL http://patchy-source-controller.patchy.svc.cluster.local:9790
cm eval evaluation-controller PATCHY_MAX_WORKSPACE_BYTES 67108864
cm eval evaluation-controller PATCHY_AUTH_CONFIG /etc/patchy/auth/config.yaml
expect eval "$ev | .spec.containers[0].volumeMounts[] | select(.name == \"auth\") | .mountPath + \" \" + (.readOnly | tostring)" \
  "/etc/patchy/auth true"
expect eval "$ev | .spec.volumes[] | select(.name == \"auth\") | .secret.secretName" patchy-evaluation-auth
expect eval 'select(.kind == "Secret" and .metadata.name == "patchy-evaluation-auth") | .stringData["config.yaml"] | from_yaml | .mode + " " + .oidc.clientID' \
  "oidc evolve"
expect eval "$ev | .metadata.annotations[\"checksum/auth\"] | length" 64
expect eval 'select(.kind == "Service" and .metadata.name == "patchy-evaluation-controller") | .spec.ports[] | .name + " " + (.port | tostring) + " -> " + .targetPort' \
  "http 8080 -> http"
expect eval "$ev | .spec.containers[0].ports[] | select(.name == \"http\") | .containerPort" 8080

# ---- evaluation controller on: source-controller's :9791, end to end ----------
cm eval source-controller PATCHY_ARTIFACT_INTERNAL_ADDR :9791
cm eval source-controller PATCHY_WORKSPACE_RETENTION 168h
expect eval "$evsrc | .containers[0].ports[] | select(.containerPort == 9791) | .name" internal
expect eval "$evsvc | select(.port == 9791) | .name + \" -> \" + .targetPort" "internal -> internal"
expect eval "$evsrcnp | select(.ports[].port == 9791) | .from[].podSelector.matchLabels[\"app.kubernetes.io/name\"]" \
  evaluation-controller
expect eval 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-evaluation-controller") | .spec.egress[] | select(.ports[].port == 9791) | .to[].podSelector.matchLabels["app.kubernetes.io/name"]' \
  source-controller
# the flag changes source-controller's config, so an upgrade that turns the
# evaluation controller on rolls source-controller and opens the listener
evsum='select(.kind == "Deployment" and .metadata.name == "patchy-source-controller") | .spec.template.metadata.annotations["checksum/config"]'
if [ -z "$(get eval "$evsum")" ] || [ "$(get default "$evsum")" = "$(get eval "$evsum")" ]; then
  fail "eval: source-controller's checksum/config did not change, so an upgrade would not open :9791"
fi

# ---- evaluation controller variants ------------------------------------------
# An operator-owned auth Secret: mounted by name, neither rendered nor hashed.
render eval-existing -f "$fixtures/evaluation-controller.yaml" \
  --set evaluationController.auth.config=null --set evaluationController.auth.existingSecret=evals-auth
expect eval-existing 'select(.kind == "Secret") | .metadata.name' ""
expect eval-existing "$ev | .spec.volumes[] | select(.name == \"auth\") | .secret.secretName" evals-auth
expect eval-existing "$ev | .metadata.annotations[\"checksum/auth\"]" null
# A bring-your-own ServiceAccount: not rendered, but still what runs and binds.
render eval-own-sa -f "$fixtures/evaluation-controller.yaml" \
  --set evaluationController.serviceAccount.create=false --set evaluationController.serviceAccount.name=evals
expect eval-own-sa 'select(.kind == "ServiceAccount" and .metadata.labels["app.kubernetes.io/name"] == "evaluation-controller") | .metadata.name' ""
expect eval-own-sa "$ev | .spec.serviceAccountName" evals
expect eval-own-sa 'select(.kind == "RoleBinding" and .metadata.labels["app.kubernetes.io/name"] == "evaluation-controller") | .subjects[].name' \
  "evals
evals"
# claude enabled only on the evaluation fleet still deploys the broker.
render eval-broker -f "$fixtures/evaluation-controller.yaml" \
  --set agent.runners.claude.enabled=false --set agent.runners.codex.enabled=true
expect eval-broker 'select(.kind == "Deployment" and .metadata.name == "patchy-egress-broker") | .kind' Deployment
cm eval-broker evaluation-controller PATCHY_BROKER_URL http://patchy-egress-broker.patchy.svc.cluster.local:8080
# A non-brokered evaluation runner reuses agent.runners.<harness>'s Secret.
render eval-codex -f "$fixtures/evaluation-controller.yaml" \
  --set evaluationController.runners.claude.enabled=false --set evaluationController.runners.codex.enabled=true
cm eval-codex evaluation-controller PATCHY_HARNESSES codex
cm eval-codex evaluation-controller PATCHY_CODEX_SECRET patchy-openai
cm eval-codex evaluation-controller PATCHY_CODEX_SECRET_ENV OPENAI_API_KEY
cm eval-codex evaluation-controller PATCHY_BROKER_URL null
# Both exposure flavours and the example submitter tier.
render eval-exposed -f "$fixtures/evaluation-controller.yaml" \
  --set evaluationController.host=patchy-evals.example.com --set evaluationController.ingress.enabled=true \
  --set evaluationController.httpRoute.enabled=true --set evaluationController.rbac.userRoles=true
expect eval-exposed 'select(.kind == "Ingress" and .metadata.name == "patchy-evaluation-controller") | .spec.rules[0].host + " " + .spec.rules[0].http.paths[0].backend.service.name' \
  "patchy-evals.example.com patchy-evaluation-controller"
expect eval-exposed 'select(.kind == "HTTPRoute" and .metadata.name == "patchy-evaluation-controller") | .spec.hostnames[0] + " " + .spec.rules[0].backendRefs[0].name' \
  "patchy-evals.example.com patchy-evaluation-controller"
expect eval-exposed 'select(.kind == "ClusterRole" and .metadata.name == "patchy-evaluations-submitter") | .rules[0].verbs | join(",")' \
  "create,get,delete"
# Without its NetworkPolicy the component still renders; the :9791 ingress
# on source-controller is keyed on the component, not on this flag.
render eval-no-np -f "$fixtures/evaluation-controller.yaml" --set evaluationController.networkPolicy.create=false
expect eval-no-np 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-evaluation-controller") | .kind' ""
expect eval-no-np "$ev | .spec.serviceAccountName" patchy-evaluation-controller
expect eval-no-np "$evsrcnp | select(.ports[].port == 9791) | .from[].podSelector.matchLabels[\"app.kubernetes.io/name\"]" \
  evaluation-controller

# ---- evaluation controller guards --------------------------------------------
ef=$fixtures/evaluation-controller.yaml
expect_fail "eval without auth" "evaluationController requires auth configuration" \
  --set evaluationController.enabled=true
expect_fail "eval with both auth sources" \
  "evaluationController.auth.existingSecret and evaluationController.auth.config are mutually exclusive" \
  -f "$ef" --set evaluationController.auth.existingSecret=evals-auth
expect_fail "eval without a runner" "evaluationController.enabled requires at least one evaluationController.runners" \
  -f "$ef" --set evaluationController.runners.claude.enabled=false
expect_fail "eval ingress without host" "evaluationController.host is required when evaluationController.ingress is enabled" \
  -f "$ef" --set evaluationController.ingress.enabled=true
expect_fail "eval httpRoute without host" "evaluationController.host is required when evaluationController.httpRoute is enabled" \
  -f "$ef" --set evaluationController.httpRoute.enabled=true

# ---- a harness enabled only for evaluations keeps its egress policy ----------
# Evaluation Jobs carry the same harness label as finding Jobs, so a harness
# enabled on either fleet needs its per-harness policy: without one a Cilium
# or GKE pod is default-denied its model API, and an Istio pod has no
# REGISTRY_ONLY Sidecar beside the base policy's TCP 443 to anywhere.
hnp='select(.kind == "CiliumNetworkPolicy" or .kind == "FQDNNetworkPolicy" or .kind == "Sidecar" or .kind == "ServiceEntry") | .kind + "/" + .metadata.name'
render eval-codex-cilium -f "$ef" --set agent.networkPolicy.mode=cilium --set evaluationController.runners.codex.enabled=true
expect eval-codex-cilium "$hnp" "CiliumNetworkPolicy/patchy-agent-egress-claude
CiliumNetworkPolicy/patchy-agent-egress-codex"
expect eval-codex-cilium 'select(.kind == "CiliumNetworkPolicy" and .metadata.name == "patchy-agent-egress-codex") | .spec.endpointSelector.matchLabels["patchy.bitwisemedia.uk/harness"]' codex
render eval-codex-gke -f "$ef" --set agent.networkPolicy.mode=gke --set evaluationController.runners.codex.enabled=true
expect eval-codex-gke "$hnp" "FQDNNetworkPolicy/patchy-agent-egress-codex"
render eval-claude-istio -f "$ef" --set agent.networkPolicy.mode=istio \
  --set agent.runners.claude.enabled=false --set agent.runners.codex.enabled=true
expect eval-claude-istio "$hnp" "ServiceEntry/patchy-agent-codex
Sidecar/patchy-agent-egress-claude
Sidecar/patchy-agent-egress-codex"
# ...and a harness enabled nowhere gets none: the evaluation fleet's runner
# flags mean nothing while the controller itself is off.
render eval-off-cilium --set agent.networkPolicy.mode=cilium --set evaluationController.runners.codex.enabled=true
expect eval-off-cilium "$hnp" "CiliumNetworkPolicy/patchy-agent-egress-claude"

# ---- ...and the install NOTES name its model Secret -------------------------
# A non-brokered harness is enabled only when its credential Secret exists in
# the agent namespace, whichever fleet runs it, so the NOTES' list of Secrets
# to create follows the same rule as the egress policies above.
codexsecret="patchy-openai (key api-key, as OPENAI_API_KEY)"
notes notes-eval-codex -f "$ef" \
  --set evaluationController.runners.claude.enabled=false --set evaluationController.runners.codex.enabled=true
notes_has notes-eval-codex "$codexsecret" yes
notes_has notes-eval-codex "patchy-copilot" no
notes notes-codex --set agent.runners.codex.enabled=true
notes_has notes-codex "$codexsecret" yes
notes notes-eval-off --set evaluationController.runners.codex.enabled=true
notes_has notes-eval-off "patchy-openai" no

# ---- intent controller: off by default --------------------------------------
it='select(.kind == "Deployment" and .metadata.name == "patchy-intent-controller") | .spec.template'
icm='select(.kind == "ConfigMap" and .metadata.name == "patchy-intent-controller-config") | .data'
expect default 'select(.metadata.labels["app.kubernetes.io/name"] == "intent-controller") | .kind' ""
expect default "select(.kind == \"ConfigMap\") | (.data // {}) | keys | .[] | select(test(\"^PATCHY_INTENT_\"))" ""

# ---- intent controller on: the Deployment, its identity and its grants ------
ifx=$fixtures/intent-controller.yaml
render intent -f "$ifx"
expect intent "$it | .spec.containers[0].image | split(\":\") | .[0]" ghcr.io/devthenet-labs/patchy/intent-controller
expect intent "$it | .spec.serviceAccountName" patchy-intent-controller
expect intent "$it | .spec.containers[0].envFrom[] | .configMapRef.name" patchy-intent-controller-config
expect intent "$it | .spec.containers[0].ports[] | .name + \" \" + (.containerPort | tostring)" "health 8081"
expect intent "$it | .metadata.labels[\"app.kubernetes.io/component\"]" controller
expect intent 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-intent-controller") | .metadata.namespace' patchy
# Every binding names that ServiceAccount, every Role it references is
# rendered, and there is no ClusterRole: the design's tightest posture.
expect intent 'select((.kind == "RoleBinding" or .kind == "ClusterRoleBinding") and .metadata.labels["app.kubernetes.io/name"] == "intent-controller") | .roleRef.kind + "/" + .roleRef.name + " <- " + (.subjects[] | .kind + " " + .namespace + "/" + .name)' \
  "Role/patchy-intent-controller <- ServiceAccount patchy/patchy-intent-controller
Role/patchy-intent-controller-jobs <- ServiceAccount patchy/patchy-intent-controller"
expect intent 'select((.kind == "Role" or .kind == "ClusterRole") and .metadata.labels["app.kubernetes.io/name"] == "intent-controller") | .kind + "/" + .metadata.name + " " + (.metadata.namespace // "-")' \
  "Role/patchy-intent-controller patchy
Role/patchy-intent-controller-jobs patchy-agents"
# The release-namespace Role, rule by rule: exactly the verbs the engine uses.
irole='select(.kind == "Role" and .metadata.name == "patchy-intent-controller") | .rules[]'
expect intent "$irole | (.resources | join(\",\")) + \" \" + (.verbs | join(\",\"))" \
  "projects get,list,watch
projects/status update
intents create,get,list,watch,delete
intents/status,intents/finalizers update
intentruns create,get,list,watch,update
intentruns/status,intentruns/finalizers update
repositories create,get,list,watch,delete
forges get,list,watch
configmaps create,get,list,watch,update
secrets get
leases get,create,update
events create,patch"
# secrets get only on the named Forge Secrets; no other rule names secrets.
expect intent "$irole | select(.resources[0] == \"secrets\") | .resourceNames | join(\",\")" \
  "patchy-github,patchy-github-enterprise"
expect intent "$irole | select(.resourceNames == null) | .resources[] | select(. == \"secrets\")" ""
# The agents-namespace Role is a copy of the shared agent-jobs Role.
if [ -z "$(get intent 'select(.kind == "Role" and .metadata.name == "patchy-intent-controller-jobs") | .rules')" ] ||
  [ "$(get intent 'select(.kind == "Role" and .metadata.name == "patchy-intent-controller-jobs") | .rules')" != \
    "$(get intent 'select(.kind == "Role" and .metadata.name == "patchy-agent-jobs") | .rules')" ]; then
  fail "intent: patchy-intent-controller-jobs is not a copy of the agent-jobs Role"
fi
# NetworkPolicy: probes in; DNS and TCP 443/6443 out, like the job controllers.
inp='select(.kind == "NetworkPolicy" and .metadata.name == "patchy-intent-controller") | .spec'
expect intent "$inp | .ingress[].ports[] | .protocol + \"/\" + (.port | tostring)" "TCP/8081"
expect intent "$inp | .egress[].ports[] | .protocol + \"/\" + (.port | tostring)" "UDP/53
TCP/53
TCP/443
TCP/6443"

# ---- intent controller on: its ConfigMap holds only what it binds -----------
# Brokered claude only, whatever agent.runners enables for findings.
cm intent intent-controller PATCHY_HARNESSES claude
expect intent "$icm | .PATCHY_CLAUDE_AGENT_IMAGE | split(\":\") | .[0]" ghcr.io/devthenet-labs/patchy/claude-agent-runner
cm intent intent-controller PATCHY_BROKER_URL http://patchy-egress-broker.patchy.svc.cluster.local:8080
cm intent intent-controller PATCHY_CLAUDE_PROVIDER anthropic
cm intent intent-controller PATCHY_AGENT_NAMESPACE patchy-agents
cm intent intent-controller PATCHY_AGENT_SERVICE_ACCOUNT patchy-agent
cm intent intent-controller PATCHY_JOB_TTL 1h
# The finding job controllers' keys stay theirs: the intent Job deadline is
# its own, it runs no other harness, and it has no model allowlist.
for key in PATCHY_JOB_DEADLINE PATCHY_MODEL_ALLOWLIST PATCHY_CODEX_AGENT_IMAGE PATCHY_COPILOT_AGENT_IMAGE \
  PATCHY_INVESTIGATE_MODEL PATCHY_REMEDIATE_MODEL PATCHY_MAX_ATTEMPTS PATCHY_LISTEN_ADDR \
  PATCHY_REPOSITORY_IMAGES PATCHY_AGENT_EPHEMERAL_STORAGE PATCHY_CHANGESET_MAX_ENTRIES; do
  cm intent intent-controller "$key" null
done
# Its own keys equal the kustomize component's, which
# cmd/intent-controller/serve_test.go holds to the binary's flags and
# defaults — so every chart key is one the binary binds.
component=deploy/kustomize/components/intent-controller/configmap.yaml
ckeys=$(yq '.data | keys | .[] | select(test("^PATCHY_INTENT_"))' "$component" | sort)
expect intent "$icm | keys | .[] | select(test(\"^PATCHY_INTENT_\"))" "$ckeys"
for key in $ckeys; do
  cm intent intent-controller "$key" "$(yq ".data.$key" "$component")"
done
# The finding controllers are not touched by the flag...
for c in integration-controller source-controller context-controller investigation-controller remediation-controller; do
  csum="select(.kind == \"Deployment\" and .metadata.name == \"patchy-$c\") | .spec.template.metadata.annotations[\"checksum/config\"]"
  if [ "$(get default "$csum")" != "$(get intent "$csum")" ]; then
    fail "intent: enabling the intent controller changed $c's config"
  fi
done
# ...and a config change rolls the intent controller.
render intent-tuned -f "$ifx" --set intentController.config.plan.maxTurns=10 \
  --set intentController.config.intentTTL=0s --set intentController.config.rateLimitFloor=0 \
  --set intentController.config.logLevel=debug --set intentController.config.extra.PATCHY_INTENT_POLL_INTERVAL=2m
cm intent-tuned intent-controller PATCHY_INTENT_PLAN_MAX_TURNS 10
cm intent-tuned intent-controller PATCHY_INTENT_TTL 0s
cm intent-tuned intent-controller PATCHY_INTENT_RATE_LIMIT_FLOOR 0
cm intent-tuned intent-controller PATCHY_LOG_LEVEL debug
cm intent-tuned intent-controller PATCHY_INTENT_POLL_INTERVAL 2m
if [ "$(get intent "$it | .metadata.annotations[\"checksum/config\"]")" = \
  "$(get intent-tuned "$it | .metadata.annotations[\"checksum/config\"]")" ]; then
  fail "intent-tuned: checksum/config did not change, so an upgrade would not roll the controller"
fi

# ---- intent controller on: repository images reach it -----------------------
render intent-ri -f "$ifx" -f "$fixtures/repository-images.yaml"
cm intent-ri intent-controller PATCHY_REPOSITORY_IMAGES true
cm intent-ri intent-controller PATCHY_AGENT_EPHEMERAL_STORAGE 8Gi
cm intent-ri intent-controller PATCHY_CHANGESET_MAX_ENTRIES 500
cm intent-ri intent-controller PATCHY_REPOSITORY_IMAGE_REGISTRIES null
cm intent-ri intent-controller DOCKER_CONFIG null
# The global egress proxy reaches it too: it talks to GitHub.
render intent-proxy -f "$ifx" --set proxy.httpsProxy=http://proxy.example.com:3128
cm intent-proxy intent-controller HTTPS_PROXY http://proxy.example.com:3128
cm intent-proxy intent-controller NO_PROXY localhost,127.0.0.1,.svc,.cluster.local

# ---- intent controller on: claude everywhere it runs ------------------------
# Intents run on claude even when the finding fleet does not: the broker
# deploys, the agent egress admits it, and each egress dialect keeps a claude
# policy for the intent pods.
render intent-codex -f "$ifx" --set agent.runners.claude.enabled=false --set agent.runners.codex.enabled=true
expect intent-codex 'select(.kind == "Deployment" and .metadata.name == "patchy-egress-broker") | .kind' Deployment
expect intent-codex 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-agents-egress") | .spec.egress[].to[].podSelector.matchLabels["app.kubernetes.io/name"] | select(. == "egress-broker")' \
  egress-broker
cm intent-codex intent-controller PATCHY_HARNESSES claude
cm intent-codex investigation-controller PATCHY_HARNESSES codex
render intent-codex-cilium -f "$ifx" --set agent.networkPolicy.mode=cilium \
  --set agent.runners.claude.enabled=false --set agent.runners.codex.enabled=true
expect intent-codex-cilium "$hnp" "CiliumNetworkPolicy/patchy-agent-egress-claude
CiliumNetworkPolicy/patchy-agent-egress-codex"
render intent-codex-istio -f "$ifx" --set agent.networkPolicy.mode=istio \
  --set agent.runners.claude.enabled=false --set agent.runners.codex.enabled=true
expect intent-codex-istio "$hnp" "ServiceEntry/patchy-agent-codex
Sidecar/patchy-agent-egress-claude
Sidecar/patchy-agent-egress-codex"
# ...and with the controller off, claude disabled for findings is claude off.
render intent-off-codex --set agent.runners.claude.enabled=false --set agent.runners.codex.enabled=true
expect intent-off-codex 'select(.kind == "Deployment" and .metadata.name == "patchy-egress-broker") | .kind' ""

# ---- intent controller variants ----------------------------------------------
# A bring-your-own ServiceAccount: not rendered, but still what runs and binds.
render intent-own-sa -f "$ifx" --set intentController.serviceAccount.create=false \
  --set intentController.serviceAccount.name=intents
expect intent-own-sa 'select(.kind == "ServiceAccount" and .metadata.labels["app.kubernetes.io/name"] == "intent-controller") | .metadata.name' ""
expect intent-own-sa "$it | .spec.serviceAccountName" intents
expect intent-own-sa 'select(.kind == "RoleBinding" and .metadata.labels["app.kubernetes.io/name"] == "intent-controller") | .subjects[].name' \
  "intents
intents"
# Without its NetworkPolicy the component still renders.
render intent-no-np -f "$ifx" --set intentController.networkPolicy.create=false
expect intent-no-np "$inp | .podSelector" ""
expect intent-no-np "$it | .spec.serviceAccountName" patchy-intent-controller
render intent-extra-egress -f "$ifx" \
  --set-json 'intentController.networkPolicy.extraEgress=[{"ports":[{"protocol":"TCP","port":3128}]}]'
expect intent-extra-egress "$inp | .egress[].ports[] | select(.port == 3128) | .protocol" TCP

# ---- intent controller guards -------------------------------------------------
# An empty resourceNames list grants every Secret, so it must never render:
# the schema refuses it, and the template refuses it again when the schema
# is skipped.
expect_fail "intent without forge secrets" "missing property 'forgeSecrets'" \
  -f "$ifx" --set-json 'intentController.forgeSecrets=null'
expect_fail "intent with an empty forge secret list" "intentController/forgeSecrets" \
  -f "$ifx" --set-json 'intentController.forgeSecrets=[]'
expect_fail "intent without forge secrets, schema skipped" \
  "intentController.enabled requires intentController.forgeSecrets" \
  -f "$ifx" --set-json 'intentController.forgeSecrets=[]' --skip-schema-validation
expect_fail "intent with a malformed forge secret" "intentController/forgeSecrets/0" \
  -f "$ifx" --set-json 'intentController.forgeSecrets=["Not A Name"]'
expect_fail "intent with a unitless TTL" "intentController/config/intentTTL" \
  -f "$ifx" --set intentController.config.intentTTL=0
expect_fail "intent with a zero-turn stage" "intentController/config/build/maxTurns" \
  -f "$ifx" --set intentController.config.build.maxTurns=0

# ---- ...and the install NOTES say what it still needs ------------------------
notes notes-intent -f "$ifx"
notes_has notes-intent "intent-controller is enabled" yes
notes_has notes-intent "patchy-github, patchy-github-enterprise" yes
notes_has notes-intent "every intent build blocks on" yes
notes notes-intent-ri -f "$ifx" -f "$fixtures/repository-images.yaml"
notes_has notes-intent-ri "every intent build blocks on" no
notes notes-intent-off
notes_has notes-intent-off "intent-controller" no

# ---- egress broker limits ---------------------------------------------------
render limits -f "$fixtures/broker-limits.yaml"
cm limits egress-broker PATCHY_REQUESTS_PER_POD 2000
cm limits egress-broker PATCHY_CONCURRENT_PER_POD 4
cm limits egress-broker PATCHY_CONCURRENCY_WAIT -1s
cm limits egress-broker PATCHY_TOKENS_PER_POD 30000000
cm limits egress-broker PATCHY_TOKENS_PER_HOUR 100000000
cm limits egress-broker PATCHY_MAX_TOKENS_CEILING 64000
cm limits egress-broker PATCHY_MODEL_ALLOWLIST anthropic/claude-sonnet-5,anthropic/claude-opus-5
cm limits egress-broker PATCHY_BETA_DENYLIST none
cm limits egress-broker PATCHY_MAX_ANTHROPIC_REQUEST_BYTES 4194304
cm limits egress-broker PATCHY_MAX_REQUEST_BYTES 20971520
cm limits egress-broker PATCHY_PREAUTH_REQUESTS_PER_SECOND 2.5
cm limits egress-broker PATCHY_PREAUTH_BURST 100
cm limits egress-broker PATCHY_TOKEN_REVIEWS_PER_SECOND 20
render limits-extra -f "$fixtures/broker-limits.yaml" --set egressBroker.config.extra.PATCHY_TOKENS_PER_POD=7
cm limits-extra egress-broker PATCHY_TOKENS_PER_POD 7
if [ "$(get default 'select(.kind == "Deployment" and .metadata.name == "patchy-egress-broker") | .spec.template.metadata.annotations["checksum/config"]')" = \
  "$(get limits 'select(.kind == "Deployment" and .metadata.name == "patchy-egress-broker") | .spec.template.metadata.annotations["checksum/config"]')" ]; then
  fail "limits: the broker's checksum/config did not change, so an upgrade would not roll it"
fi

# ---- charts/patchy-config: Projects ------------------------------------------
# The CR chart renders .Values.projects into Project CRs verbatim; its values
# schema embeds the CRD's spec schema (hack/codegen.sh), so a malformed entry
# fails the render client-side rather than at the API server.
cfgchart=charts/patchy-config

# render_cfg NAME [helm args...]: render the CR chart into $out/NAME.yaml.
render_cfg() {
  name=$1
  shift
  if ! helm template patchy-config "$cfgchart" --namespace patchy "$@" >"$out/$name.yaml" 2>"$out/$name.err"; then
    fail "$name: render failed: $(cat "$out/$name.err")"
    : >"$out/$name.yaml"
  fi
}

# expect_fail_cfg DESC NEEDLE [helm args...]: the CR chart's render fails and
# its error contains NEEDLE verbatim.
expect_fail_cfg() {
  desc=$1
  needle=$2
  shift 2
  if helm template patchy-config "$cfgchart" --namespace patchy "$@" >/dev/null 2>"$out/guard.err"; then
    fail "config guard $desc: render succeeded, want a failure containing: $needle"
  elif ! grep -qF -- "$needle" "$out/guard.err"; then
    fail "config guard $desc: error lacks '$needle': $(cat "$out/guard.err")"
  fi
}

render_cfg cfg-default
expect cfg-default 'select(.kind == "Project") | .metadata.name' ""
cf=$fixtures/config-projects.yaml
render_cfg cfg-projects -f "$cf"
expect cfg-projects 'select(.kind == "Project") | .metadata.namespace + "/" + .metadata.name' "patchy/target
patchy/docs"
expect cfg-projects 'select(.kind == "Project") | .apiVersion' "patchy.bitwisemedia.uk/v1alpha1
patchy.bitwisemedia.uk/v1alpha1"
expect cfg-projects 'select(.kind == "Project") | .metadata.labels["app.kubernetes.io/name"]' "intent-controller
intent-controller"
# spec is rendered verbatim: the minimal entry gains nothing client-side (the
# CRD defaults it server-side), the full one keeps every value it set.
expect cfg-projects 'select(.kind == "Project" and .metadata.name == "target") | .spec | keys | join(",")' \
  "approvers,intentRepository,repositories"
# (helm's toYaml sorts map keys, so both sides are compared key-sorted.)
for path in intentRepository labels.trigger labels.approve approvers.logins limits checks requireRepositoryImage suspend \
  repositories; do
  want=$(yq eval -o=json -I=0 ".projects[1].spec.$path | sort_keys(..)" "$cf")
  expect cfg-projects \
    "select(.kind == \"Project\" and .metadata.name == \"docs\") | .spec.$path | sort_keys(..) | to_json(0)" "$want"
done
render_cfg cfg-common -f "$cf" --set-json 'commonLabels={"team":"platform"}' \
  --set-json 'commonAnnotations={"owner":"ops"}'
expect cfg-common 'select(.kind == "Project" and .metadata.name == "docs") | .metadata.labels.team + " " + .metadata.annotations.owner' \
  "platform ops"

# The guards: one valid project, then that project with one field broken.
# Each case passes the whole projects list (helm replaces a list wholesale
# rather than merging --set indices into a -f list), and the base renders, so
# every failure below is the broken field's.
base='{"name":"t","spec":{"intentRepository":"https://github.com/acme/intents","approvers":{"logins":["octocat"]},"repositories":[{"name":"t","url":"https://github.com/acme/t"}]}}'
# project JQ: the base project with a yq expression applied, as compact JSON.
project() {
  printf '%s' "$base" | yq eval -p=json -o=json -I=0 "$1" -
}
render_cfg cfg-guard-base --set-json "projects=[$base]"
expect cfg-guard-base 'select(.kind == "Project") | .metadata.name' "t"
expect_fail_cfg "project without a name" "projects/0" \
  --set-json "projects=[$(project 'del(.name)')]"
render_cfg cfg-guard-name --set-json "projects=[$(project '.name = "ppppppppppppppppppppppppp"')]"
expect cfg-guard-name 'select(.kind == "Project") | .metadata.name | length' "25"
expect_fail_cfg "project name over 25 characters" "projects/0/name" \
  --set-json "projects=[$(project '.name = "pppppppppppppppppppppppppp"')]"
expect_fail_cfg "repository key over 16 characters" "projects/0/spec/repositories/0/name" \
  --set-json "projects=[$(project '.spec.repositories[0].name = "kkkkkkkkkkkkkkkkk"')]"
expect_fail_cfg "unknown spec field" "projects/0/spec" \
  --set-json "projects=[$(project '.spec.bogus = true')]"
nine=$(for i in 0 1 2 3 4 5 6 7 8; do printf '{"name": "a%s", "url": "https://github.com/acme/a%s"},' "$i" "$i"; done)
expect_fail_cfg "nine repositories" "projects/0/spec/repositories" \
  --set-json "projects=[$(project ".spec.repositories = [${nine%,}]")]"
expect_fail_cfg "no approvers" "projects/0/spec/approvers/logins" \
  --set-json "projects=[$(project '.spec.approvers.logins = []')]"
expect_fail_cfg "credentials in a repository url" "projects/0/spec/repositories/0/url" \
  --set-json "projects=[$(project '.spec.repositories[0].url = "https://x:token@github.com/acme/t"')]"
expect_fail_cfg "cost ceiling past its cap" "projects/0/spec/limits/maxCostMicroUSD" \
  --set-json "projects=[$(project '.spec.limits.maxCostMicroUSD = 1000000001')]"

if [ "$failures" -gt 0 ]; then
  echo "chart-render-test: $failures assertion(s) failed" >&2
  exit 1
fi
echo "chart-render-test: all assertions passed"
