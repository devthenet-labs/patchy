{{/*
Copyright 2026 Bitwise Media Group Ltd.
SPDX-License-Identifier: MIT
*/}}

{{- define "patchy.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "patchy.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Per-controller ConfigMap name. Context: dict "root" $ "name" <component>.
*/}}
{{- define "patchy.configMapName" -}}
{{- printf "%s-%s-config" (include "patchy.fullname" .root) .name -}}
{{- end }}

{{/*
Per-controller ServiceAccount name: <controller>.serviceAccount.name, or
<fullname>-<component>. Context: dict "root" $ "name" <component> "vals"
<controller values>.
*/}}
{{- define "patchy.serviceAccountName" -}}
{{- .vals.serviceAccount.name | default (printf "%s-%s" (include "patchy.fullname" .root) .name) -}}
{{- end }}

{{/*
Selector labels for one component. Context: dict "root" $ "name" <component>.
app.kubernetes.io/name matches deploy/kustomize (source-controller, ...);
instance disambiguates releases.
*/}}
{{- define "patchy.selectorLabels" -}}
app.kubernetes.io/name: {{ .name }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/part-of: patchy
{{- end }}

{{/*
Full label set. Context: dict "root" $ "name" <component> ["component" <role>].
*/}}
{{- define "patchy.labels" -}}
{{- /*
The bare `app` duplicates app.kubernetes.io/name for tooling that predates
the recommended-label set (kubescape C-0076 counts it, some dashboards group
by it). Not in selectorLabels: selectors are immutable on upgrade.
*/ -}}
app: {{ .name }}
helm.sh/chart: {{ printf "%s-%s" .root.Chart.Name .root.Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/version: {{ .root.Chart.AppVersion | quote }}
{{ include "patchy.selectorLabels" . }}
{{- with .component }}
app.kubernetes.io/component: {{ . }}
{{- end }}
{{- with .root.Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Object annotations: commonAnnotations plus optional per-object extras, which
win key-by-key. Context: dict "root" $ ["extra" <map>]. Empty output when
both maps are empty — wrap the annotations: key in `with`.
*/}}
{{- define "patchy.annotations" -}}
{{- $a := merge (dict) (.extra | default dict) (.root.Values.commonAnnotations | default dict) -}}
{{- if $a -}}
{{- toYaml $a -}}
{{- end -}}
{{- end }}

{{/*
Image reference for one binary. Context: dict "root" $ "binary" <name>
["image" <per-component override map>]. The repository (registry included)
defaults to <image.repository>/<binary>; a digest pins (and beats the tag);
the tag defaults to v<appVersion>, which is how goreleaser tags the images.
*/}}
{{- define "patchy.image" -}}
{{- $img := .image | default dict -}}
{{- $g := .root.Values.image -}}
{{- $repository := $img.repository | default (printf "%s/%s" $g.repository .binary) -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $repository $img.digest -}}
{{- else -}}
{{- printf "%s:%s" $repository ($img.tag | default $g.tag | default (printf "v%s" .root.Chart.AppVersion)) -}}
{{- end -}}
{{- end }}

{{/*
The pod labels internal/jobs stamps on every agent Job pod. Fixed by the
controller, not by this chart — the sandbox NetworkPolicies select on them.
*/}}
{{- define "patchy.agentPodSelector" -}}
app.kubernetes.io/name: patchy-agent
app.kubernetes.io/managed-by: patchy
{{- end }}

{{/*
The edge IngressClass's name while edgeIngressClass.create is on, else empty:
the className the webhook and status-page Ingresses fall back to when their
own is empty, so with create off they render exactly as before.
*/}}
{{- define "patchy.edgeIngressClassName" -}}
{{- $e := .Values.edgeIngressClass | default dict -}}
{{- if $e.create -}}
{{- $e.name | default (printf "%s-edge" (include "patchy.fullname" .)) -}}
{{- end -}}
{{- end }}

{{/*
The node-local DNS egress rule (clusterDNSCIDR): UDP and TCP 53 to that one
address. Every NetworkPolicy that allows DNS to kube-system includes it right
after that rule, so it widens exactly the pods the chart already isolates and
selects no new ones (a separate podSelector-{} policy would egress-isolate any
pod no other policy selects, such as a controller whose
networkPolicy.create is false). EKS Auto Mode serves DNS from each node at
the cluster DNS Service address, not from kube-system pods, so once
NetworkPolicy is enforced the kube-system rule matches nothing there. Empty
while clusterDNSCIDR is unset, so the policies render as before. Context: the
root; include it with nindent 8 inside `with`.
*/}}
{{- define "patchy.clusterDNSEgress" -}}
{{- with .Values.clusterDNSCIDR -}}
# DNS at the node-local cluster resolver (clusterDNSCIDR).
- to:
      - ipBlock:
            cidr: {{ . }}
  ports:
      - protocol: UDP
        port: 53
      - protocol: TCP
        port: 53
{{- end -}}
{{- end }}

{{/*
Whether the egress credential broker deploys: exactly when a claude runner is
enabled anywhere (findings, evaluations or intents). There is deliberately no
separate enabled knob — claude runs proxy-only, so claude ⇒ broker, and a
fake-only dev/e2e values file renders none of it. intent-controller runs
brokered claude and nothing else, so enabling it enables claude whatever
agent.runners.claude says. Returns a non-empty string for yes.
*/}}
{{- define "patchy.brokerEnabled" -}}
{{- if or .Values.agent.runners.claude.enabled (and .Values.evaluationController.enabled .Values.evaluationController.runners.claude.enabled) .Values.intentController.enabled -}}
yes
{{- end -}}
{{- end }}

{{/*
Whether a harness runs anywhere (findings, evaluations or intents), by the
same rule as patchy.brokerEnabled. Evaluation and intent Jobs carry the same
harness label as finding Jobs, so the per-harness egress policies (Cilium,
GKE FQDN, Istio) render for a harness enabled on any fleet; hosts and
dnsPatterns still come from agent.runners.<id>. Intents run on claude only.
Takes (dict "root" $ "id" $id); returns a non-empty string for yes.
*/}}
{{- define "patchy.harnessEnabled" -}}
{{- $agent := index .root.Values.agent.runners .id | default dict -}}
{{- $eval := index .root.Values.evaluationController.runners .id | default dict -}}
{{- $intent := and .root.Values.intentController.enabled (eq .id "claude") -}}
{{- if or $agent.enabled (and .root.Values.evaluationController.enabled $eval.enabled) $intent -}}
yes
{{- end -}}
{{- end }}

{{/*
The egress broker's in-cluster base URL — what PATCHY_BROKER_URL is stamped
with and what the agent pods' gateway env points at.
*/}}
{{- define "patchy.brokerURL" -}}
http://{{ include "patchy.fullname" . }}-egress-broker.{{ .Release.Namespace }}.svc.cluster.local:8080
{{- end }}

{{/*
Stamp the brokered-claude configuration into a controller's ConfigMap data
dict: the broker URL and the PATCHY_CLAUDE_PROVIDER* keys the runnercfg flags
bind. Context: dict "root" $ "data" <dict>. The PATCHY_CLAUDE_SECRET* keys
are deliberately NOT stamped anymore — the claude model credential lives in
the broker, not the agent namespace.
*/}}
{{- define "patchy.claudeProviderData" -}}
{{- $p := .root.Values.agent.runners.claude.provider | default dict -}}
{{- $_ := set .data "PATCHY_BROKER_URL" (include "patchy.brokerURL" .root) -}}
{{- $_ := set .data "PATCHY_CLAUDE_PROVIDER" ($p.name | default "anthropic") -}}
{{- with $p.region -}}
{{- $_ := set $.data "PATCHY_CLAUDE_PROVIDER_REGION" . -}}
{{- end -}}
{{- with $p.regionPrefix -}}
{{- $_ := set $.data "PATCHY_CLAUDE_PROVIDER_REGION_PREFIX" . -}}
{{- end -}}
{{- with $p.projectID -}}
{{- $_ := set $.data "PATCHY_CLAUDE_PROVIDER_PROJECT_ID" . -}}
{{- end -}}
{{- with $p.modelMap -}}
{{- $_ := set $.data "PATCHY_CLAUDE_MODEL_MAP" (include "patchy.kvList" .) -}}
{{- end -}}
{{- with $p.env -}}
{{- $_ := set $.data "PATCHY_CLAUDE_PROVIDER_ENV" (include "patchy.kvList" .) -}}
{{- end -}}
{{- end }}

{{/*
The agent Jobs' default CPU and memory (agent.resources.default) as the four
PATCHY_AGENT_* keys every controller that launches agent Jobs binds
(investigation, remediation, intent and evaluation): set into .data, each only
when its quantity is set, so the default {} leaves every ConfigMap, and with it
every checksum/config, exactly as it was. Context: dict "root" $ "data" <dict>.
*/}}
{{- define "patchy.agentResourcesData" -}}
{{- $d := (.root.Values.agent.resources | default dict).default | default dict -}}
{{- range $side, $keys := dict "requests" (dict "cpu" "PATCHY_AGENT_CPU_REQUEST" "memory" "PATCHY_AGENT_MEMORY_REQUEST") "limits" (dict "cpu" "PATCHY_AGENT_CPU_LIMIT" "memory" "PATCHY_AGENT_MEMORY_LIMIT") -}}
{{- $q := index $d $side | default dict -}}
{{- range $name, $key := $keys -}}
{{- if hasKey $q $name -}}
{{- $_ := set $.data $key (index $q $name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Render a map as the comma-joined key=value wire form the runnercfg flags
parse (--claude-model-map, --claude-provider-env). Keys sort deterministically.
*/}}
{{- define "patchy.kvList" -}}
{{- $pairs := list -}}
{{- range $k, $v := . -}}
{{- $pairs = append $pairs (printf "%s=%s" $k $v) -}}
{{- end -}}
{{- join "," $pairs -}}
{{- end }}

{{/*
The resolved hostname-enforcement mode for the agent sandbox — one of
`none`, `cilium`, `gke` or `istio`. Every egress template keys off this, so
there is exactly one place that decides which CNI dialect a cluster gets.

Precedence:

  1. agent.networkPolicy.mode, when it is not "auto".
  2. The legacy booleans (agent.networkPolicy.{cilium,istio}.enabled), so a
     values file written before `mode` existed keeps its behaviour.
  3. "auto" — capability detection against the cluster's discovery document.

Helm fills .Capabilities.APIVersions from the API server whenever it renders
against a cluster: `helm install/upgrade`, and every helm-controller
reconcile. Off-cluster `helm template` sees only Helm's built-in list, so auto
resolves to "none" in CI — deliberately, since a rendering that cannot see the
cluster must not claim an enforcement it cannot verify. Pin `mode` when you
need a deterministic render.

Two things auto will never do:

  * Select `istio`. The CRDs being installed says nothing about whether the
    namespace is injection-labelled or whether istiod runs with native
    sidecars — and without native sidecars the agent Job never completes.
    Istio stays opt-in.
  * Select `cilium` on GKE. GKE Dataplane V2 IS Cilium, and it does publish
    cilium.io CRDs, but it has not honoured the CiliumNetworkPolicy CRD since
    1.21.5-gke.1300 and rejects every L7 rule — the `toFQDNs` and `rules.dns`
    blocks this chart's CNP is made of. Rendering one there yields a policy
    that silently enforces nothing, which is strictly worse than rendering
    none: it reads as protection in `kubectl get`. GKE's own
    FQDNNetworkPolicy is the equivalent, so on GKE auto picks `gke` when that
    CRD is present and falls back to `none` when it is not.
*/}}
{{- define "patchy.egressMode" -}}
{{- $np := .Values.agent.networkPolicy -}}
{{- $mode := $np.mode | default "auto" -}}
{{- if not (has $mode (list "auto" "none" "cilium" "gke" "istio")) -}}
{{- fail (printf "agent.networkPolicy.mode: %q is not one of auto, none, cilium, gke, istio" $mode) -}}
{{- end -}}
{{- if ne $mode "auto" -}}
{{- $mode -}}
{{- else if $np.cilium.enabled -}}
cilium
{{- else if $np.istio.enabled -}}
istio
{{- else if .Capabilities.APIVersions.Has "networking.gke.io/v1alpha1/FQDNNetworkPolicy" -}}
gke
{{- else if and (not (contains "gke" (toString .Capabilities.KubeVersion.GitVersion))) (.Capabilities.APIVersions.Has "cilium.io/v2/CiliumNetworkPolicy") -}}
cilium
{{- else -}}
none
{{- end -}}
{{- end }}

{{/*
Whether the base NetworkPolicy keeps its "TCP 443 to anywhere outside the
cluster" egress rule. Returns a non-empty string for yes.

This is the rule that makes a hostname allowlist meaningful or meaningless.
Network policies are ADDITIVE — both Cilium and GKE Dataplane V2 allow a
packet that matches ANY policy selecting the pod, and neither has a way for
one policy to subtract from another. So a CiliumNetworkPolicy or an
FQDNNetworkPolicy naming api.anthropic.com, rendered alongside a plain
NetworkPolicy that already allows 443 to 0.0.0.0/0, changes nothing at all:
the union is still "443 to anywhere".

`auto` therefore drops the broad rule exactly when a hostname mode is doing
the work (cilium, gke) and keeps it when nothing else would replace it (none,
istio — the Istio sidecar enforces on SNI in a different plane, and the L3
floor is still wanted underneath it). `always` keeps it regardless, which is
the honest setting while soaking a new mode; `never` drops it regardless.
*/}}
{{- define "patchy.broadEgress" -}}
{{- $broad := .Values.agent.networkPolicy.broadEgress | default "auto" -}}
{{- if not (has $broad (list "auto" "always" "never")) -}}
{{- fail (printf "agent.networkPolicy.broadEgress: %q is not one of auto, always, never" $broad) -}}
{{- end -}}
{{- if eq $broad "always" -}}
yes
{{- else if eq $broad "never" -}}
{{- else if has (include "patchy.egressMode" .) (list "none" "istio") -}}
yes
{{- end -}}
{{- end }}

{{/*
Render-time guards for agent.repositoryImages; called once from
configmap.yaml, which always renders. Only an enabled block is judged, so
flipping the kill switch off never fails a render. Every guard fails the
render rather than leaving a controller to crash-loop, or a sandbox to run
untrusted images with broad egress:

  * registries empty — source-controller refuses to start without an
    allowlist, and an empty one would admit nothing anyway.
  * ephemeralStorage empty — both job controllers refuse to start without
    the wall on disk a repository image can fill.
  * cosignPublicKey empty without allowUnsigned — source-controller refuses
    to start without a key unless unsigned images are explicitly allowed.
  * cosignPublicKey without its PEM armour — source-controller parses a set
    key at startup, allowUnsigned or not. A template cannot parse the DER
    inside, so a well-armoured but corrupt key still stops it; the armour
    catches the pasted-the-wrong-thing case.
  * pullSecretData without pullSecret — the rendered Secret needs the name
    both namespaces share.
  * broad agent egress (the design's decision 2): no agent NetworkPolicy at
    all, or a base policy that keeps "TCP 443 to anywhere", would let a
    hostile image reach a model API with a key of its own. A NOTES warning
    is invisible in CI; a failed render is not.

The formats of a registries entry and of ephemeralStorage are
values.schema.json patterns instead, so helm lint judges them too. Each
pattern admits only values the binary accepts at startup, pinned by
TestChartRegistryPatternIsSound (cmd/source-controller) and
TestChartEphemeralStoragePatternIsSound (internal/runnercfg).
*/}}
{{- define "patchy.repositoryImagesGuard" -}}
{{- $ri := .Values.agent.repositoryImages | default dict -}}
{{- if $ri.enabled -}}
{{- if not $ri.registries -}}
{{- fail "agent.repositoryImages.enabled requires agent.repositoryImages.registries: list the registry path prefixes a declared image must sit under, e.g. 123456789012.dkr.ecr.us-east-1.amazonaws.com/patchy/ or ghcr.io/my-org/agent-images/" -}}
{{- end -}}
{{- if not $ri.ephemeralStorage -}}
{{- fail "agent.repositoryImages.enabled requires agent.repositoryImages.ephemeralStorage, a quantity such as 8Gi: the ephemeral-storage request and limit that bounds the disk a declared image can fill" -}}
{{- end -}}
{{- if and (not $ri.cosignPublicKey) (not $ri.allowUnsigned) -}}
{{- fail "agent.repositoryImages.enabled requires agent.repositoryImages.cosignPublicKey, the PEM public key declared images must be cosign-signed with; set agent.repositoryImages.allowUnsigned: true to admit unsigned images instead" -}}
{{- end -}}
{{- if and $ri.cosignPublicKey (not (and (contains "-----BEGIN PUBLIC KEY-----" $ri.cosignPublicKey) (contains "-----END PUBLIC KEY-----" $ri.cosignPublicKey))) -}}
{{- fail "agent.repositoryImages.cosignPublicKey is not a PEM public key: set it to the whole cosign.pub that cosign generate-key-pair writes, -----BEGIN PUBLIC KEY----- through -----END PUBLIC KEY----- (source-controller refuses anything else at startup)" -}}
{{- end -}}
{{- if not .Values.agent.networkPolicy.create -}}
{{- fail "agent.repositoryImages.enabled requires the agent sandbox NetworkPolicy, but agent.networkPolicy.create is false: an agent pod would have unrestricted egress. Set agent.networkPolicy.create: true" -}}
{{- end -}}
{{- if include "patchy.broadEgress" . -}}
{{- fail (printf "agent.repositoryImages.enabled requires narrow agent egress, but agent.networkPolicy.broadEgress (%q) resolves to broad under agent.networkPolicy.mode %q: the base policy would allow TCP 443 to anywhere. Set agent.networkPolicy.broadEgress: never (brokered claude runners only), or use agent.networkPolicy.mode cilium or gke." (.Values.agent.networkPolicy.broadEgress | default "auto") (include "patchy.egressMode" .)) -}}
{{- end -}}
{{- if and $ri.pullSecretData (not $ri.pullSecret) -}}
{{- fail "agent.repositoryImages.pullSecretData requires agent.repositoryImages.pullSecret, the name of the Secret it renders into both the release namespace (source-controller's mount) and agent.namespace (the kubelet's pull)" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
The preview image prefix, <preview.imageRegistry>/<preview.imagePathPrefix>/:
the one string the slot admission policy (preview-admission.yaml) and the
preview-controller (PATCHY_PREVIEW_IMAGE_PREFIX) hold every preview image to,
and the path source-controller refuses agent images from, so none of them can
disagree. With the default path, patchy/previews, it is the prefix the chart
hard-coded before the path was configurable, so those renders are byte for
byte what they were. Context: the root. It fails the render when:

  * imagePathPrefix is empty, or not lowercase ECR path segments joined by
    "/" with no leading or trailing slash. values.schema.json says the same,
    but a render that skips schema validation still lands here.
  * agent.repositoryImages is enabled and one of its registries entries is
    this prefix, contains it or sits under it. A preview runtime image is
    built from an unreviewed same-repository pull request head, so it must
    never be admissible as an agent sandbox image, and an agent toolchain
    image must never be admissible as a preview. The comparison is on
    segment boundaries over patchy.registryPathKey, so no other spelling of
    the same ECR repository passes.
*/}}
{{- define "patchy.previewImagePrefix" -}}
{{- $p := .Values.preview -}}
{{- $path := $p.imagePathPrefix | default "" | toString -}}
{{- if not (regexMatch `^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$` $path) -}}
{{- fail (printf "preview.imagePathPrefix %q must be one or more lowercase registry path segments joined by '/', with no leading or trailing slash, such as patchy/previews (the default)" $path) -}}
{{- end -}}
{{- $prefix := printf "%s/%s/" $p.imageRegistry $path -}}
{{- $ri := .Values.agent.repositoryImages | default dict -}}
{{- if $ri.enabled -}}
{{- $previewKey := include "patchy.registryPathKey" $prefix -}}
{{- range $entry := $ri.registries | default list -}}
{{- $agentKey := include "patchy.registryPathKey" (printf "%s/" (trimSuffix "/" (toString $entry))) -}}
{{- if or (hasPrefix $agentKey $previewKey) (hasPrefix $previewKey $agentKey) -}}
{{- fail (printf "agent.repositoryImages.registries entry %q overlaps the preview image prefix %s (preview.imageRegistry/preview.imagePathPrefix): the two must be disjoint, neither equal to nor under the other, or an image built from an unreviewed pull request could run as an agent sandbox (and an agent toolchain image as a preview). Move the agent images, or set preview.imagePathPrefix to a path outside every registries entry" (toString $entry) $prefix) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $prefix -}}
{{- end }}

{{/*
patchy.registryPathKey: a "host/path/" registry prefix as the registry it is
pulled from sees it, for comparing two prefixes on segment boundaries.
Lowercased (source-controller lowercases allowlist entries and references), an
explicit :443 dropped (the same HTTPS endpoint), and ECR's dual-stack and
FIPS endpoint names (<account>.dkr-ecr.<region>.on.aws,
<account>.dkr.ecr-fips.<region>.amazonaws.com, ...) folded onto the plain
<account>.dkr.ecr.<region>.amazonaws.com, which serves the same repositories.
runnerimage.registryKey is the same fold on source-controller's side.
*/}}
{{- define "patchy.registryPathKey" -}}
{{- $key := lower . -}}
{{- $key = regexReplaceAll `^([^/]+):443/` $key "${1}/" -}}
{{- regexReplaceAll `^([0-9]{12})\.dkr[.-]ecr(-fips)?\.([a-z0-9-]+)\.(amazonaws\.com|on\.aws)/` $key "${1}.dkr.ecr.${3}.amazonaws.com/" -}}
{{- end }}
