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
# Byte-order collation: some assertions compare a yq key list with a list
# sorted by sort(1), and under a UTF-8 locale sort ignores punctuation
# (PATCHY_INTENT_PR_POLL_INTERVAL vs PATCHY_INTENT_PREVIEWS_ENABLED), so the
# test failed on developer machines while passing in CI's C locale.
export LC_ALL=C

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

# same_render WANT GOT: GOT is the render WANT records, whichever helm
# rendered either. Every line counts byte for byte, comments included, except
# blank ones: helm 4.3 writes two blank lines before each document a
# {{- range }} emits where 4.2 wrote none, and changed nothing else, so a
# toolchain bump must not read as a changed policy. A blank line inside a
# block scalar is content, though, so the parsed documents must be equal too.
# On a difference, $out/same.diff says what differs.
same_render() {
  grep -v '^[[:space:]]*$' "$1" >"$out/same.want" || true
  grep -v '^[[:space:]]*$' "$2" >"$out/same.got" || true
  if ! cmp -s "$out/same.want" "$out/same.got"; then
    { echo "(blank lines left out)"; diff "$out/same.want" "$out/same.got"; } >"$out/same.diff" || true
    return 1
  fi
  if ! yq -o=json -I=0 '.' "$1" >"$out/same.want.json" 2>"$out/same.diff" ||
    ! yq -o=json -I=0 '.' "$2" >"$out/same.got.json" 2>"$out/same.diff"; then
    return 1
  fi
  if ! cmp -s "$out/same.want.json" "$out/same.got.json"; then
    { echo "the parsed documents differ (a blank line inside a block scalar?)"; diff "$out/same.want.json" "$out/same.got.json"; } >"$out/same.diff" || true
    return 1
  fi
}

# golden_render NAME FILE [helm args...]: the render (pass --show-only for one
# template) is the committed golden FILE, as same_render compares them.
golden_render() {
  name=$1
  file=$2
  shift 2
  if ! helm template patchy "$chart" --namespace patchy "$@" >"$out/$name.golden" 2>"$out/$name.err"; then
    fail "$name: render failed: $(cat "$out/$name.err")"
  elif ! same_render "$file" "$out/$name.golden"; then
    fail "$name: render differs from $file: $(head -20 "$out/same.diff")"
  fi
}

