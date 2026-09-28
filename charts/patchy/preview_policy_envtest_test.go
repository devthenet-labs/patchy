// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/bitwise-media-group/patchy/internal/kube"
)

const (
	previewRegistry = "377946145366.dkr.ecr.us-east-1.amazonaws.com"
	previewHost     = "demo-1.preview.patchy.devthe.net"
	previewSHA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	previewImage    = previewRegistry + "/patchy/previews/demo:sha-" + previewSHA
)

// TestPreviewAdmissionAgainstAPIServer applies the rendered chart policies,
// not a parallel test copy. envtest has no network dataplane: the isolation
// policy's packets are checked by the later live slot-pod probe instead.
func TestPreviewAdmissionAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	admin := startPreviewPolicyEnv(t)
	testPreviewDenials(t, admin)
	testPreviewAllowed(t, admin)
}

func startPreviewPolicyEnv(t *testing.T) client.Client {
	t.Helper()
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	admin, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := t.Context()
	for _, name := range []string{"patchy-preview-0", "patchy-preview-1", "ordinary"} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: map[string]string{"kubernetes.io/metadata.name": name},
		}}); err != nil {
			t.Fatalf("namespace %s: %v", name, err)
		}
	}
	installPreviewPolicy(t, admin)
	waitForPreviewPolicy(t, admin)
	return admin
}

func installPreviewPolicy(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	output, err := exec.CommandContext(ctx, "helm", "template", "patchy", ".", "--namespace", "patchy",
		"-f", "../../hack/testdata/chart-render/preview-foundation.yaml").Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(output)), 4096)
	var policies, bindings []*unstructured.Unstructured
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(u); err != nil {
			break
		}
		switch u.GetKind() {
		case "ValidatingAdmissionPolicy":
			if strings.HasPrefix(u.GetName(), "patchy-preview-") {
				policies = append(policies, u)
			}
		case "ValidatingAdmissionPolicyBinding":
			if strings.HasPrefix(u.GetName(), "patchy-preview-") {
				bindings = append(bindings, u)
			}
		}
	}
	if len(policies) != 4 || len(bindings) != 8 {
		t.Fatalf("rendered %d policies, %d bindings; want 4 policies and 8 per-slot bindings", len(policies), len(bindings))
	}
	for _, u := range append(policies, bindings...) {
		if err := admin.Create(ctx, u); err != nil {
			t.Fatalf("install %s %s: %v", u.GetKind(), u.GetName(), err)
		}
	}
}

