#!/usr/bin/env bash
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Re-run the first-instant EKS Auto Mode isolation gate against an immutable
# disposable PR image. This never reads a metadata response body or a token.
#
# The site is the caller's to name, in the environment (see README.md):
#   PROBE_REPOSITORY  owner/name of the app repository the disposable PR is
#                     open on
#   PROBE_IMAGE       the preview image repository its trusted publisher
#                     pushes to, <account>.dkr.ecr.<region>.amazonaws.com/
#                     <preview path prefix>/<app>
#   PROBE_TAINT_KEY   the preview NodePool's NoExecute taint key (the chart's
#                     preview.nodeIsolation.taintKey)
#   PROBE_NODE_POOL, PROBE_NODE_CLASS
#                     the preview NodePool and NodeClass (the chart's
#                     preview.nodeIsolation.nodePool/nodeClass; default
#                     patchy-preview)
# AWS credentials (AWS_PROFILE and the rest) come from the caller's
# environment as they are; the script sets none.
set -euo pipefail

usage() {
    echo "usage: PROBE_REPOSITORY=<owner>/<app> PROBE_IMAGE=<account>.dkr.ecr.<region>.amazonaws.com/<prefix>/<app> \\" >&2
    echo "       PROBE_TAINT_KEY=<taint key> $0 <open disposable PR number>" >&2
    exit 2
}