# same_render_is DESC WANT FILE: same_render's verdict on FILE against the
# default golden is WANT (same or differs).
same_render_is() {
  got=differs
  if same_render "$golden/preview-admission.default.yaml" "$3"; then
    got=same
  fi
  if [ "$got" != "$2" ]; then
    fail "golden comparison, $1: $got, want $2"
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
expect default 'select(.metadata.name == "alb-preview" or (.metadata.name | test("^patchy-preview-")) or (.metadata.namespace | test("^patchy-preview-"))) | .kind' ""
expect default 'select(.metadata.name == "patchy-preview-controller") | .kind' ""
for key in PATCHY_REPOSITORY_IMAGES PATCHY_REPOSITORY_IMAGE_REGISTRIES PATCHY_REPOSITORY_IMAGE_ON_REJECT \
  PATCHY_REPOSITORY_IMAGE_COSIGN_KEY_FILE PATCHY_AGENT_EPHEMERAL_STORAGE PATCHY_CHANGESET_MAX_ENTRIES DOCKER_CONFIG \
  PATCHY_REPOSITORY_IMAGE_DENIED_REGISTRIES; do
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

# ---- preview security foundation (no workloads or ALB until later) ---------
render preview -f "$fixtures/preview-foundation.yaml"
render preview-guardrails -f "$fixtures/preview-foundation.yaml" --set preview.placeholder.enabled=false
render preview-runtime -f "$fixtures/preview-foundation.yaml" \
  -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml"
expect preview-guardrails 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-preview-ingresses") | .metadata.name' patchy-preview-ingresses
expect preview-guardrails 'select(.metadata.name == "patchy-preview-placeholder") | .kind' ""
expect preview-guardrails 'select(.kind == "Ingress" and (.metadata.namespace | test("^patchy-preview-"))) | .metadata.name' ""
expect preview 'select(.kind == "Service" and .metadata.name == "patchy-preview-placeholder") | .metadata.namespace' patchy-preview-0
expect preview 'select(.kind == "Service" and .metadata.name == "patchy-preview-placeholder") | .spec.type' ClusterIP
expect preview 'select(.kind == "Service" and .metadata.name == "patchy-preview-placeholder") | .spec | has("selector")' false
expect preview 'select(.kind == "Service" and .metadata.name == "patchy-preview-placeholder") | .metadata.annotations."helm.sh/resource-policy"' keep
expect preview 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .metadata.namespace' patchy-preview-0
expect preview 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .spec.ingressClassName' alb-preview
expect preview 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .spec.rules[0].host' placeholder.preview.patchy.devthe.net
expect preview 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .spec.rules[0].http.paths[0].backend.service.name' patchy-preview-placeholder
expect preview 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .metadata.annotations."helm.sh/resource-policy"' keep
expect preview 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .spec | has("tls")' false
expect preview 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .spec | has("defaultBackend")' false
expect preview 'select(.kind == "Pod" and (.metadata.namespace | test("^patchy-preview-"))) | .metadata.name' ""
expect preview-runtime 'select(.kind == "Deployment" and .metadata.name == "patchy-preview-controller") | .spec.template.spec.containers[0].image | split(":") | .[0]' \
  ghcr.io/devthenet-labs/patchy/preview-controller
expect preview-runtime 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-preview-controller") | .metadata.namespace' patchy
expect preview-runtime 'select(.kind == "Role" and .metadata.name == "patchy-preview-controller") | .metadata.namespace' \
  'patchy
patchy-preview-0
patchy-preview-1'
expect preview-runtime 'select(.kind == "ClusterRole" and .metadata.name == "patchy-preview-controller") | .kind' ""
expect preview-runtime 'select(.kind == "Role" and .metadata.namespace == "patchy" and .metadata.name == "patchy-preview-controller") | .rules[] | select(.resources[] == "projects") | .verbs[]' get
expect preview-runtime 'select(.kind == "Role" and .metadata.name == "patchy-preview-controller") | .rules[].resources[] | select(. == "secrets" or . == "namespaces" or . == "networkpolicies")' ""
# It records Events (leader election, reconciles) in its own namespace like every other controller; without this its
# event recorder is refused at startup.
expect preview-runtime 'select(.kind == "Role" and .metadata.namespace == "patchy" and .metadata.name == "patchy-preview-controller") | .rules[] | select(.resources[] == "events") | .verbs | join(",")' create,patch
expect preview-runtime 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-preview-controller") | .spec.egress[].to[].ipBlock.cidr | select(. != null)' 172.20.0.1/32
cm preview-runtime preview-controller PATCHY_PREVIEW_SLOT_COUNT 2
cm preview-runtime preview-controller PATCHY_PREVIEW_IMAGE_PREFIX 377946145366.dkr.ecr.us-east-1.amazonaws.com/patchy/previews/
cm preview-runtime preview-controller PATCHY_PREVIEW_HOST_SUFFIX preview.patchy.devthe.net
cm preview-runtime preview-controller PATCHY_PREVIEW_NODE_POOL patchy-preview
cm preview-runtime preview-controller PATCHY_PREVIEW_NODE_CLASS patchy-preview
cm preview-runtime preview-controller PATCHY_PREVIEW_TAINT_KEY patchy.devthe.net/preview-only
cm preview-runtime intent-controller PATCHY_INTENT_PREVIEWS_ENABLED true
# Ready waits for the load balancer's target health by default (previews run
# only on EKS Auto Mode, which was seen injecting the gate on a live preview):
# the controller is told to, and the slot namespaces opt into Auto Mode's
# readiness-gate injection. Opting out drops both, so the slots run ungated
# exactly as slice 2 ran them.
cm preview-runtime preview-controller PATCHY_PREVIEW_TARGET_HEALTH true
expect preview-runtime 'select(.kind == "Namespace" and (.metadata.name | test("^patchy-preview-"))) | .metadata.labels."eks.amazonaws.com/pod-readiness-gate-inject"' 'enabled
enabled'
render preview-runtime-ungated -f "$fixtures/preview-foundation.yaml" \
  -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml" \
  --set previewController.config.targetHealth=false
cm preview-runtime-ungated preview-controller PATCHY_PREVIEW_TARGET_HEALTH false
expect preview-runtime-ungated 'select(.kind == "Namespace" and (.metadata.name | test("^patchy-preview-"))) | .metadata.labels."eks.amazonaws.com/pod-readiness-gate-inject"' 'null
null'
expect_fail 'preview controller without slots' 'requires preview.enabled' \
  -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml"
expect_fail 'preview controller without stable placeholder' 'requires preview.placeholder.enabled' \
  -f "$fixtures/preview-foundation.yaml" -f "$fixtures/intent-controller.yaml" \
  -f "$fixtures/preview-controller.yaml" --set preview.placeholder.enabled=false
expect_fail 'preview controller without intent writer' 'requires intentController.enabled' \
  -f "$fixtures/preview-foundation.yaml" -f "$fixtures/preview-controller.yaml"
expect_fail 'preview controller without API CIDR' 'apiServerCIDR is required' \
  -f "$fixtures/preview-foundation.yaml" -f "$fixtures/intent-controller.yaml" \
  --set previewController.enabled=true
expect_fail 'preview controller with broad API CIDR' 'does not match pattern' \
  -f "$fixtures/preview-foundation.yaml" -f "$fixtures/intent-controller.yaml" \
  --set previewController.enabled=true --set previewController.config.apiServerCIDR=0.0.0.0/0
expect preview 'select(.kind == "Namespace" and (.metadata.name | test("^patchy-preview-"))) | .metadata.name' 'patchy-preview-0
patchy-preview-1'
expect preview 'select(.kind == "Namespace" and (.metadata.name | test("^patchy-preview-"))) | .metadata.labels."pod-security.kubernetes.io/enforce"' 'restricted
restricted'
for kind in Namespace NetworkPolicy ResourceQuota LimitRange ValidatingAdmissionPolicy ValidatingAdmissionPolicyBinding IngressClass IngressClassParams; do
  expect preview "select(.kind == \"$kind\" and ((.metadata.name | test(\"preview\")) or (.metadata.namespace | test(\"preview\")))) | .metadata.annotations.\"helm.sh/resource-policy\" | select(. != \"keep\")" ""
done
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .metadata.namespace' 'patchy-preview-0
patchy-preview-1'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.policyTypes | join(",")' 'Ingress,Egress
Ingress,Egress'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.podSelector | length' '0
0'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.egress | length' '1
1'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.egress[0].to | length' '1
1'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.egress[0].ports | map(.protocol + "/" + (.port | tostring)) | join(",")' 'UDP/53,TCP/53
UDP/53,TCP/53'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.egress[].to[].ipBlock.cidr' '172.20.0.10/32
172.20.0.10/32'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.ingress[].from[].ipBlock.cidr' '10.40.128.0/24
10.40.129.0/24
10.40.128.0/24
10.40.129.0/24'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.ingress[].ports[].port' 'http
http'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.ingress | length' '1
1'
# The slot quota holds one Preview of up to four components: a Service each
# plus slot 0's placeholder, and every component's Pod replaced at once.
expect preview 'select(.kind == "ResourceQuota" and .metadata.name == "preview-quota") | .spec.hard.services + "/" + .spec.hard.pods' '5/8
5/8'
expect preview 'select(.kind == "ResourceQuota" and .metadata.name == "preview-quota") | .spec.hard."services.loadbalancers" + "/" + .spec.hard."services.nodeports"' '0/0
0/0'
# The multi-component admission rules are rendered into the slot policies.
expect preview 'select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name == "patchy-preview-pods" or .metadata.name == "patchy-preview-deployments")) | .spec.validations[].expression | select(. == "size(variables.pod.containers) == 1")' 'size(variables.pod.containers) == 1
size(variables.pod.containers) == 1'
expect preview 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-preview-ingresses") | .spec.validations[].message | select(test("one rule of at most 4 Prefix paths"))' \
  'preview Ingresses have one rule of at most 4 Prefix paths in the component path grammar, each backed by a preview- Service on port 80'
expect preview 'select(.kind == "IngressClass" and .metadata.name == "alb-preview") | .metadata.annotations."ingressclass.kubernetes.io/is-default-class"' 'false'
expect preview 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec.namespaceSelector.matchExpressions[0].values | join(",")' 'patchy-preview-0,patchy-preview-1'
expect preview 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec.inboundCIDRs | join(",")' '75.70.97.14/32'
expect preview 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec.certificateARNs | length' '1'
expect preview 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec.sslPolicy' 'ELBSecurityPolicy-TLS13-1-2-2021-06'
expect preview 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec.listeners[0].protocol + "/" + (.spec.listeners[0].port | tostring)' 'HTTPS/443'
# albSubnetIDs pins the preview ALB to the subnets whose CIDRs the slots
# admit; unset, placement stays with Auto Mode's discovery, as before.
expect preview 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec | has("subnets")' 'false'
render preview-subnets -f "$fixtures/preview-foundation.yaml" \
  --set 'preview.albSubnetIDs={subnet-0123456789abcdef0,subnet-0123456789abcdef1}'
expect preview-subnets 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec.subnets.ids | join(",")' 'subnet-0123456789abcdef0,subnet-0123456789abcdef1'
expect preview-subnets 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec.subnets | keys | join(",")' 'ids'
expect_fail 'preview subnet IDs not one per CIDR' 'must name one subnet per preview.albSubnetCIDRs entry' \
  -f "$fixtures/preview-foundation.yaml" --set 'preview.albSubnetIDs={subnet-0123456789abcdef0}'
expect_fail 'preview subnet ID that is not one' 'does not match pattern' \
  -f "$fixtures/preview-foundation.yaml" --set 'preview.albSubnetIDs={sg-0123456789abcdef0,subnet-0123456789abcdef1}'
expect preview 'select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name | test("^patchy-preview-"))) | .spec.failurePolicy' 'Fail
Fail
Fail
Fail
Fail
Fail
Fail'
expect preview 'select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name | test("^patchy-preview-outside-"))) | .metadata.name' 'patchy-preview-outside-pods
patchy-preview-outside-deployments
patchy-preview-outside-ingresses'
expect preview 'select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name | test("^patchy-preview-outside-"))) | .spec.matchConditions[0].expression' '!(request.namespace in ["patchy-preview-0","patchy-preview-1"])
!(request.namespace in ["patchy-preview-0","patchy-preview-1"])
!(request.namespace in ["patchy-preview-0","patchy-preview-1"])'
expect preview 'select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name | test("^patchy-preview-")) and (.metadata.name | test("^patchy-preview-outside-") | not)) | .spec.matchConditions[0].expression' 'request.namespace in ["patchy-preview-0","patchy-preview-1","patchy-preview-2","patchy-preview-3"]
request.namespace in ["patchy-preview-0","patchy-preview-1","patchy-preview-2","patchy-preview-3"]
request.namespace in ["patchy-preview-0","patchy-preview-1","patchy-preview-2","patchy-preview-3"]
request.namespace in ["patchy-preview-0","patchy-preview-1","patchy-preview-2","patchy-preview-3"]'
expect preview 'select(.kind == "ValidatingAdmissionPolicyBinding" and (.metadata.name | test("^patchy-preview-all-slots-"))) | has("spec")' 'true
true
true
true'
expect preview 'select(.kind == "ValidatingAdmissionPolicyBinding" and (.metadata.name | test("^patchy-preview-0|^patchy-preview-1"))) | .spec.matchResources.namespaceSelector.matchLabels."kubernetes.io/metadata.name"' 'patchy-preview-0
patchy-preview-1
patchy-preview-0
patchy-preview-1
patchy-preview-0
patchy-preview-1
patchy-preview-0
patchy-preview-1'
render preview-one -f "$fixtures/preview-foundation.yaml" --set preview.slotCount=1
expect preview-one 'select(.kind == "ValidatingAdmissionPolicyBinding" and .metadata.name == "patchy-preview-0-pods") | .spec.matchResources.namespaceSelector.matchLabels."kubernetes.io/metadata.name"' 'patchy-preview-0'
expect preview-one 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-preview-outside-pods") | .spec.matchConditions[0].expression' '!(request.namespace in ["patchy-preview-0"])'
expect preview 'select(.kind == "Deployment" and (.metadata.namespace | test("preview"))) | .metadata.name' ""
expect_fail 'preview missing image registry' 'preview.imageRegistry' --set preview.enabled=true
expect_fail 'preview missing node pool' 'preview.nodeIsolation.nodePool' -f "$fixtures/preview-foundation.yaml" --set preview.nodeIsolation.nodePool=
expect_fail 'preview missing node class' 'preview.nodeIsolation.nodeClass' -f "$fixtures/preview-foundation.yaml" --set preview.nodeIsolation.nodeClass=
expect_fail 'preview missing node taint key' 'preview.nodeIsolation.taintKey' -f "$fixtures/preview-foundation.yaml" --set preview.nodeIsolation.taintKey=
expect_fail 'preview zero slots' "'/preview/slotCount': minimum" -f "$fixtures/preview-foundation.yaml" --set preview.slotCount=0
expect_fail 'preview zero slots, schema skipped' 'preview.slotCount must be between 1 and 4' \
  -f "$fixtures/preview-foundation.yaml" --set preview.slotCount=0 --skip-schema-validation
expect_fail 'preview missing cert' 'preview.certificateARN' -f "$fixtures/preview-foundation.yaml" --set preview.certificateARN=

# ---- the preview image prefix (preview.imagePathPrefix) ---------------------
# One helper (patchy.previewImagePrefix) feeds the slot admission policy, the
# preview-controller and source-controller's denied path. With the default
# path the admission policies are byte for byte what main rendered before the
# path was configurable, blank lines aside (same_render: helm versions differ
# in those alone): preview-admission.default.yaml is that render (helm 4.2.3),
# and is never regenerated to make this pass. A custom path changes only the
# prefix and its length (preview-admission.custom.yaml; regenerate it with the
# same helm template command and review the diff against the default).
golden=$fixtures/golden
pv=$fixtures/preview-foundation.yaml
reg=377946145366.dkr.ecr.us-east-1.amazonaws.com
# The comparison itself, on edits of the default golden, so no helm version
# decides whether it is exercised. Helm 4.3's shape (two blank lines before
# every document but the first) is the same render; a changed rule, a changed
# comment or a blank line inside a folded CEL expression is not.
awk 'NR > 1 && $0 == "---" { print ""; print "" } { print }' \
  "$golden/preview-admission.default.yaml" >"$out/golden-helm43.yaml"
same_render_is 'helm 4.3 blank lines between documents' same "$out/golden-helm43.yaml"
sed 's/substring(61)/substring(60)/' "$golden/preview-admission.default.yaml" >"$out/golden-rule.yaml"
same_render_is 'a changed rule' differs "$out/golden-rule.yaml"
sed 's/^    # for this security decision\.$/    # for this decision./' \
  "$golden/preview-admission.default.yaml" >"$out/golden-comment.yaml"
same_render_is 'a changed comment' differs "$out/golden-comment.yaml"
awk '{ print } !done && /variables\.pod\.containers\.all\(c,$/ { print ""; done = 1 }' \
  "$golden/preview-admission.default.yaml" >"$out/golden-scalar.yaml"
same_render_is 'a blank line inside a block scalar' differs "$out/golden-scalar.yaml"
golden_render preview-vap-default "$golden/preview-admission.default.yaml" -f "$pv" \
  --show-only templates/preview-admission.yaml
golden_render preview-vap-custom "$golden/preview-admission.custom.yaml" -f "$pv" \
  --set preview.imagePathPrefix=acme/runtime.images --show-only templates/preview-admission.yaml
render preview-prefix-custom -f "$pv" -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml" \
  --set preview.imagePathPrefix=acme/runtime.images
cm preview-prefix-custom preview-controller PATCHY_PREVIEW_IMAGE_PREFIX "$reg/acme/runtime.images/"
notes notes-preview-prefix -f "$pv" --set preview.imagePathPrefix=acme/runtime.images
notes_has notes-preview-prefix "--image $reg/acme/runtime.images/<app>" yes
expect_fail 'preview image path prefix empty' "'/preview/imagePathPrefix': '' does not match pattern" \
  -f "$pv" --set preview.imagePathPrefix=
expect_fail 'preview image path prefix trailing slash' "'patchy/previews/' does not match pattern" \
  -f "$pv" --set preview.imagePathPrefix=patchy/previews/
expect_fail 'preview image path prefix leading slash' "'/patchy/previews' does not match pattern" \
  -f "$pv" --set preview.imagePathPrefix=/patchy/previews
expect_fail 'preview image path prefix uppercase' "does not match pattern" \
  -f "$pv" --set preview.imagePathPrefix=Patchy/previews
expect_fail 'preview image path prefix unset' "missing property 'imagePathPrefix'" \
  -f "$pv" --set preview.imagePathPrefix=null
# A render that skips the schema still meets the template's own check.
expect_fail 'preview image path prefix empty, schema skipped' 'preview.imagePathPrefix "" must be' \
  -f "$pv" --set preview.imagePathPrefix= --skip-schema-validation
expect_fail 'preview image path prefix trailing slash, schema skipped' \
  'preview.imagePathPrefix "patchy/previews/" must be' \
  -f "$pv" --set preview.imagePathPrefix=patchy/previews/ --skip-schema-validation

# Disjoint from the agent allowlist: an agent.repositoryImages.registries
# entry that is the preview prefix, contains it or sits under it fails the
# render, compared on segment boundaries after the fold source-controller
# applies (case, an explicit :443, ECR's dual-stack and FIPS endpoint names).
# A runtime image is built from an unreviewed pull request head and must
# never be admissible as an agent sandbox image, nor a toolchain image as a
# preview.
ri=$fixtures/repository-images.yaml
for entry in "$reg/patchy/" "$reg/patchy/previews" "$reg/patchy/previews/" "$reg/patchy/previews/agents/" \
  "377946145366.DKR.ECR.us-east-1.amazonaws.com/Patchy/" "$reg:443/patchy/" \
  "377946145366.dkr-ecr.us-east-1.on.aws/patchy/" "377946145366.dkr.ecr-fips.us-east-1.amazonaws.com/patchy/previews/x"; do
  expect_fail "agent registry $entry overlaps the preview prefix" \
    "agent.repositoryImages.registries entry \"$entry\" overlaps the preview image prefix $reg/patchy/previews/" \
    -f "$pv" -f "$ri" --set "agent.repositoryImages.registries={ghcr.io/example/agent-images/,$entry}"
done
expect_fail 'custom preview prefix under an agent registry' \
  "agent.repositoryImages.registries entry \"$reg/apps/\" overlaps the preview image prefix $reg/apps/previews/" \
  -f "$pv" -f "$ri" --set "agent.repositoryImages.registries={$reg/apps/}" --set preview.imagePathPrefix=apps/previews
# Disjoint entries render: a sibling sharing the prefix's leading
# characters, another region's registry, a disjoint custom pair; and with
# repository images off nothing is judged (the kill switch never fails a
# render).
for entry in "$reg/patchy/previews-agents/" "377946145366.dkr.ecr.us-west-2.amazonaws.com/patchy/" "$reg/patchy/app-envs/"; do
  render "preview-disjoint-$(echo "$entry" | tr -c 'a-z0-9\n' '-')" -f "$pv" -f "$ri" \
    --set "agent.repositoryImages.registries={$entry}"
done
render preview-disjoint-custom -f "$pv" -f "$ri" --set "agent.repositoryImages.registries={$reg/apps/agents/}" \
  --set preview.imagePathPrefix=apps/previews
render preview-overlap-images-off -f "$pv" -f "$ri" --set agent.repositoryImages.enabled=false \
  --set "agent.repositoryImages.registries={$reg/patchy/}"
cm preview-overlap-images-off source-controller PATCHY_REPOSITORY_IMAGE_DENIED_REGISTRIES null
# source-controller refuses agent images from the preview prefix on its own
# too (runnerimage.Policy.Deny), whatever reaches its registries key.
render preview-ri -f "$pv" -f "$ri"
cm preview-ri source-controller PATCHY_REPOSITORY_IMAGE_DENIED_REGISTRIES "$reg/patchy/previews/"
cm preview-disjoint-custom source-controller PATCHY_REPOSITORY_IMAGE_DENIED_REGISTRIES "$reg/apps/previews/"
for c in investigation-controller remediation-controller integration-controller; do
  cm preview-ri "$c" PATCHY_REPOSITORY_IMAGE_DENIED_REGISTRIES null
done

# ---- EKS Auto Mode toggles: off by default, and off means absent ------------
# clusterDNSCIDR, preview.nodeIsolation.create and edgeIngressClass.create are
# opt-in: the default render has none of their objects or rules, and setting
# every one of their other values without the switch renders the default byte
# for byte, so an install that never sets them never changes.
pf=$fixtures/preview-foundation.yaml
am=$fixtures/auto-mode.yaml
unswitched=$fixtures/auto-mode-unswitched.yaml
dnsnp='select(.kind == "NetworkPolicy") | .spec.egress[] | select(.ports | map(.port) | contains([53])) | .to[] | select(has("ipBlock")) | .ipBlock.cidr'
expect default "$dnsnp" ""
expect default 'select(.kind == "NodeClass" or .kind == "NodePool" or .kind == "IngressClass" or .kind == "IngressClassParams") | .kind' ""
# same BASE OTHER: the two renders are byte-identical.
same() {
  if ! cmp -s "$out/$1.yaml" "$out/$2.yaml"; then
    fail "$2: values under preview.nodeIsolation and edgeIngressClass changed the $1 render without their create switch"
  fi
}
render toggles-off -f "$unswitched"
same default toggles-off
# With previews on too, the live shape of an install whose NodePool and
# NodeClass were applied by hand: the chart must not start rendering its own
# (an ownership conflict on upgrade) because a sub-value such as subnetIDs got
# filled in before create. The edge class must not appear beside alb-preview.
nodesoredge='select(.kind == "NodeClass" or .kind == "NodePool" or ((.kind == "IngressClass" or .kind == "IngressClassParams") and .metadata.name != "alb-preview")) | .kind + "/" + .metadata.name'
render preview-toggles-off -f "$pf" -f "$unswitched"
same preview preview-toggles-off
expect preview "$nodesoredge" ""
render preview-runtime-toggles-off -f "$pf" -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml" \
  -f "$unswitched"
same preview-runtime preview-runtime-toggles-off
expect preview-runtime "$nodesoredge" ""
render toggles-off-ingress --set webhook.host=patchy.example.com --set webhook.ingress.enabled=true \
  --set statusServer.host=status.patchy.example.com --set statusServer.ingress.enabled=true \
  --set edgeIngressClass.loadBalancerName=example-patchy
expect toggles-off-ingress 'select(.kind == "Ingress") | .spec.ingressClassName' "null
null"

# ---- clusterDNSCIDR: node-local DNS beside every kube-system DNS rule --------
# EKS Auto Mode serves DNS at the cluster DNS address on each node, not from
# kube-system pods. The rule lands in exactly the policies that already allow
# DNS to kube-system, UDP and TCP 53 only, and selects no new pod.
render dns --set clusterDNSCIDR=10.100.0.10/32
dnsrule='.spec.egress[] | select(.to // [] | any_c(.ipBlock.cidr == "10.100.0.10/32"))'
kubedns='select(.kind == "NetworkPolicy" and (.spec.egress // [] | any_c(.to // [] | any_c(.namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "kube-system")))) | .metadata.namespace + "/" + .metadata.name'
nodedns='select(.kind == "NetworkPolicy" and (.spec.egress // [] | any_c(.to // [] | any_c(.ipBlock.cidr == "10.100.0.10/32")))) | .metadata.namespace + "/" + .metadata.name'
expect dns "$nodedns" "patchy/patchy-egress-broker
patchy/patchy-integration-controller
patchy/patchy-source-controller
patchy/patchy-context-controller
patchy/patchy-investigation-controller
patchy/patchy-remediation-controller
patchy-agents/patchy-agents-egress
patchy/patchy-status-server"
expect dns "select(.kind == \"NetworkPolicy\") | $dnsrule | .ports | map(.protocol + \"/\" + (.port | tostring)) | join(\",\")" \
  "$(for i in 1 2 3 4 5 6 7 8; do echo UDP/53,TCP/53; done)"
expect dns "select(.kind == \"NetworkPolicy\") | $dnsrule | .to | length" "$(for i in 1 2 3 4 5 6 7 8; do echo 1; done)"
npsel='select(.kind == "NetworkPolicy") | .metadata.namespace + "/" + .metadata.name + " " + (.spec.podSelector | to_json(0)) + " " + (.spec.policyTypes | join(","))'
if [ "$(get default "$npsel")" != "$(get dns "$npsel")" ]; then
  fail "dns: clusterDNSCIDR changed which pods a NetworkPolicy selects"
fi
# Every component that can run, on: the two lists stay equal.
render dns-all -f "$pf" -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml" \
  -f "$fixtures/evaluation-controller.yaml" --set clusterDNSCIDR=10.100.0.10/32
if [ -z "$(get dns-all "$kubedns")" ] || [ "$(get dns-all "$kubedns")" != "$(get dns-all "$nodedns")" ]; then
  fail "dns-all: policies with kube-system DNS ($(get dns-all "$kubedns" | tr '\n' ' ')) != policies with node-local DNS ($(get dns-all "$nodedns" | tr '\n' ' '))"
fi
expect dns-all "$nodedns | select(test(\"intent-controller|evaluation-controller|preview-controller\"))" "patchy/patchy-evaluation-controller
patchy/patchy-intent-controller
patchy/patchy-preview-controller"
# The preview slot policy's DNS address defaults to it; an explicit
# preview.dnsCIDR wins; with neither, enabling previews fails.
render preview-dns-default -f "$pf" --set preview.dnsCIDR= --set clusterDNSCIDR=10.100.0.10/32
expect preview-dns-default 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.egress[].to[].ipBlock.cidr' '10.100.0.10/32
10.100.0.10/32'
render preview-dns-explicit -f "$pf" --set clusterDNSCIDR=10.100.0.10/32
expect preview-dns-explicit 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.egress[].to[].ipBlock.cidr' '172.20.0.10/32
172.20.0.10/32'
expect_fail 'preview without any DNS address' 'preview.dnsCIDR is required when preview.enabled=true' -f "$pf" --set preview.dnsCIDR=
expect_fail 'clusterDNSCIDR wider than one address' "'/clusterDNSCIDR'" --set clusterDNSCIDR=172.20.0.0/16

# ---- preview.nodeIsolation.create: the DefaultDeny NodeClass and NodePool -----
render am -f "$pf" -f "$am"
nc='select(.kind == "NodeClass")'
np='select(.kind == "NodePool")'
expect am "$nc | .apiVersion + \" \" + .metadata.name" "eks.amazonaws.com/v1 patchy-preview"
expect am "$nc | .spec.networkPolicy + \" \" + .spec.networkPolicyEventLogs" "DefaultDeny Disabled"
expect am "$nc | .spec.role" example-preview-node
expect am "$nc | .spec.subnetSelectorTerms[].id" "subnet-0123456789abcdef0
subnet-0fedcba9876543210"
expect am "$nc | .spec.securityGroupSelectorTerms[].id" sg-0123456789abcdef0
expect am "$nc | .spec.ephemeralStorage | .size + \" \" + (.iops | tostring) + \" \" + (.throughput | tostring)" "20Gi 3000 125"
expect am "$np | .apiVersion + \" \" + .metadata.name" "karpenter.sh/v1 patchy-preview"
expect am "$np | .spec.template.spec.nodeClassRef | .group + \"/\" + .kind + \"/\" + .name" eks.amazonaws.com/NodeClass/patchy-preview
expect am "$np | .spec.template.spec.taints[] | .key + \"=\" + .value + \":\" + .effect" "patchy.devthe.net/preview-only=true:NoExecute"
expect am "$np | .spec.template.spec.requirements[] | .key + \" \" + .operator + \" \" + (.values | join(\",\"))" \
  "node.kubernetes.io/instance-type In t3a.medium
kubernetes.io/arch In amd64
kubernetes.io/os In linux
karpenter.sh/capacity-type In on-demand"
expect am "$np | .spec.disruption | .consolidationPolicy + \" \" + .consolidateAfter + \" \" + .budgets[0].nodes" "WhenEmpty 30s 1"
expect am "$np | .spec.limits | .cpu + \"/\" + .nodes" "4/2"
for kind in NodeClass NodePool IngressClass IngressClassParams; do
  expect am "select(.kind == \"$kind\") | .metadata.annotations.\"helm.sh/resource-policy\" | select(. != \"keep\")" ""
done
render am-tuned -f "$pf" -f "$am" --set-json 'preview.nodeIsolation.instanceTypes=["m7g.large","c7g.large"]' \
  --set preview.nodeIsolation.arch=arm64 --set preview.nodeIsolation.cpuLimit=8 --set preview.nodeIsolation.nodeLimit=3 \
  --set preview.nodeIsolation.ephemeralStorage.size=40Gi
expect am-tuned "$np | .spec.template.spec.requirements[0:2][] | .values | join(\",\")" "m7g.large,c7g.large
arm64"
expect am-tuned "$np | .spec.limits | .cpu + \"/\" + .nodes" "8/3"
expect am-tuned "$nc | .spec.ephemeralStorage.size" 40Gi
expect_fail 'nodes without previews' 'preview.nodeIsolation.create requires preview.enabled' -f "$am"
expect_fail 'nodes without a role' 'preview.nodeIsolation.create requires preview.nodeIsolation.role' \
  -f "$pf" -f "$am" --set preview.nodeIsolation.role=
expect_fail 'nodes without subnets' 'preview.nodeIsolation.create requires preview.nodeIsolation.subnetIDs' \
  -f "$pf" -f "$am" --set-json 'preview.nodeIsolation.subnetIDs=[]'
expect_fail 'nodes without security groups' 'preview.nodeIsolation.create requires preview.nodeIsolation.securityGroupIDs' \
  -f "$pf" -f "$am" --set-json 'preview.nodeIsolation.securityGroupIDs=[]'
expect_fail 'node role as an ARN' "'/preview/nodeIsolation/role'" \
  -f "$pf" -f "$am" --set preview.nodeIsolation.role=arn:aws:iam::123456789012:role/preview
expect_fail 'node subnet as a CIDR' "'/preview/nodeIsolation/subnetIDs/0'" \
  -f "$pf" -f "$am" --set-json 'preview.nodeIsolation.subnetIDs=["10.0.0.0/24"]'
# AWS IDs are 8 or 17 hex digits, as preview.albSubnetIDs already holds them.
expect_fail 'node subnet of neither ID length' "'/preview/nodeIsolation/subnetIDs/0'" \
  -f "$pf" -f "$am" --set-json 'preview.nodeIsolation.subnetIDs=["subnet-0123456789ab"]'
expect_fail 'node security group of neither ID length' "'/preview/nodeIsolation/securityGroupIDs/0'" \
  -f "$pf" -f "$am" --set-json 'preview.nodeIsolation.securityGroupIDs=["sg-0123456789ab"]'
# DefaultDeny is fixed: there is no value to turn it into DefaultAllow.
expect_fail 'node class network policy as a value' "additional properties 'networkPolicy' not allowed" \
  -f "$pf" -f "$am" --set preview.nodeIsolation.networkPolicy=DefaultAllow

# ---- edgeIngressClass.create: the edge class the Ingresses fall back to -------
ec='select(.kind == "IngressClass" and .metadata.name == "patchy-edge")'
ep='select(.kind == "IngressClassParams" and .metadata.name == "patchy-edge")'
expect am "$ec | .spec.controller" eks.amazonaws.com/alb
expect am "$ec | .spec.parameters | .apiGroup + \"/\" + .kind + \"/\" + .name" eks.amazonaws.com/IngressClassParams/patchy-edge
expect am "$ec | .metadata.annotations.\"ingressclass.kubernetes.io/is-default-class\"" false
expect am "$ep | .spec | .scheme + \" \" + .loadBalancerName + \" \" + .group.name" "internet-facing example-patchy example-patchy"
expect am "$ep | .spec.certificateARNs | join(\",\")" \
  arn:aws:acm:us-east-1:123456789012:certificate/00000000-0000-0000-0000-000000000000
expect am "$ep | .spec.namespaceSelector.matchLabels.\"kubernetes.io/metadata.name\"" patchy
expect am 'select(.kind == "Ingress" and .metadata.namespace == "patchy") | .metadata.name + " " + .spec.ingressClassName' \
  "patchy-webhook patchy-edge
patchy-status-server patchy-edge"
# An explicit className wins; the name, scheme and certificates are values;
# the namespace pin follows the release namespace.
render am-edge-tuned -f "$am" --set preview.nodeIsolation.create=false --namespace platform --set webhook.ingress.className=nginx \
  --set edgeIngressClass.name=public-edge --set edgeIngressClass.scheme=internal --set-json 'edgeIngressClass.certificateARNs=[]'
expect am-edge-tuned 'select(.kind == "Ingress") | .metadata.name + " " + .spec.ingressClassName' \
  "patchy-webhook nginx
patchy-status-server public-edge"
expect am-edge-tuned 'select(.kind == "IngressClassParams") | .metadata.name + " " + .spec.scheme + " " + (.spec | has("certificateARNs") | tostring)' \
  "public-edge internal false"
expect am-edge-tuned 'select(.kind == "IngressClassParams") | .spec.namespaceSelector.matchLabels."kubernetes.io/metadata.name"' platform
expect_fail 'edge without a load balancer name' 'edgeIngressClass.create requires edgeIngressClass.loadBalancerName' \
  --set edgeIngressClass.create=true
expect_fail 'edge on the preview ALB' 'edgeIngressClass.loadBalancerName must differ from preview.albName' \
  -f "$pf" -f "$am" --set edgeIngressClass.loadBalancerName=devthenet-dev-preview
expect_fail 'edge named like the preview class' 'edgeIngressClass.name must not be alb-preview' \
  -f "$am" --set preview.nodeIsolation.create=false --set edgeIngressClass.name=alb-preview
# The name is the ALB group's name too, and a group name is lowercase.
expect_fail 'edge load balancer name with capitals' "'/edgeIngressClass/loadBalancerName'" \
  --set edgeIngressClass.create=true --set edgeIngressClass.loadBalancerName=Example-Patchy
expect_fail 'edge load balancer name over 32 characters' "'/edgeIngressClass/loadBalancerName'" \
  --set edgeIngressClass.create=true --set edgeIngressClass.loadBalancerName=abcdefghijklmnopqrstuvwxyz0123456

# ---- the install NOTES walk previews through what is left -----------------
notes notes-preview -f "$pf"
notes_has notes-preview "kubectl get ingress patchy-preview-placeholder -n patchy-preview-0" yes
notes_has notes-preview "point *.preview.patchy.devthe.net at that hostname" yes
notes_has notes-preview "NodeClass patchy-preview and NodePool patchy-preview must" yes
notes_has notes-preview "bash hack/preview-isolation-probe/run.sh" yes
notes_has notes-preview "--taint-key patchy.devthe.net/preview-only" yes
notes_has notes-preview "COST: the preview ALB bills from the moment the placeholder creates it" yes
notes_has notes-preview "(even with previewController off)" yes
# The NodePool is the operator's, so its disruption policy and limits are
# theirs: no promise of the chart's 30s consolidation or a cap.
notes_has notes-preview "A preview node bills from launch until NodePool patchy-preview's own" yes
notes_has notes-preview "30s after" no
notes_has notes-preview "CPUs" no
notes notes-preview-nodes -f "$pf" -f "$am"
notes_has notes-preview-nodes "this release renders NodeClass patchy-preview" yes
notes_has notes-preview-nodes "A preview node (t3a.medium) bills from launch until it" yes
notes_has notes-preview-nodes "is consolidated, 30s after its last preview Pod. Budget by CPU: the pool" yes
notes_has notes-preview-nodes "stops launching nodes at 4 CPUs." yes
# CPU is the bound; the node limit is not promised (Auto Mode may not
# enforce limits.nodes).
notes_has notes-preview-nodes "so do not count on its limit of 2 nodes." yes
notes_has notes-preview-nodes "and 2 nodes" no
notes_has notes-preview-nodes "disruption policy removes it" no
notes notes-preview-guardrails -f "$pf" --set preview.placeholder.enabled=false
notes_has notes-preview-guardrails "preview.placeholder.enabled is false" yes
notes_has notes-preview-guardrails "kubectl get ingress patchy-preview-placeholder" no
notes notes-preview-off
notes_has notes-preview-off "Previews:" no
notes_has notes-preview-off "COST:" no

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
# No preview prefix to deny without previews.
cm on source-controller PATCHY_REPOSITORY_IMAGE_DENIED_REGISTRIES null
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
# Multi-repository intents are off unless asked for, and the flag rolls the
# controller like any other setting.
cm intent intent-controller PATCHY_INTENT_MULTI_REPO false
render intent-multi -f "$ifx" --set intentController.config.multiRepo=true
cm intent-multi intent-controller PATCHY_INTENT_MULTI_REPO true
if [ "$(get intent "$it | .metadata.annotations[\"checksum/config\"]")" = \
  "$(get intent-multi "$it | .metadata.annotations[\"checksum/config\"]")" ]; then
  fail "intent-multi: checksum/config did not change, so turning multiRepo on would not roll the controller"
fi
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
# Per-repository previews (slice 3): rendered verbatim, their fields
# checked client-side. The CEL rules (at most four, distinct paths, no
# shorthand beside them) are the API server's; the schema envtest covers them.
webpreview='{"imageRepository":"registry.example/patchy/previews/acme-web","port":8080,"readinessPath":"/healthz"}'
render_cfg cfg-repo-preview --set-json "projects=[$(project ".spec.repositories[0].preview = $webpreview | .spec.repositories[0].preview.path = \"/api\"")]"
expect cfg-repo-preview 'select(.kind == "Project") | .spec.repositories[0].preview.path + " " + .spec.repositories[0].preview.imageRepository' \
  "/api registry.example/patchy/previews/acme-web"
expect_fail_cfg "a preview path outside the grammar" "projects/0/spec/repositories/0/preview/path" \
  --set-json "projects=[$(project ".spec.repositories[0].preview = $webpreview | .spec.repositories[0].preview.path = \"/API\"")]"
expect_fail_cfg "an unknown repository preview field" "projects/0/spec/repositories/0/preview" \
  --set-json "projects=[$(project ".spec.repositories[0].preview = $webpreview | .spec.repositories[0].preview.dockerfile = \"x\"")]"

if [ "$failures" -gt 0 ]; then
  echo "chart-render-test: $failures assertion(s) failed" >&2
  exit 1
fi
echo "chart-render-test: all assertions passed"
