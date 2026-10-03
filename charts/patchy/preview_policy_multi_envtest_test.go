// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The slice-3 admission rules for multi-component previews: one container
// per Pod; one Ingress rule of Prefix paths in the component path grammar,
// each to a preview- Service on port 80; and a Service annotation limited to
// its component's health-check path.
const multiIngressDenial = "one rule of at most 4 Prefix paths"

// componentPath is one Prefix path of a preview Ingress.
func componentPath(path, service string) networkingv1.HTTPIngressPath {
	pathType := networkingv1.PathTypePrefix
	return networkingv1.HTTPIngressPath{Path: path, PathType: &pathType,
		Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
			Name: service, Port: networkingv1.ServiceBackendPort{Number: 80},
		}}}
}

// twoComponentIngress is the Ingress a two-component Preview renders: /api
// first, then /, each to its own component's Service.
func twoComponentIngress(name string) *networkingv1.Ingress {
	i := ingress("patchy-preview-0", name, "alb-preview", previewHost)
	i.Annotations = map[string]string{"alb.ingress.kubernetes.io/healthcheck-path": "/healthz"}
	i.Spec.Rules[0].HTTP.Paths = []networkingv1.HTTPIngressPath{
		componentPath("/api", "preview-demo-1-api"), componentPath("/", "preview-demo-1"),
	}
	return i
}

func testMultiComponentAdmission(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	for _, tc := range []struct {
		name string
		obj  client.Object
		want string
	}{
		{"two-container pod", mutatePod(pod("patchy-preview-0", "two-containers", previewImage), func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "sidecar", Image: previewImage})
		}), "exactly one container"},
		{"two-container deployment", mutateDeployment(deployment("patchy-preview-0", "two-containers", previewImage),
			func(d *appsv1.Deployment) {
				d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers,
					corev1.Container{Name: "sidecar", Image: previewImage})
			}), "exactly one container"},
		{"two rules", mutateIngress(twoComponentIngress("two-rules"), func(i *networkingv1.Ingress) {
			i.Spec.Rules = append(i.Spec.Rules, i.Spec.Rules[0])
		}), multiIngressDenial},
		{"an implementation-specific path", mutateIngress(twoComponentIngress("implementation-specific"),
			func(i *networkingv1.Ingress) {
				other := networkingv1.PathTypeImplementationSpecific
				i.Spec.Rules[0].HTTP.Paths[0].PathType = &other
			}), multiIngressDenial},
		{"an exact path", mutateIngress(twoComponentIngress("exact"), func(i *networkingv1.Ingress) {
			exact := networkingv1.PathTypeExact
			i.Spec.Rules[0].HTTP.Paths[0].PathType = &exact
		}), multiIngressDenial},
		{"a path outside the grammar", mutateIngress(twoComponentIngress("uppercase"), func(i *networkingv1.Ingress) {
			i.Spec.Rules[0].HTTP.Paths[0].Path = "/API"
		}), multiIngressDenial},
		{"a regex path", mutateIngress(twoComponentIngress("regex"), func(i *networkingv1.Ingress) {
			i.Spec.Rules[0].HTTP.Paths[0].Path = "/api/*"
		}), multiIngressDenial},
		{"a foreign backend", mutateIngress(twoComponentIngress("foreign"), func(i *networkingv1.Ingress) {
			i.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name = "patchy-status-server"
		}), multiIngressDenial},
		{"a backend on port 8080", mutateIngress(twoComponentIngress("port"), func(i *networkingv1.Ingress) {
			i.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Number = 8080
		}), multiIngressDenial},
		{"five paths", mutateIngress(twoComponentIngress("five"), func(i *networkingv1.Ingress) {
			for _, p := range []string{"/a", "/b", "/c"} {
				i.Spec.Rules[0].HTTP.Paths = append(i.Spec.Rules[0].HTTP.Paths, componentPath(p, "preview-demo-1-x"))
			}
		}), multiIngressDenial},
		{"no paths", mutateIngress(twoComponentIngress("none"), func(i *networkingv1.Ingress) {
			i.Spec.Rules[0].HTTP.Paths = nil
		}), ""},
		{"the placeholder to a preview Service", mutateIngress(
			ingress("patchy-preview-0", "patchy-preview-placeholder", "alb-preview", previewHost),
			func(i *networkingv1.Ingress) {
				i.Annotations = placeholderHelmAnnotations()
				i.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name = "preview-demo-1"
			}), multiIngressDenial},
		{"a service with another annotation", mutateService(
			service("patchy-preview-0", "preview-demo-1-api", corev1.ServiceTypeClusterIP), func(s *corev1.Service) {
				s.Annotations = map[string]string{"alb.ingress.kubernetes.io/target-type": "instance"}
			}), "restricted to a safe healthcheck path"},
		{"a service health check with a query", mutateService(
			service("patchy-preview-0", "preview-demo-1-api", corev1.ServiceTypeClusterIP), func(s *corev1.Service) {
				s.Annotations = map[string]string{"alb.ingress.kubernetes.io/healthcheck-path": "/healthz?token=1"}
			}), "restricted to a safe healthcheck path"},
	} {
		t.Run("multi-component denies "+tc.name, func(t *testing.T) {
			err := admin.Create(ctx, tc.obj, client.DryRunAll)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want rejection containing %q, got %v", tc.want, err)
			}
		})
	}
	for _, obj := range []client.Object{
		twoComponentIngress("two-components"),
		mutateIngress(twoComponentIngress("four-components"), func(i *networkingv1.Ingress) {
			i.Spec.Rules[0].HTTP.Paths = append(i.Spec.Rules[0].HTTP.Paths,
				componentPath("/api/v1", "preview-demo-1-v1"), componentPath("/docs", "preview-demo-1-docs"))
		}),
		mutateService(service("patchy-preview-0", "preview-demo-1-api", corev1.ServiceTypeClusterIP),
			func(s *corev1.Service) {
				s.Annotations = map[string]string{"alb.ingress.kubernetes.io/healthcheck-path": "/api/healthz"}
			}),
	} {
		if err := admin.Create(ctx, obj, client.DryRunAll); err != nil {
			t.Errorf("valid %T %s refused: %v", obj, obj.GetName(), err)
		}
	}
}

// testPlaceholderStaysAdmissible: the chart's kept placeholder Service and
// Ingress are admitted on create and on a later update (Helm patches them on
// every upgrade), so a tightened policy can never wedge a release the way a
// retained older policy once did.
func testPlaceholderStaysAdmissible(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	svc := mutateService(service("patchy-preview-0", "patchy-preview-placeholder", corev1.ServiceTypeClusterIP),
		func(s *corev1.Service) { s.Annotations = placeholderHelmAnnotations() })
	ing := mutateIngress(ingress("patchy-preview-0", "patchy-preview-placeholder", "alb-preview", previewHost),
		func(i *networkingv1.Ingress) { i.Annotations = placeholderHelmAnnotations() })
	for _, obj := range []client.Object{svc, ing} {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("placeholder %T refused on create: %v", obj, err)
		}
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels["helm.sh/chart"] = "patchy-0.13.0"
		obj.SetLabels(labels)
		if err := admin.Update(ctx, obj); err != nil {
			t.Errorf("placeholder %T refused on update: %v", obj, err)
		}
	}
}
