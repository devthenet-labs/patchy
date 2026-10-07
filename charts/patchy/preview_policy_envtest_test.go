// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"fmt"
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
	previewRegistry = "111122223333.dkr.ecr.us-east-1.amazonaws.com"
	previewHost     = "demo-1.preview.patchy.devthe.net"
	previewSHA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	previewImage    = previewRegistry + "/patchy/previews/demo:sha-" + previewSHA
	previewTaintKey = "patchy.devthe.net/preview-only"
)

// TestPreviewAdmissionAgainstAPIServer applies the rendered chart policies,
// not a parallel test copy. envtest has no network dataplane: the isolation
// policy's packets are checked by the later live slot-pod probe instead.
func TestPreviewAdmissionAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	env, admin := startPreviewPolicyEnv(t)
	testPreviewDenials(t, admin)
	testPreviewAllowed(t, admin)
	testMultiComponentAdmission(t, admin)
	testPlaceholderStaysAdmissible(t, admin)
	testSecretRefs(t, admin)
	testLegacyObjectsCanStillBeDeleted(t, admin)
	testIsolationProbeService(t, env)
}

// startPreviewPolicyEnv starts an API server holding the slot namespaces and
// the chart's preview policies, rendered from the preview fixture plus
// helmArgs, and waits until every policy enforces.
func startPreviewPolicyEnv(t *testing.T, helmArgs ...string) (*envtest.Environment, client.Client) {
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
	for _, name := range []string{"patchy-preview-0", "patchy-preview-1", "patchy-preview-2", "ordinary"} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: map[string]string{"kubernetes.io/metadata.name": name},
		}}); err != nil {
			t.Fatalf("namespace %s: %v", name, err)
		}
	}
	// A scheduler-bound Pod already exists when the policy is installed. Its
	// later metadata updates must not be mistaken for direct node placement.
	bound := pod("patchy-preview-0", "scheduler-bound", previewImage)
	bound.Spec.NodeName = "i-0123456789abcdef0"
	if err := admin.Create(ctx, bound); err != nil {
		t.Fatalf("create pre-policy scheduled Pod: %v", err)
	}
	outsideBound := pod("ordinary", "scheduler-bound", "nginx:latest")
	outsideBound.Spec.NodeName = "i-0123456789abcdef0"
	if err := admin.Create(ctx, outsideBound); err != nil {
		t.Fatalf("create pre-policy ordinary scheduled Pod: %v", err)
	}
	createLegacySlotObjects(t, admin)
	installPreviewPolicy(t, admin, helmArgs...)
	// The image the rendered policies admit: the secret-refs probe must fail
	// on its Secret reference alone, not on the image rule as well.
	image := previewImage
	for _, arg := range helmArgs {
		if prefix, ok := strings.CutPrefix(arg, "preview.imagePathPrefix="); ok {
			image = previewRegistry + "/" + prefix + "/demo:sha-" + previewSHA
		}
	}
	waitForPreviewPolicy(t, admin, image)
	return env, admin
}

func installPreviewPolicy(t *testing.T, admin client.Client, helmArgs ...string) {
	t.Helper()
	ctx := t.Context()
	args := append([]string{"template", "patchy", ".", "--namespace", "patchy",
		"-f", "../../hack/testdata/chart-render/preview-foundation.yaml"}, helmArgs...)
	output, err := exec.CommandContext(ctx, "helm", args...).Output()
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
	if len(policies) != 8 || len(bindings) != 16 {
		t.Fatalf("rendered %d policies, %d bindings; want 8 policies and 16 bindings", len(policies), len(bindings))
	}
	for _, u := range append(policies, bindings...) {
		if err := admin.Create(ctx, u); err != nil {
			t.Fatalf("install %s %s: %v", u.GetKind(), u.GetName(), err)
		}
	}
}

