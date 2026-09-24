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

if [ "$failures" -gt 0 ]; then
  echo "chart-render-test: $failures assertion(s) failed" >&2
  exit 1
fi
echo "chart-render-test: all assertions passed"
