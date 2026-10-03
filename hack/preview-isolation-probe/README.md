# Re-running the preview cold-start isolation gate

Run this after **every EKS, EKS Auto Mode, or VPC CNI upgrade, and before relying on previews again**. It reproduces the
2026-09-30 accepted test: three fresh Deployment-managed starts in slots 0, 1, 0, each after the dedicated pool has
reached zero nodes. The binary's first network operation is a concurrent burst: four workers make four attempts against
each of eight forbidden targets. All 128 connections must be blocked; DNS may succeed after the policy is programmed. A
connection refusal is inconclusive, not a pass. The IMDSv2 test issues only a token PUT, reads only the status line, and
treats an established TCP connection as a failure; it never reads a token or credentials.

A fourth, **sibling run** covers multi-component previews, where several components share one slot. Two more probe Pods
stand in for a sibling component (with its own ClusterIP Service) in slot 0 and for a Pod in slot 1, then the probe runs
in slot 0 with three more forbidden targets: the sibling's Pod, the sibling's Service and the other slot's Pod. All 176
connections must be blocked. The stand-ins listen on nothing, so a policy that let a connection through shows as a
refused, inconclusive attempt, which fails the run too. The pool is warm by then: the sibling run checks the slot policy
between Pods, while the three runs before it check the cold start.

The script **creates** every probe object (`kubectl create`) and never client-side applies one: `kubectl apply` adds a
`kubectl.kubernetes.io/last-applied-configuration` annotation, and a slot Service may carry no annotation but a safe
health-check path, so admission would refuse the sibling's Service. The sibling Service is `sibling-service.yaml`, and
the chart's preview policy envtest (`charts/patchy/preview_probe_envtest_test.go`) runs the script's own command on it
against the rendered policies.

The probe is **never merged into `devthenet-labs/patchy-preview-demo` main**. Prepare a disposable same-repo PR on a
`test/preview-*` branch based on current main. Copy this directory's `cmd/netprobe/` to the demo repo's `cmd/netprobe/`,
and replace the demo repo's runtime `Dockerfile` and `.dockerignore` with the two files here. Do not alter its
agent-toolchain image, build/publish workflow, or application source. Verify the PR diff contains only those files; open
the PR and wait for the uncredentialed build and the trusted publisher to succeed. The publisher must push
`patchy/previews/patchy-preview-demo:sha-<full PR head SHA>`; the script confirms that tag and its digest exist in ECR.
Never run the probe from a fork PR or from an image tagged from main.

From this patchy checkout, with `brvtl` active in `gh` and access to `devthenet-dev`, run:

```sh
bash hack/preview-isolation-probe/run.sh <disposable-PR-number>
```

The script discovers the current Kubernetes and patchy Service ClusterIPs (rather than trusting stale IPs), verifies the
NodeClass is `DefaultDeny` and both slot NetworkPolicies exist, and refuses to overwrite an existing probe Deployment.
It uses the immutable PR-head image in a **Deployment-managed Pod**, with the existing admission rules unchanged. It
deletes each Deployment after its result and waits for the preview NodePool and NodeClaims to return to zero. A
forbidden success, inconclusive attempt, DNS failure, refused admission, missing image, or timeout stops the run; keep
previews unused and investigate. Afterward, close the disposable PR **without merging** it.

This tests observed connectivity, **not** the Auto Mode network-policy agent logs. The `no bpf context registered`
agent-log check remains open until node diagnostics or managed-agent log delivery make those logs directly available.
Record that distinction with every re-run. Before enabling previews, also verify that the preview ALB, DNS alias,
admission guards and slot quotas match the approved configuration for that rollout.
