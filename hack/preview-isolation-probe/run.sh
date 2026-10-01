#!/usr/bin/env bash
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Re-run the first-instant EKS Auto Mode isolation gate against an immutable
# disposable PR image. This never reads a metadata response body or a token.
set -euo pipefail

if [[ $# -ne 1 || ! $1 =~ ^[0-9]+$ ]]; then
    echo "usage: $0 <open disposable patchy-preview-demo PR number>" >&2
    exit 2
fi

pr_number=$1
repo=devthenet-labs/patchy-preview-demo
region=us-east-1
registry=377946145366.dkr.ecr.us-east-1.amazonaws.com
image_repo=patchy/previews/patchy-preview-demo
deployment=preview-isolation-probe
created=0
export AWS_PROFILE=devthenet

cleanup() {
    if [[ $created -eq 1 ]]; then
        for slot in 0 1; do
            kubectl delete deployment "$deployment" -n "patchy-preview-$slot" --ignore-not-found --wait=true >&2 || true
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

if ! kubectl get nodeclass patchy-preview -o json | jq -e '.spec.networkPolicy == "DefaultDeny"' >/dev/null; then
    echo "preview NodeClass is not DefaultDeny" >&2
    exit 1
fi
for slot in 0 1; do
    ns="patchy-preview-$slot"
    kubectl get networkpolicy preview-isolation -n "$ns" >/dev/null
    if kubectl get deployment "$deployment" -n "$ns" >/dev/null 2>&1; then
        echo "existing $ns/$deployment: refusing to overwrite another probe" >&2
        exit 1
    fi
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
        nodes=$(kubectl get nodes -l karpenter.sh/nodepool=patchy-preview -o name)
        claims=$(kubectl get nodeclaims -o json | jq -r '.items[] | select(.metadata.labels["karpenter.sh/nodepool"] == "patchy-preview") | .metadata.name')
        if [[ -z $nodes && -z $claims ]]; then
            return 0
        fi
        sleep 10
        tries=$((tries + 1))
    done
    echo "preview pool did not return to zero within 20 minutes" >&2
    return 1
}

run_slot() {
    local slot=$1 ns="patchy-preview-$1" pod logs summary tries=0
    wait_zero
    echo "Cold-starting Deployment-managed probe in $ns" >&2
    created=1
    kubectl apply -n "$ns" -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $deployment
  namespace: $ns
spec:
  replicas: 1
  strategy: {type: Recreate}
  selector:
    matchLabels: {app: $deployment}
  template:
    metadata:
      labels: {app: $deployment}
    spec:
      serviceAccountName: default
      automountServiceAccountToken: false
      nodeSelector:
        karpenter.sh/nodepool: patchy-preview
        eks.amazonaws.com/nodeclass: patchy-preview
      tolerations:
        - key: patchy.devthe.net/preview-only
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
          resources:
            requests: {cpu: 25m, memory: 32Mi}
            limits: {cpu: 250m, memory: 256Mi}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
EOF
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
                    if [[ $summary != PASS ]] || ! jq -s -e \
                        '([.[] | select(.outcome == "blocked")] | length) == 128 and
                         ([.[] | select(.name == "dns" and .outcome == "ok")] | length) == 1 and
                         ([.[] | select(.outcome == "inconclusive" or .outcome == "REACHABLE")] | length) == 0' \
                        <<<"$logs" >/dev/null; then
                        echo "probe $ns/$pod ended $summary, or did not record all 128 blocked attempts and DNS" >&2
                        cleanup
                        exit 1
                    fi
                    echo "$ns/$pod: 128 blocked, DNS ok, no forbidden success ($summary)" >&2
                    kubectl delete deployment "$deployment" -n "$ns" --wait=true >&2
                    created=0
                    wait_zero
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

run_slot 0
run_slot 1
run_slot 0
echo "Cold-start isolation passed three runs. Agent-log check remains open; see README.md." >&2
echo "Close PR #$pr_number unmerged after recording the result." >&2
