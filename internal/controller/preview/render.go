// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"fmt"
	"regexp"
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
	labelManagedBy  = "app.kubernetes.io/managed-by"
	managedBy       = "patchy-preview-controller"
	finalizer       = "patchy.bitwisemedia.uk/preview-cleanup"
)

var (
	shaPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	pathPattern = regexp.MustCompile(`^/[a-zA-Z0-9/_-]*$`)
	namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

func ptr[T any](value T) *T { return &value }

// Settings are fixed operator values, never taken from a Preview spec or an
// agent. SlotCount is the only global concurrency/spend bound.
type Settings struct {
	Namespace      string
	SlotCount      int
	ImagePrefix    string // <registry>/patchy/previews/
	HostSuffix     string
	NodePool       string
	NodeClass      string
	TaintKey       string
	RolloutTimeout time.Duration
	PollInterval   time.Duration
	MaxRetries     int32
}

func (s Settings) Validate() error {
	if s.Namespace == "" || s.SlotCount < 1 || s.SlotCount > 4 ||
		s.ImagePrefix == "" || !strings.HasSuffix(s.ImagePrefix, "/patchy/previews/") ||
		s.HostSuffix == "" || s.NodePool == "" || s.NodeClass == "" || s.TaintKey == "" ||
		s.RolloutTimeout <= 0 || s.PollInterval <= 0 || s.MaxRetries < 1 || s.MaxRetries > 3 {
		return fmt.Errorf("invalid preview-controller settings")
	}
	return nil
}

func (s Settings) slotName(n int32) string         { return fmt.Sprintf("patchy-preview-%d", n) }
func resourceName(p *v1alpha1.Preview) string      { return "preview-" + p.Name }
func (s Settings) host(p *v1alpha1.Preview) string { return p.Spec.HostLabel + "." + s.HostSuffix }
func image(c v1alpha1.PreviewComponent) string     { return c.ImageRepository + ":sha-" + c.Revision }

func labelsFor(p *v1alpha1.Preview) map[string]string {
	return map[string]string{
		labelPreview: p.Name, labelPreviewUID: string(p.UID), labelManagedBy: managedBy,
	}
}

func (s Settings) validatePreview(p *v1alpha1.Preview) error {
	if p.Spec.IntentRef.Name != p.Name || p.Spec.IntentRef.UID == "" ||
		p.Spec.HostLabel != p.Name || !namePattern.MatchString(p.Spec.HostLabel) ||
		len(p.Spec.Components) != 1 || p.Spec.TTL.Duration < time.Hour ||
		p.Spec.TTL.Duration > 72*time.Hour {
		return fmt.Errorf("invalid Preview identity, components or TTL")
	}
	c := p.Spec.Components[0]
	if !namePattern.MatchString(c.Name) || !strings.HasPrefix(c.ImageRepository, s.ImagePrefix) ||
		!namePattern.MatchString(strings.TrimPrefix(c.ImageRepository, s.ImagePrefix)) ||
		!shaPattern.MatchString(c.Revision) || c.Port < 1 || c.Port > 65535 ||
		!pathPattern.MatchString(c.ReadinessPath) || len(c.ReadinessPath) > 128 {
		return fmt.Errorf("invalid Preview component")
	}
	return nil
}

func (s Settings) deployment(p *v1alpha1.Preview, slot int32) *appsv1.Deployment {
	c := p.Spec.Components[0]
	labels := labelsFor(p)
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: resourceName(p), Namespace: s.slotName(slot), Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
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

func (s Settings) service(p *v1alpha1.Preview, slot int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: resourceName(p), Namespace: s.slotName(slot), Labels: labelsFor(p)},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP, Selector: labelsFor(p),
			Ports: []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromString("http"),
				Protocol: corev1.ProtocolTCP}},
		},
	}
}

func (s Settings) ingress(p *v1alpha1.Preview, slot int32) *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	className := "alb-preview"
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: resourceName(p), Namespace: s.slotName(slot), Labels: labelsFor(p),
			Annotations: map[string]string{"alb.ingress.kubernetes.io/healthcheck-path": p.Spec.Components[0].ReadinessPath},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &className,
			Rules: []networkingv1.IngressRule{{
				Host: s.host(p), IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
						Path: "/", PathType: &pathType,
						Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
							Name: resourceName(p), Port: networkingv1.ServiceBackendPort{Number: 80},
						}},
					}}},
				},
			}},
		},
	}
}