func waitForPreviewPolicy(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	// Bindings propagate asynchronously. Wait for a rejection that names our
	// policy, so a fast API server cannot produce a false successful test.
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := admin.Create(ctx, pod("patchy-preview-0", "probe", "nginx:latest"), client.DryRunAll)
		if err != nil && strings.Contains(err.Error(), "only immutable patchy preview ECR images") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("preview policy did not enforce before deadline: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func testPreviewDenials(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	for _, tc := range []struct {
		name string
		obj  client.Object
		want string
	}{
		{"slot image", pod("patchy-preview-0", "wrong-image", "nginx:latest"), "only immutable"},
		{"second slot image", pod("patchy-preview-1", "wrong-image", "nginx:latest"), "only immutable"},
		{"short SHA", pod("patchy-preview-0", "short-sha",
			previewRegistry+"/patchy/previews/demo:sha-aaaa"), "only immutable"},
		{"app env image", pod("patchy-preview-0", "app-env",
			previewRegistry+"/patchy/app-envs/demo:sha-"+previewSHA), "only immutable"},
		{"slot token", mutatePod(pod("patchy-preview-0", "token", previewImage), func(p *corev1.Pod) {
			p.Spec.AutomountServiceAccountToken = nil
		}), "API token mounting"},
		{"nondefault service account", mutatePod(
			pod("patchy-preview-0", "service-account", previewImage), func(p *corev1.Pod) {
				p.Spec.ServiceAccountName = "admin"
			}), "default ServiceAccount"},
		{"slot init", mutatePod(pod("patchy-preview-0", "init", previewImage), func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "init", Image: previewImage}}
		}), "no init"},
		{"slot volume", mutatePod(pod("patchy-preview-0", "volume", previewImage), func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "bad", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/tmp"},
			}}}
		}), "emptyDir"},
		{"deployment image", deployment("patchy-preview-0", "wrong-deployment", "nginx:latest"), "only immutable"},
		{"external service", service("patchy-preview-0", "external", corev1.ServiceTypeLoadBalancer), "internal ClusterIP"},
		{"external IP", mutateService(
			service("patchy-preview-0", "external-ip", corev1.ServiceTypeClusterIP), func(s *corev1.Service) {
				s.Spec.ExternalIPs = []string{"203.0.113.1"}
			}), "internal ClusterIP"},
		{"default class", ingress("patchy-preview-0", "default-class", "alb", previewHost), "alb-preview"},
		{"omitted class", mutateIngress(
			ingress("patchy-preview-0", "omitted-class", "alb", previewHost), func(i *networkingv1.Ingress) {
				i.Spec.IngressClassName = nil
			}), "alb-preview"},
		{"other host", ingress("patchy-preview-0", "other-host", "alb-preview", "patchy.devthe.net"), "single-label"},
		{"per-Ingress TLS", mutateIngress(
			ingress("patchy-preview-0", "tls", "alb-preview", previewHost), func(i *networkingv1.Ingress) {
				i.Spec.TLS = []networkingv1.IngressTLS{{Hosts: []string{previewHost}, SecretName: "unsafe"}}
			}), "class-level TLS"},
		{"unsafe annotation", mutateIngress(
			ingress("patchy-preview-0", "unsafe-annotation", "alb-preview", previewHost), func(i *networkingv1.Ingress) {
				i.Annotations = map[string]string{"alb.ingress.kubernetes.io/security-groups": "bypass"}
			}), "annotations are restricted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := admin.Create(ctx, tc.obj, client.DryRunAll)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want rejection containing %q, got %v", tc.want, err)
			}
		})
	}
	valid := deployment("patchy-preview-0", "update-probe", previewImage)
	if err := admin.Create(ctx, valid); err != nil {
		t.Fatalf("create valid Deployment for update test: %v", err)
	}
	valid.Spec.Template.Spec.Containers[0].Image = "nginx:latest"
	if err := admin.Update(ctx, valid, client.DryRunAll); err == nil || !strings.Contains(err.Error(), "only immutable") {
		t.Fatalf("unsafe Deployment update was not denied: %v", err)
	}
	p := pod("patchy-preview-0", "ephemeral-probe", previewImage)
	if err := admin.Create(ctx, p); err != nil {
		t.Fatalf("create valid Pod for ephemeral-container test: %v", err)
	}
	p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
		Name: "debug", Image: "nginx:latest",
	}}}
	if err := admin.SubResource("ephemeralcontainers").Update(ctx, p, client.DryRunAll); err == nil ||
		!strings.Contains(err.Error(), "no init or ephemeral containers") {
		t.Fatalf("unsafe ephemeral-container update was not denied: %v", err)
	}
}

func testPreviewAllowed(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	for _, obj := range []client.Object{
		pod("patchy-preview-0", "allowed-pod", previewImage),
		pod("patchy-preview-1", "allowed-pod-second-slot", previewImage),
		deployment("patchy-preview-0", "allowed-deployment", previewImage),
		service("patchy-preview-0", "allowed-service", corev1.ServiceTypeClusterIP),
		ingress("patchy-preview-0", "allowed-ingress", "alb-preview", previewHost),
		// A normal workload in an unrelated namespace is unaffected by the
		// exact-name binding; an ordinary unapproved image remains valid there.
		pod("ordinary", "ordinary-pod", "nginx:latest"),
		ingress("ordinary", "ordinary-ingress", "alb", "ordinary.example.com"),
	} {
		if err := admin.Create(ctx, obj, client.DryRunAll); err != nil {
			t.Errorf("valid %T %s/%s refused: %v", obj, obj.GetNamespace(), obj.GetName(), err)
		}
	}
}

func pod(ns, name, image string) *corev1.Pod {
	no := false
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: corev1.PodSpec{
		ServiceAccountName: "default", AutomountServiceAccountToken: &no,
		Containers: []corev1.Container{{Name: "app", Image: image}},
	}}
}

func mutatePod(p *corev1.Pod, f func(*corev1.Pod)) *corev1.Pod { f(p); return p }

func deployment(ns, name, image string) *appsv1.Deployment {
	p := pod(ns, name, image)
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}}, Spec: p.Spec},
	}}
}

func service(ns, name string, kind corev1.ServiceType) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: corev1.ServiceSpec{
		Type: kind, Ports: []corev1.ServicePort{{Port: 80}},
	}}
}

func mutateService(s *corev1.Service, f func(*corev1.Service)) *corev1.Service { f(s); return s }

func ingress(ns, name, class, host string) *networkingv1.Ingress {
	path := networkingv1.PathTypePrefix
	backend := networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
		Name: "demo", Port: networkingv1.ServiceBackendPort{Number: 80},
	}}
	rule := networkingv1.IngressRule{
		Host: host,
		IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
			Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &path, Backend: backend}},
		}},
	}
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       networkingv1.IngressSpec{IngressClassName: &class, Rules: []networkingv1.IngressRule{rule}},
	}
}

func mutateIngress(i *networkingv1.Ingress, f func(*networkingv1.Ingress)) *networkingv1.Ingress {
	f(i)
	return i
}
