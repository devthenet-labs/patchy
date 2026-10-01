# preview-controller

`preview-controller` is the default-off, namespaced runtime renderer for intent PRs. It uses `Preview` CRs as its only
work queue; intent-controller is the sole writer of their spec, and preview-controller is the sole writer of their
status. No GitHub, ECR or AWS call is made by this binary. It never runs an agent and never receives a Secret. See
[the accepted preview design](../design/intent-driven-development.md#previews-slice-2-d5) and the Helm security
foundation in `charts/patchy/README.md`.

Before rendering, the controller independently checks the Preview component against the operator's Project config and
its revision against the Intent's recorded open PR head. A mismatched spec fails closed and removes an exposed Ingress
before releasing the slot.

`preview.enabled` first creates guarded, empty slot namespaces and the non-default `alb-preview` class.
`previewController.enabled` then adds the controller, one namespaced Role in the release namespace and one in each slot,
and turns on intent-controller's Preview projector. Both switches default to `false`; the chart refuses to enable the
renderer without the foundation and the intent-controller. The controller's own NetworkPolicy allows only DNS and the
Kubernetes API Service `/32`. The slot Roles allow Deployment, Service and Ingress CRUD plus Pod and ReplicaSet reads,
and nothing cluster-scoped. The security foundation still applies its fail-closed admission, NetworkPolicy, quota and
dedicated-node rules to every workload.

To opt in one benign application Project, set a fixed runtime contract on its `Project`:

```yaml
spec:
  preview:
    imageRepository: 377946145366.dkr.ecr.us-east-1.amazonaws.com/patchy/previews/patchy-preview-demo
    port: 8080
    readinessPath: /health
```

The Project must still have exactly one application repository. Do **not** add this block to patchy-target. The runtime
publisher must publish the immutable `sha-<full PR head SHA>` image before the preview can become Ready. No fork PR may
publish; the uncredentialed build has no `id-token` access, and the trusted main-context publisher never checks out or
executes PR code. The Preview's host is `<project>-<issue>.<preview.hostSuffix>`; its spec contains the Intent UID, host
label, component name/image repository/full head SHA/port/readiness path, and a 72-hour TTL.

`Pending` takes a free slot, or `Queued` waits in creation order behind other Previews. `Deploying` creates a fixed
ClusterIP Service and a single-replica, restricted Deployment on the dedicated `DefaultDeny` preview pool. Only after a
Pod is Ready with the requested image and has an image ID does it create the fixed `alb-preview` Ingress and mark the
Preview `Ready` with its URL and image ID. A PR-head change removes the old Ingress before updating the runtime image.
Timed-out rollouts are retried at most `previewController.config.maxRetries` times (default three) with a 10-minute
per-attempt deadline; `Failed` frees the slot after cleanup. `Expired` is reached 72 hours after the last successful
deployment (or after creation/attempt start if none ever succeeds); a later new PR head may revive it. An Intent
merge/close or Project opt-out deletes the Preview. Its finalizer waits until its rendered resources and
Pods/ReplicaSets are gone. A periodic sweep deletes owned orphans left by a lost CR, while the queue refuses to reuse a
slot that still contains owned resources. Reducing `slotCount` while a Preview owns a removed slot deliberately blocks
finalizer removal: restore the count and drain first.

Before enabling any Project preview, complete the separate ALB, placeholder Ingress and wildcard DNS check-in, then run
the cold-start isolation gate in `hack/preview-isolation-probe/README.md` with a disposable PR image. Repeat that gate
after any EKS, Auto Mode or VPC CNI upgrade and before relying on previews again. The direct Auto Mode network-policy
agent-log check is still open; absence of errors in Kubernetes events is not a substitute.
