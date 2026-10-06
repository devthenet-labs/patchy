// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

const (
	labelPreview    = "patchy.bitwisemedia.uk/preview"
	labelPreviewUID = "patchy.bitwisemedia.uk/preview-uid"
	// labelComponent names the component a multi-component Preview's
	// Deployment, Service and Pods belong to; their selectors carry it, so
	// no Service selects a sibling's Pods.
	labelComponent = "patchy.bitwisemedia.uk/preview-component"
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedBy      = "patchy-preview-controller"
	finalizer      = "patchy.bitwisemedia.uk/preview-cleanup"
	// annotationHealthcheck is the one annotation a preview Service or
	// Ingress carries, the only one the slot admission policy admits there.
	annotationHealthcheck = "alb.ingress.kubernetes.io/healthcheck-path"
	// maxObjectName bounds a rendered name: a Service name is a DNS label.
	maxObjectName = 63
)

var (
	shaPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	pathPattern = regexp.MustCompile(`^/[a-zA-Z0-9/_-]*$`)
	namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// routePattern is a component's route path, the grammar of the Project's
	// repositories[].preview.path and the slot admission policy's Ingress
	// paths.
	routePattern = regexp.MustCompile(`^/([a-z0-9-]+(/[a-z0-9-]+)*)?$`)
	// prefixPattern is the shape of the operator's image prefix
	// (<preview.imageRegistry>/<preview.imagePathPrefix>/ in the chart): a
	// lowercase registry host with an optional port, one or more lowercase
	// repository path segments in ECR's grammar, and the trailing slash an
	// image repository continues from. The prefix plus one namePattern leaf
	// always fits the CRD's imageRepository pattern, and the slot admission
	// policy's leaf ([a-z0-9-]+) admits every namePattern leaf.
	prefixPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)+/$`)
)

func ptr[T any](value T) *T { return &value }

// Settings are fixed operator values, never taken from a Preview spec or an
// agent. SlotCount is the only global concurrency/spend bound.
type Settings struct {
	Namespace      string
	SlotCount      int
	ImagePrefix    string // exact <registry>/<path>/ every image repository is one leaf under
	HostSuffix     string
	NodePool       string
	NodeClass      string
	TaintKey       string
	RolloutTimeout time.Duration
	PollInterval   time.Duration
	MaxRetries     int32
	// TargetHealth makes Ready mean the load balancer's target is healthy.
	// The load balancer controller injects a target-health readiness gate
	// into a slot Pod (its namespace opts in by label) only when the Pod is
	// created after the target group binding exists, which follows the
	// Ingress. So the Ingress comes first and stays across redeploys and
	// retries (one that lacks a newly added component's backend is replaced
	// instead: only a new Ingress's address proves that backend's binding
	// exists), Deployments wait until the load balancer has admitted it, and
	// a component is Ready only once its Pod carries a readiness gate and
	// every gate is True. The controller renders no gate of its own, so the
	// one a slot Pod carries is the load balancer's (its condition type,
	// target-health.elbv2.k8s.aws/<binding> from the upstream controller, is
	// not relied on, since EKS Auto Mode's is not documented). Off, the
	// Ingress is created only once every component is
	// Ready, as in slice 2, and the host can answer 404 for a few seconds
	// after Ready while the target registers.
	TargetHealth bool
}

// Validate refuses settings the controller must not run with. The image
// prefix is checked for its shape only, never for a fixed path: which path
// is the operator's (preview.imagePathPrefix), and the chart keeps it
// disjoint from the agent-image allowlist.
func (s Settings) Validate() error {
	if !prefixPattern.MatchString(s.ImagePrefix) {
		return fmt.Errorf("invalid preview-controller settings: image prefix %q is not <registry>/<path>/ "+
			"(a lowercase registry host, one or more lowercase path segments and a trailing slash)", s.ImagePrefix)
	}
	if s.Namespace == "" || s.SlotCount < 1 || s.SlotCount > 4 ||
		s.HostSuffix == "" || s.NodePool == "" || s.NodeClass == "" || s.TaintKey == "" ||
		s.RolloutTimeout <= 0 || s.PollInterval <= 0 || s.MaxRetries < 1 || s.MaxRetries > 3 {
		return fmt.Errorf("invalid preview-controller settings")
	}
	return nil
}

func (s Settings) slotName(n int32) string { return fmt.Sprintf("patchy-preview-%d", n) }

// resourceName names the Preview's Ingress, and its first component's
// Deployment and Service: preview-<p>, the name every single-component Preview
// has had since slice 2.
func resourceName(p *v1alpha1.Preview) string      { return "preview-" + p.Name }
func (s Settings) host(p *v1alpha1.Preview) string { return p.Spec.HostLabel + "." + s.HostSuffix }
func image(c v1alpha1.PreviewComponent) string     { return c.ImageRepository + ":sha-" + c.Revision }

// componentName names component i's Deployment and Service. The first keeps
// preview-<p>, so a single-component Preview renders exactly what it always
// did: an existing preview is not re-rendered across an upgrade, and a
// rolled-back controller, which knows only that name, can still clean it up.
// Each further component is preview-<p>-<c>.
func componentName(p *v1alpha1.Preview, i int) string {
	if i == 0 {
		return resourceName(p)
	}
	return resourceName(p) + "-" + p.Spec.Components[i].Name
}

// labelsFor are the labels every object of the Preview carries; cleanup,
// pruning and the orphan sweep find its objects by them.
func labelsFor(p *v1alpha1.Preview) map[string]string {
	return map[string]string{
		labelPreview: p.Name, labelPreviewUID: string(p.UID), labelManagedBy: managedBy,
	}
}

// componentLabels label component i's Deployment and Service and select its
// Pods. A single-component Preview keeps labelsFor exactly, byte-identical to
// slice 2 (a Deployment's selector is immutable, so adding a label would fail
// the update rather than re-render it). With several components each adds its
// own name, so no component's Service selects a sibling's Pods.
func componentLabels(p *v1alpha1.Preview, i int) map[string]string {
	l := labelsFor(p)
	if len(p.Spec.Components) > 1 {
		l[labelComponent] = p.Spec.Components[i].Name
	}
	return l
}

func (s Settings) validatePreview(p *v1alpha1.Preview) error {
	n := len(p.Spec.Components)
	if p.Spec.IntentRef.Name != p.Name || p.Spec.IntentRef.UID == "" ||
		p.Spec.HostLabel != p.Name || !namePattern.MatchString(p.Spec.HostLabel) ||
		n < 1 || n > v1alpha1.MaxPreviewComponents || p.Spec.TTL.Duration < time.Hour ||
		p.Spec.TTL.Duration > 72*time.Hour {
		return fmt.Errorf("invalid Preview identity, components or TTL")
	}
	names, paths := map[string]bool{}, map[string]bool{}
	for i, c := range p.Spec.Components {
		path := v1alpha1.PreviewComponentPath(c)
		if names[c.Name] || paths[path] || !s.validComponent(p, i) {
			return fmt.Errorf("invalid Preview component")
		}
		names[c.Name], paths[path] = true, true
	}
	return nil
}

// validComponent checks component i on its own: a DNS-label name whose
// rendered object name fits a Service name, a route path in the component
// grammar, an image repository exactly one DNS-label leaf under the
// operator's prefix (no other registry, no nested path, no tag), a full SHA,
// and a safe port and readiness path.
func (s Settings) validComponent(p *v1alpha1.Preview, i int) bool {
	c := p.Spec.Components[i]
	return namePattern.MatchString(c.Name) && routePattern.MatchString(v1alpha1.PreviewComponentPath(c)) &&
		len(componentName(p, i)) <= maxObjectName && strings.HasPrefix(c.ImageRepository, s.ImagePrefix) &&
		namePattern.MatchString(strings.TrimPrefix(c.ImageRepository, s.ImagePrefix)) &&
		shaPattern.MatchString(c.Revision) && c.Port >= 1 && c.Port <= 65535 &&
		pathPattern.MatchString(c.ReadinessPath) && len(c.ReadinessPath) <= 128
}

func (s Settings) deployment(p *v1alpha1.Preview, i int, slot int32) *appsv1.Deployment {
	c := p.Spec.Components[i]
	labels := componentLabels(p, i)
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: componentName(p, i), Namespace: s.slotName(slot), Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			// A new revision's Pod starts beside the serving one, which stops
			// only once the new one is available: Ready, so with target health
			// once its load balancer target is healthy. A redeploy, often of
			// a pull request head whose image the app's CI is still
			// publishing, never takes the host down. The slot quota holds one
			// surge Pod per component.
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge: ptr(intstr.FromInt32(1)), MaxUnavailable: ptr(intstr.FromInt32(0)),
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: "default", AutomountServiceAccountToken: ptr(false),
					NodeSelector: map[string]string{
						"karpenter.sh/nodepool": s.NodePool, "eks.amazonaws.com/nodeclass": s.NodeClass,
					},
					Tolerations: []corev1.Toleration{{
						Key: s.TaintKey, Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoExecute,
					}},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr(true), RunAsUser: ptr(int64(65532)), RunAsGroup: ptr(int64(65532)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name: c.Name, Image: image(c), ImagePullPolicy: corev1.PullIfNotPresent,
						Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: c.Port, Protocol: corev1.ProtocolTCP}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
								Path: c.ReadinessPath, Port: intstr.FromString("http"), Scheme: corev1.URISchemeHTTP,
							}},
							PeriodSeconds: 5, FailureThreshold: 3,
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("25m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("250m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true),
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
}

// service fronts component i. With several components each Service carries
// its component's health-check path, which the load balancer prefers over the
// Ingress-level one for that Service's target group; a single-component
// Preview's Service carries none, as in slice 2.
func (s Settings) service(p *v1alpha1.Preview, i int, slot int32) *corev1.Service {
	var annotations map[string]string
	if len(p.Spec.Components) > 1 {
		annotations = map[string]string{annotationHealthcheck: p.Spec.Components[i].ReadinessPath}
	}
	labels := componentLabels(p, i)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: componentName(p, i), Namespace: s.slotName(slot), Labels: labels,
			Annotations: annotations},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP, Selector: componentLabels(p, i),
			Ports: []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromString("http"),
				Protocol: corev1.ProtocolTCP}},
		},
	}
}

// rootComponent is the component whose health-check path the Ingress carries:
// the one served at "/", else the first.
func rootComponent(p *v1alpha1.Preview) int {
	return max(0, slices.IndexFunc(p.Spec.Components, func(c v1alpha1.PreviewComponent) bool {
		return v1alpha1.PreviewComponentPath(c) == "/"
	}))
}

// ingress is the Preview's one host: a single rule with a Prefix path per
// component, longest first, so the load balancer tries a sibling's deeper
// prefix before the root's catch-all whether or not it orders rules itself.
func (s Settings) ingress(p *v1alpha1.Preview, slot int32) *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	className := "alb-preview"
	paths := make([]networkingv1.HTTPIngressPath, 0, len(p.Spec.Components))
	for i, c := range p.Spec.Components {
		paths = append(paths, networkingv1.HTTPIngressPath{
			Path: v1alpha1.PreviewComponentPath(c), PathType: &pathType,
			Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
				Name: componentName(p, i), Port: networkingv1.ServiceBackendPort{Number: 80},
			}},
		})
	}
	slices.SortStableFunc(paths, func(a, b networkingv1.HTTPIngressPath) int {
		return cmp.Or(cmp.Compare(len(b.Path), len(a.Path)), strings.Compare(a.Path, b.Path))
	})
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: resourceName(p), Namespace: s.slotName(slot), Labels: labelsFor(p),
			Annotations: map[string]string{
				annotationHealthcheck: p.Spec.Components[rootComponent(p)].ReadinessPath,
			},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &className,
			Rules: []networkingv1.IngressRule{{
				Host: s.host(p), IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{Paths: paths},
				},
			}},
		},
	}
}