if [[ $# -ne 1 || ! $1 =~ ^[0-9]+$ ]]; then
    usage
fi
for name in PROBE_REPOSITORY PROBE_IMAGE PROBE_TAINT_KEY; do
    if [[ -z ${!name:-} ]]; then
        echo "$name is not set" >&2
        usage
    fi
done
# Each value lands in a gh, aws or kubectl argument or in the probe's
# manifest, so each must be exactly the shape it names.
if [[ ! $PROBE_REPOSITORY =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
    echo "PROBE_REPOSITORY must be <owner>/<name>, not $PROBE_REPOSITORY" >&2
    exit 2
fi
if [[ ! $PROBE_IMAGE =~ ^([0-9]{12}\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com)/([a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*)$ ]]; then
    echo "PROBE_IMAGE must be an ECR repository, <account>.dkr.ecr.<region>.amazonaws.com/<path>, not $PROBE_IMAGE" >&2
    exit 2
fi
registry=${BASH_REMATCH[1]}
region=${BASH_REMATCH[2]}
image_repo=${BASH_REMATCH[3]}
dns_name='[a-z0-9]([-a-z0-9]*[a-z0-9])?'
if [[ ! $PROBE_TAINT_KEY =~ ^($dns_name(\.$dns_name)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$ ]]; then
    echo "PROBE_TAINT_KEY must be a taint key, [<dns prefix>/]<name>, not $PROBE_TAINT_KEY" >&2
    exit 2
fi
taint_key=$PROBE_TAINT_KEY
node_pool=${PROBE_NODE_POOL:-patchy-preview}
node_class=${PROBE_NODE_CLASS:-patchy-preview}
for value in "$node_pool" "$node_class"; do
    if [[ ! $value =~ ^$dns_name(\.$dns_name)*$ ]]; then
        echo "PROBE_NODE_POOL and PROBE_NODE_CLASS must be Kubernetes object names, not $value" >&2
        exit 2
    fi
done

pr_number=$1
repo=$PROBE_REPOSITORY
deployment=preview-isolation-probe
probe_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# The sibling stage's targets: another component's Pod and Service in the
# probe's slot, and a Pod in the other slot. The Service is
# sibling-service.yaml, which names it this too.
sibling=preview-isolation-sibling
other=preview-isolation-other
created=0

cleanup() {
    if [[ $created -eq 1 ]]; then
        for slot in 0 1; do
            for name in "$deployment" "$sibling" "$other"; do
                kubectl delete deployment "$name" -n "patchy-preview-$slot" --ignore-not-found --wait=true >&2 || true
            done
            kubectl delete service "$sibling" -n "patchy-preview-$slot" --ignore-not-found --wait=true >&2 || true
        done
    fi
}
trap cleanup EXIT

pr_json=$(gh pr view "$pr_number" --repo "$repo" --json state,isCrossRepository,headRefOid,headRefName,baseRefName)
if ! jq -e '.state == "OPEN" and .isCrossRepository == false and .baseRefName == "main" and
    (.headRefName | startswith("test/preview-")) and (.headRefOid | test("^[0-9a-f]{40}$"))' <<<"$pr_json" >/dev/null; then
    echo "PR is not an open, same-repository disposable test/preview-* PR into main" >&2
    exit 1
fi
sha=$(jq -r '.headRefOid' <<<"$pr_json")
digest=$(aws ecr describe-images --region "$region" --repository-name "$image_repo" \
    --image-ids "imageTag=sha-$sha" --query 'imageDetails[0].imageDigest' --output text)
if [[ ! $digest =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "trusted publisher has not published the full-PR-head image" >&2
    exit 1
fi
echo "Using disposable PR #$pr_number head $sha ($digest)" >&2

if ! kubectl get nodeclass "$node_class" -o json | jq -e '.spec.networkPolicy == "DefaultDeny"' >/dev/null; then
    echo "preview NodeClass is not DefaultDeny" >&2
    exit 1
fi
for slot in 0 1; do
    ns="patchy-preview-$slot"
    kubectl get networkpolicy preview-isolation -n "$ns" >/dev/null
    for name in "$deployment" "$sibling" "$other"; do
        if kubectl get deployment "$name" -n "$ns" >/dev/null 2>&1; then
            echo "existing $ns/$name: refusing to overwrite another probe" >&2
            exit 1
        fi
    done
done

service_ip() {
    kubectl get service "$2" -n "$1" -o jsonpath='{.spec.clusterIP}'
}
api_ip=$(service_ip default kubernetes)
broker_ip=$(service_ip patchy patchy-egress-broker)
integration_ip=$(service_ip patchy patchy-integration-controller)
source_ip=$(service_ip patchy patchy-source-controller)
status_ip=$(service_ip patchy patchy-status-server)
for ip in "$api_ip" "$broker_ip" "$integration_ip" "$source_ip" "$status_ip"; do
    if [[ ! $ip =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo "could not discover every forbidden service ClusterIP" >&2
        exit 1
    fi
done

wait_zero() {
    local tries=0
    while (( tries < 120 )); do
        local nodes claims
        nodes=$(kubectl get nodes -l "karpenter.sh/nodepool=$node_pool" -o name)
        claims=$(kubectl get nodeclaims -o json | jq -r --arg pool "$node_pool" \
            '.items[] | select(.metadata.labels["karpenter.sh/nodepool"] == $pool) | .metadata.name')
        if [[ -z $nodes && -z $claims ]]; then
            return 0
        fi
        sleep 10
        tries=$((tries + 1))
    done
    echo "preview pool did not return to zero within 20 minutes" >&2
    return 1
}

# create_probe NAMESPACE NAME [EXTRA_ENV_LINES]: a Deployment-managed netprobe
# Pod, in the exact preview Pod shape, labelled app=NAME. EXTRA_ENV_LINES are
# further env entries, already indented. Every probe object is created, never
# client-side applied: apply adds a last-applied annotation, which the slot
# Service policy refuses (it admits only a health-check path).
create_probe() {
    local ns=$1 name=$2 extra_env=${3:-}
    created=1
    kubectl create -n "$ns" -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $name
  namespace: $ns
spec:
  replicas: 1
  strategy: {type: Recreate}
  selector:
    matchLabels: {app: $name}
  template:
    metadata:
      labels: {app: $name}
    spec:
      serviceAccountName: default
      automountServiceAccountToken: false
      nodeSelector:
        karpenter.sh/nodepool: $node_pool
        eks.amazonaws.com/nodeclass: $node_class
      tolerations:
        - key: $taint_key
          operator: Equal
          value: "true"
          effect: NoExecute
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: probe
          image: $registry/$image_repo:sha-$sha
          imagePullPolicy: Always
          env:
            - {name: PROBE_KUBERNETES_API, value: "$api_ip"}
            - {name: PROBE_EGRESS_BROKER, value: "$broker_ip"}
            - {name: PROBE_INTEGRATION_CONTROLLER, value: "$integration_ip"}
            - {name: PROBE_SOURCE_CONTROLLER, value: "$source_ip"}
            - {name: PROBE_STATUS_SERVER, value: "$status_ip"}
$extra_env
          resources:
            requests: {cpu: 25m, memory: 32Mi}
            limits: {cpu: 250m, memory: 256Mi}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
EOF
}

# run_slot SLOT [EXTRA_ENV_LINES] [TARGETS]: run the probe in slot SLOT and
# require every one of its TARGETS (default 8) to be blocked on all 16
# attempts, and DNS to work. The cold-start runs call it on an empty pool.
run_slot() {
    local slot=$1 extra_env=${2:-} targets=${3:-8} ns="patchy-preview-$1" pod logs summary tries=0 blocked
    blocked=$((targets * 16))
    echo "Starting Deployment-managed probe in $ns ($targets forbidden targets)" >&2
    create_probe "$ns" "$deployment" "$extra_env"
    while (( tries < 120 )); do
        pod=$(kubectl get pods -n "$ns" -l "app=$deployment" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
        if [[ -n $pod ]]; then
            logs=$(kubectl logs "$pod" -n "$ns" -c probe 2>/dev/null || true)
            if [[ -n $logs ]]; then
                if jq -e 'select(.outcome == "REACHABLE" or .outcome == "SECURITY_FAILURE")' <<<"$logs" >/dev/null 2>&1; then
                    echo "SECURITY FAILURE in $ns/$pod; deleting probe now" >&2
                    cleanup
                    exit 1
                fi
                summary=$(jq -r 'select(.name == "summary") | .outcome' <<<"$logs" 2>/dev/null | tail -1)
                if [[ -n $summary ]]; then
                    if [[ $summary != PASS ]] || ! jq -s -e --argjson blocked "$blocked" \
                        '([.[] | select(.outcome == "blocked")] | length) == $blocked and
                         ([.[] | select(.name == "dns" and .outcome == "ok")] | length) == 1 and
                         ([.[] | select(.outcome == "inconclusive" or .outcome == "REACHABLE")] | length) == 0' \
                        <<<"$logs" >/dev/null; then
                        echo "probe $ns/$pod ended $summary, or did not record all $blocked blocked attempts and DNS" >&2
                        cleanup
                        exit 1
                    fi
                    echo "$ns/$pod: $blocked blocked, DNS ok, no forbidden success ($summary)" >&2
                    kubectl delete deployment "$deployment" -n "$ns" --wait=true >&2
                    return 0
                fi
            fi
        fi
        sleep 10
        tries=$((tries + 1))
    done
    echo "probe in $ns produced no complete result within 20 minutes" >&2
    return 1
}

# pod_ip NAMESPACE NAME: the IP of NAME's running Pod, waiting up to 20 minutes.
pod_ip() {
    local ns=$1 name=$2 ip tries=0
    while (( tries < 120 )); do
        ip=$(kubectl get pods -n "$ns" -l "app=$name" -o jsonpath='{.items[0].status.podIP}' 2>/dev/null || true)
        if [[ $ip =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
            echo "$ip"
            return 0
        fi
        sleep 10
        tries=$((tries + 1))
    done
    echo "no Pod IP for $ns/$name within 20 minutes" >&2
    return 1
}

# run_siblings: multi-component isolation. Two more probe Pods stand in for
# a sibling component (with its own Service) in slot 0 and for a Pod in slot
# 1; the probe in slot 0 must then also find the sibling's Pod and Service
# and the other slot's Pod blocked. They listen on nothing, so a broken
# policy shows as a refused (inconclusive) attempt, which fails the run too.
# The pool is warm by now: this checks the slot policy between Pods, while
# the three runs before it check a cold start.
run_siblings() {
    local sibling_ip service_ip other_ip extra_env
    echo "Starting sibling targets in patchy-preview-0 and patchy-preview-1" >&2
    create_probe patchy-preview-0 "$sibling"
    create_probe patchy-preview-1 "$other"
    kubectl create -n patchy-preview-0 -f "$probe_dir/sibling-service.yaml"
    sibling_ip=$(pod_ip patchy-preview-0 "$sibling")
    other_ip=$(pod_ip patchy-preview-1 "$other")
    service_ip=$(service_ip patchy-preview-0 "$sibling")
    if [[ ! $service_ip =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo "could not discover the sibling Service ClusterIP" >&2
        exit 1
    fi
    extra_env="            - {name: PROBE_SIBLING_POD, value: \"$sibling_ip\"}
            - {name: PROBE_SIBLING_SERVICE, value: \"$service_ip\"}
            - {name: PROBE_OTHER_SLOT_POD, value: \"$other_ip\"}"
    run_slot 0 "$extra_env" 11
    kubectl delete deployment "$sibling" -n patchy-preview-0 --wait=true >&2
    kubectl delete service "$sibling" -n patchy-preview-0 --wait=true >&2
    kubectl delete deployment "$other" -n patchy-preview-1 --wait=true >&2
    created=0
    wait_zero
}

for slot in 0 1 0; do
    wait_zero
    run_slot "$slot"
    created=0
done
wait_zero
run_siblings
echo "Cold-start isolation passed three runs and the sibling run. Agent-log check remains open; see README.md." >&2
echo "Close PR #$pr_number unmerged after recording the result." >&2