func waitForPreviewPolicy(t *testing.T, admin client.Client, image string) {
	t.Helper()
	ctx := t.Context()
	// Bindings propagate independently. A Pod rejection does not prove that
	// Deployment, Service or Ingress policy is active yet, so wait for one
	// rejection from every rendered policy before testing the admission matrix.
	outsidePod := pod("ordinary", "probe", "nginx:latest")
	outsidePod.Spec.Tolerations = previewTolerations()
	outsideDeployment := deployment("ordinary", "probe", "nginx:latest")
	outsideDeployment.Spec.Template.Spec.Tolerations = previewTolerations()
	probes := []struct {
		name string
		obj  client.Object
		want string
	}{
		{"slot pod", pod("patchy-preview-0", "probe", "nginx:latest"), "only immutable patchy preview ECR images"},
		{"slot deployment", deployment("patchy-preview-0", "probe", "nginx:latest"), "only immutable"},
		{"outside pod", outsidePod, "preview-only toleration"},
		{"outside deployment", outsideDeployment, "preview-only toleration"},
		{"slot service", service("patchy-preview-0", "probe", corev1.ServiceTypeLoadBalancer), "internal ClusterIP"},
		{"slot ingress", ingress("patchy-preview-0", "probe", "alb", previewHost), "alb-preview"},
		{"outside ingress", ingress("ordinary", "probe", "alb-preview", previewHost),
			"reserved for the exact preview slot"},
		{"slot secret env", withSecretEnv(pod("patchy-preview-0", "probe", image)), "may not read a Secret"},
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var notReady []string
		for _, probe := range probes {
			// Admission/defaulting can mutate even dry-run objects (notably a
			// Service's clusterIPs). Each probe must start from a fresh object.
			err := admin.Create(ctx, probe.obj.DeepCopyObject().(client.Object), client.DryRunAll)
			if err == nil || !strings.Contains(err.Error(), probe.want) {
				notReady = append(notReady, fmt.Sprintf("%s: %v", probe.name, err))
			}
		}
		if len(notReady) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("preview policies did not enforce before deadline: %v", notReady)
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
		{"retired slot toleration", pod("patchy-preview-2", "retired", previewImage), "preview-only toleration"},
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
		{"slot pod missing node selector", mutatePod(
			pod("patchy-preview-0", "missing-selector", previewImage), func(p *corev1.Pod) {
				p.Spec.NodeSelector = nil
			}), "dedicated preview node"},
		{"slot pod wrong pool", mutatePod(pod("patchy-preview-0", "wrong-pool", previewImage), func(p *corev1.Pod) {
			p.Spec.NodeSelector["karpenter.sh/nodepool"] = "general-purpose"
		}), "dedicated preview node"},
		{"slot pod wrong class", mutatePod(pod("patchy-preview-0", "wrong-class", previewImage), func(p *corev1.Pod) {
			p.Spec.NodeSelector["eks.amazonaws.com/nodeclass"] = "default"
		}), "dedicated preview node"},
		{"slot pod missing toleration", mutatePod(
			pod("patchy-preview-0", "missing-toleration", previewImage), func(p *corev1.Pod) {
				p.Spec.Tolerations = nil
			}), "preview-only taint"},
		{"slot pod wildcard toleration", mutatePod(
			pod("patchy-preview-0", "wildcard-toleration", previewImage), func(p *corev1.Pod) {
				p.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
			}), "preview-only taint"},
		{"slot pod temporary toleration", mutatePod(
			pod("patchy-preview-0", "temporary-toleration", previewImage), func(p *corev1.Pod) {
				one := int64(1)
				p.Spec.Tolerations[0].TolerationSeconds = &one
			}), "preview-only taint"},
		{"slot pod direct node", mutatePod(pod("patchy-preview-0", "direct-node", previewImage), func(p *corev1.Pod) {
			p.Spec.NodeName = "i-0123456789abcdef0"
		}), "default scheduler"},
		{"slot pod custom scheduler", mutatePod(
			pod("patchy-preview-0", "custom-scheduler", previewImage), func(p *corev1.Pod) {
				p.Spec.SchedulerName = "other-scheduler"
			}), "default scheduler"},
		{"slot deployment missing node selector", mutateDeployment(
			deployment("patchy-preview-0", "deployment-selector", previewImage), func(d *appsv1.Deployment) {
				d.Spec.Template.Spec.NodeSelector = nil
			}), "dedicated preview node"},
		{"slot deployment missing toleration", mutateDeployment(
			deployment("patchy-preview-0", "deployment-toleration", previewImage), func(d *appsv1.Deployment) {
				d.Spec.Template.Spec.Tolerations = nil
			}), "preview-only taint"},
		{"slot deployment direct node", mutateDeployment(
			deployment("patchy-preview-0", "deployment-node", previewImage), func(d *appsv1.Deployment) {
				d.Spec.Template.Spec.NodeName = "i-0123456789abcdef0"
			}), "default scheduler"},
		{"outside pod preview toleration", mutatePod(
			pod("ordinary", "preview-toleration", "nginx:latest"), func(p *corev1.Pod) {
				p.Spec.Tolerations = previewTolerations()
			}), "preview-only toleration"},
		{"outside pod wildcard toleration", mutatePod(
			pod("ordinary", "wildcard-toleration", "nginx:latest"), func(p *corev1.Pod) {
				p.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
			}), "preview-only toleration"},
		{"outside deployment preview toleration", mutateDeployment(
			deployment("ordinary", "deployment-preview-toleration", "nginx:latest"), func(d *appsv1.Deployment) {
				d.Spec.Template.Spec.Tolerations = previewTolerations()
			}), "preview-only toleration"},
		{"outside deployment wildcard toleration", mutateDeployment(
			deployment("ordinary", "deployment-wildcard-toleration", "nginx:latest"), func(d *appsv1.Deployment) {
				d.Spec.Template.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
			}), "preview-only toleration"},
		{"outside pod direct node", mutatePod(pod("ordinary", "outside-direct-node", "nginx:latest"),
			func(p *corev1.Pod) { p.Spec.NodeName = "i-0123456789abcdef0" }), "direct node placement"},
		{"outside deployment direct node", mutateDeployment(
			deployment("ordinary", "outside-deployment-node", "nginx:latest"),
			func(d *appsv1.Deployment) { d.Spec.Template.Spec.NodeName = "i-0123456789abcdef0" }),
			"direct node placement"},
		{"external service", service("patchy-preview-0", "external", corev1.ServiceTypeLoadBalancer), "internal ClusterIP"},
		{"external IP", mutateService(
			service("patchy-preview-0", "external-ip", corev1.ServiceTypeClusterIP), func(s *corev1.Service) {
				s.Spec.ExternalIPs = []string{"203.0.113.1"}
			}), "internal ClusterIP"},
		{"helm service annotations on other name", mutateService(
			service("patchy-preview-0", "other-service", corev1.ServiceTypeClusterIP), func(s *corev1.Service) {
				s.Annotations = placeholderHelmAnnotations()
			}), "reserved for the placeholder"},
		{"helm service annotations in other slot", mutateService(
			service("patchy-preview-1", "patchy-preview-placeholder", corev1.ServiceTypeClusterIP), func(s *corev1.Service) {
				s.Annotations = placeholderHelmAnnotations()
			}), "reserved for the placeholder"},
		{"wrong placeholder service ownership", mutateService(
			service("patchy-preview-0", "patchy-preview-placeholder", corev1.ServiceTypeClusterIP), func(s *corev1.Service) {
				s.Annotations = map[string]string{"meta.helm.sh/release-name": "other"}
			}), "reserved for the placeholder"},
		{"extra placeholder service annotation", mutateService(
			service("patchy-preview-0", "patchy-preview-placeholder", corev1.ServiceTypeClusterIP), func(s *corev1.Service) {
				s.Annotations = placeholderHelmAnnotations()
				s.Annotations["example.com/extra"] = "value"
			}), "reserved for the placeholder"},
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
		{"outside preview class", ingress("ordinary", "outside-preview-class", "alb-preview", previewHost),
			"reserved for the exact preview slot"},
		{"retired slot preview class", ingress("patchy-preview-2", "retired-preview-class", "alb-preview", previewHost),
			"reserved for the exact preview slot"},
		{"unsafe annotation", mutateIngress(
			ingress("patchy-preview-0", "unsafe-annotation", "alb-preview", previewHost), func(i *networkingv1.Ingress) {
				i.Annotations = map[string]string{"alb.ingress.kubernetes.io/security-groups": "bypass"}
			}), "annotations are restricted"},
		{"helm ingress annotations on other name", mutateIngress(
			ingress("patchy-preview-0", "other-ingress", "alb-preview", previewHost), func(i *networkingv1.Ingress) {
				i.Annotations = placeholderHelmAnnotations()
			}), "annotations are restricted"},
		{"helm ingress annotations in other slot", mutateIngress(
			ingress("patchy-preview-1", "patchy-preview-placeholder", "alb-preview", previewHost),
			func(i *networkingv1.Ingress) {
				i.Annotations = placeholderHelmAnnotations()
			}), "annotations are restricted"},
		{"wrong placeholder ingress ownership", mutateIngress(
			ingress("patchy-preview-0", "patchy-preview-placeholder", "alb-preview", previewHost),
			func(i *networkingv1.Ingress) {
				i.Annotations = map[string]string{"meta.helm.sh/release-name": "other"}
			}), "annotations are restricted"},
		{"extra placeholder ingress annotation", mutateIngress(
			ingress("patchy-preview-0", "patchy-preview-placeholder", "alb-preview", previewHost),
			func(i *networkingv1.Ingress) {
				i.Annotations = placeholderHelmAnnotations()
				i.Annotations["alb.ingress.kubernetes.io/security-groups"] = "bypass"
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
	valid.Spec.Template.Spec.Containers[0].Image = previewImage
	valid.Spec.Template.Spec.NodeSelector = nil
	if err := admin.Update(ctx, valid, client.DryRunAll); err == nil ||
		!strings.Contains(err.Error(), "dedicated preview node") {
		t.Fatalf("unsafe Deployment placement update was not denied: %v", err)
	}
	outside := pod("ordinary", "outside-update", "nginx:latest")
	if err := admin.Create(ctx, outside); err != nil {
		t.Fatalf("create ordinary Pod for update test: %v", err)
	}
	outside.Spec.Tolerations = previewTolerations()
	if err := admin.Update(ctx, outside, client.DryRunAll); err == nil ||
		!strings.Contains(err.Error(), "preview-only toleration") {
		t.Fatalf("unsafe outside Pod update was not denied: %v", err)
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
	for _, ns := range []string{"patchy-preview-0", "ordinary"} {
		bound := &corev1.Pod{}
		if err := admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: "scheduler-bound"}, bound); err != nil {
			t.Fatalf("get pre-policy scheduled Pod in %s: %v", ns, err)
		}
		if bound.Labels == nil {
			bound.Labels = make(map[string]string)
		}
		bound.Labels["example.com/updated"] = "true"
		if err := admin.Update(ctx, bound, client.DryRunAll); err != nil {
			t.Errorf("valid scheduled Pod metadata update refused in %s: %v", ns, err)
		}
	}
	for _, obj := range []client.Object{
		pod("patchy-preview-0", "allowed-pod", previewImage),
		pod("patchy-preview-1", "allowed-pod-second-slot", previewImage),
		deployment("patchy-preview-0", "allowed-deployment", previewImage),
		service("patchy-preview-0", "allowed-service", corev1.ServiceTypeClusterIP),
		mutateService(service("patchy-preview-0", "patchy-preview-placeholder", corev1.ServiceTypeClusterIP),
			func(s *corev1.Service) { s.Annotations = placeholderHelmAnnotations() }),
		ingress("patchy-preview-0", "allowed-ingress", "alb-preview", previewHost),
		mutateIngress(ingress("patchy-preview-0", "patchy-preview-placeholder", "alb-preview", previewHost),
			func(i *networkingv1.Ingress) { i.Annotations = placeholderHelmAnnotations() }),
		// A normal workload in an unrelated namespace is unaffected by the
		// slot policy; an ordinary unapproved image remains valid there.
		pod("ordinary", "ordinary-pod", "nginx:latest"),
		mutatePod(pod("ordinary", "unrelated-toleration", "nginx:latest"), func(p *corev1.Pod) {
			p.Spec.Tolerations = []corev1.Toleration{{Key: "example.com/unrelated", Operator: corev1.TolerationOpExists}}
		}),
		deployment("ordinary", "ordinary-deployment", "nginx:latest"),
		ingress("ordinary", "ordinary-ingress", "alb", "ordinary.example.com"),
	} {
		if err := admin.Create(ctx, obj, client.DryRunAll); err != nil {
			t.Errorf("valid %T %s/%s refused: %v", obj, obj.GetNamespace(), obj.GetName(), err)
		}
	}
}

func placeholderHelmAnnotations() map[string]string {
	return map[string]string{
		"helm.sh/resource-policy":        "keep",
		"meta.helm.sh/release-name":      "patchy",
		"meta.helm.sh/release-namespace": "patchy",
	}
}

func pod(ns, name, image string) *corev1.Pod {
	no := false
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: corev1.PodSpec{
		ServiceAccountName: "default", AutomountServiceAccountToken: &no,
		Containers: []corev1.Container{{Name: "app", Image: image}},
	}}
	if strings.HasPrefix(ns, "patchy-preview-") {
		p.Spec.NodeSelector = map[string]string{
			"karpenter.sh/nodepool":       "patchy-preview",
			"eks.amazonaws.com/nodeclass": "patchy-preview",
		}
		p.Spec.Tolerations = previewTolerations()
	}
	return p
}

func previewTolerations() []corev1.Toleration {
	return []corev1.Toleration{{
		Key: previewTaintKey, Operator: corev1.TolerationOpEqual,
		Value: "true", Effect: corev1.TaintEffectNoExecute,
	}}
}

func mutatePod(p *corev1.Pod, f func(*corev1.Pod)) *corev1.Pod { f(p); return p }

func mutateDeployment(d *appsv1.Deployment, f func(*appsv1.Deployment)) *appsv1.Deployment {
	f(d)
	return d
}

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

// ingress is a preview Ingress routing / to a preview Service; the named
// placeholder in slot 0 routes to its own Service, as the chart renders it.
func ingress(ns, name, class, host string) *networkingv1.Ingress {
	path := networkingv1.PathTypePrefix
	service := "preview-demo-1"
	if ns == "patchy-preview-0" && name == "patchy-preview-placeholder" {
		service = name
	}
	backend := networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
		Name: service, Port: networkingv1.ServiceBackendPort{Number: 80},
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
