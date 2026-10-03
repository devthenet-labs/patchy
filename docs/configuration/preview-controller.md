# preview-controller

`preview-controller` is the default-off, namespaced runtime renderer for intent PRs. It uses `Preview` CRs as its only
work queue; intent-controller is the sole writer of their spec, and preview-controller is the sole writer of their
status. No GitHub, ECR or AWS call is made by this binary. It never runs an agent and never receives a Secret. See
[the accepted preview design](../design/intent-driven-development.md#previews-slice-2-d5) and the Helm security
foundation in `charts/patchy/README.md`.

Before rendering, the controller re-derives the Preview from the operator's Project config and the Intent's recorded
state (`v1alpha1.DesiredPreviewComponents`, the function the writer uses too) and checks every component against it. A
mismatched spec fails closed and removes an exposed Ingress before releasing the slot.

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

`spec.preview` is the one-repository shorthand: it needs exactly one application repository. Do **not** add this block
to patchy-target. The runtime publisher must publish the immutable `sha-<full PR head SHA>` image before the preview can
become Ready. No fork PR may publish; the uncredentialed build has no `id-token` access, and the trusted main-context
publisher never checks out or executes PR code. The Preview's host is `<project>-<issue>.<preview.hostSuffix>`; its spec
contains the Intent UID, host label, each component's name/image repository/full head SHA/port/readiness path/route
path, and a 72-hour TTL.

A Project with several repositories previews them per repository instead, at most four, each under its own path on the
one host:

```yaml
spec:
  repositories:
    - name: web
      url: https://github.com/acme/Acme.Web_App
      preview:
        imageRepository: 377946145366.dkr.ecr.us-east-1.amazonaws.com/patchy/previews/acme-web
        port: 8080
        readinessPath: /healthz
        path: / # the default
    - name: lib # never previewed: no runtime
      url: https://github.com/acme/shared-lib
    - name: api
      url: https://github.com/acme/api
      preview:
        imageRepository: 377946145366.dkr.ecr.us-east-1.amazonaws.com/patchy/previews/acme-api
        port: 8080
        readinessPath: /api/healthz
        path: /api
```

Each previewed repository becomes one component, named by its key, with its own Deployment and Service: the first keeps
the single-component name `preview-<project>-<issue>`, and each further one is `preview-<project>-<issue>-<key>`. Their
selectors carry the component's name, so no Service reaches a sibling's Pods. The one Ingress routes a `Prefix` path per
component, longest first; the load balancer does not rewrite paths, so an API under `/api` serves `/api/...`, and a page
calls it same-origin, from the browser (the slot NetworkPolicy keeps components from reaching each other directly). Each
component Service carries its own health-check path; the Ingress carries the `/` component's. The repository name is
free: the key and the image leaf are the operator's. intent-controller writes every Preview through the derivation the
preview-controller checks it against, so a one-repository Project may use either form. An intent of a Project with
several repositories waits for multi-repository intents (the Project reports `UnsupportedRepositories` until then), so
only one-repository Projects get Previews today.

`Pending` takes a free slot, or `Queued` waits in creation order behind other Previews. `Deploying` first prunes the
objects of components no longer rendered, so a renamed component's Service fits the slot quota, then creates a fixed
ClusterIP Service and a single-replica, restricted Deployment per component on the dedicated `DefaultDeny` preview pool;
a component whose spec did not change is not rolled. With `targetHealth` off (the default), only after every component
has a Ready Pod with the requested image and an image ID does it create the fixed `alb-preview` Ingress and mark the
Preview `Ready` with its URL and each component's revision and image ID, and a PR-head change removes the old Ingress
before updating the runtime images. With `targetHealth` on, the Ingress comes first and stays (below). Cleanup and
pruning find a Preview's objects by its UID label, so a component the Project stops previewing is removed. Timed-out
rollouts are retried at most `previewController.config.maxRetries` times (default three) with a 10-minute per-attempt
deadline; `Failed` frees the slot after cleanup. `Expired` is reached 72 hours after the last successful deployment (or
after creation/attempt start if none ever succeeds); a later new PR head may revive it. An Intent merge/close or Project
opt-out deletes the Preview. Its finalizer waits until its rendered resources and Pods/ReplicaSets are gone. A periodic
sweep deletes owned orphans left by a lost CR, while the queue refuses to reuse a slot that still contains owned
resources. Reducing `slotCount` while a Preview owns a removed slot deliberately blocks finalizer removal: restore the
count and drain first.

With `previewController.config.targetHealth: true` (`--preview-target-health` on the binary; off by default), `Ready`
also means the load balancer's target is healthy, so the host does not answer 404 or nothing for the seconds after
`Ready` while a new target registers. The chart then labels the slot namespaces
`eks.amazonaws.com/pod-readiness-gate-inject: enabled`, EKS Auto Mode's opt-in to inject a target-health readiness gate
into the slot's Pods (the upstream AWS Load Balancer Controller's label is `elbv2.k8s.aws/pod-readiness-gate-inject`,
but the `alb-preview` class is Auto Mode's). The gate is injected only into a Pod created after its target group binding
exists, which follows the Ingress, so in this mode the Ingress is created with the Services, before any Deployment; the
Deployments wait until the load balancer has published the Ingress's address; and a component is Ready only once its Pod
carries a readiness gate and every gate is True. The Ingress stays across a PR-head redeploy and a retry (the new Pods
need its binding): the Recreate rollout stops the old revision before the new one starts, and the load balancer routes
nothing to a target until it is healthy. A spec that adds a component (or renames one) replaces the Ingress instead,
withdrawing the host until `Ready`: the load balancer keeps an Ingress's address once published, so only a new Ingress's
address shows that the new component's binding exists. A Preview already `Ready` when the mode is switched on keeps
serving on its Pods rather than being restarted to grow a gate. The Auto Mode label comes from AWS (an EKS Auto Mode
blog post and a maintainer's answer on aws/containers-roadmap#2511), not from the EKS user guide, and has not yet been
checked on a live preview, which is why the mode is off by default: if Auto Mode injects no gate, every rollout times
out and retries, the retry's message naming the missing gate, and `targetHealth: false` restores the ungated behaviour.
Turn it on for a live single-repository preview check, and keep it on once a Preview reaches `Ready` on a gated Pod.

Before enabling any Project preview, complete the separate ALB, placeholder Ingress and wildcard DNS check-in, then run
the cold-start isolation gate in `hack/preview-isolation-probe/README.md` with a disposable PR image. Repeat that gate
after any EKS, Auto Mode or VPC CNI upgrade and before relying on previews again. The direct Auto Mode network-policy
agent-log check is still open; absence of errors in Kubernetes events is not a substitute.
