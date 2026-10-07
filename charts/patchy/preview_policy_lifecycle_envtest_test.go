// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// autoModeFinalizer is the finalizer EKS Auto Mode puts on every Ingress of
// the preview IngressGroup, and removes with an UPDATE once the load balancer
// has forgotten it.
const autoModeFinalizer = "group.ingress.eks.amazonaws.com/devthenet-dev-preview"

// Objects that predate the policies and no longer conform to them: what a
// slot holds after a rule tightens on a live install. Each carries a
// finalizer, as Auto Mode's Ingresses do.
const (
	legacyIngress = "legacy-ingress"
	legacyService = "legacy-service"
	legacyPod     = "legacy-secret-env"
)

// createLegacySlotObjects runs before the policies are installed.
func createLegacySlotObjects(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	ing := ingress("patchy-preview-0", legacyIngress, "alb-preview", previewHost)
	ing.Annotations = map[string]string{"alb.ingress.kubernetes.io/security-groups": "legacy"}
	ing.Finalizers = []string{autoModeFinalizer}
	svc := service("patchy-preview-0", legacyService, corev1.ServiceTypeClusterIP)
	svc.Annotations = map[string]string{"example.com/legacy": "true"}
	svc.Finalizers = []string{"example.com/legacy"}
	for _, obj := range []client.Object{ing, svc, withSecretEnv(pod("patchy-preview-0", legacyPod, previewImage))} {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("create pre-policy %T %s: %v", obj, obj.GetName(), err)
		}
	}
}

// withSecretEnv gives a Pod's container one variable read from a Secret.
func withSecretEnv(p *corev1.Pod) *corev1.Pod {
	p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{
		Name: "CLIENT_SECRET",
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "patchy-preview-oidc"}, Key: "clientSecret",
		}},
	})
	return p
}

// testSecretRefs: no slot Pod or Deployment may name a Secret through its
// environment, while ordinary env sources stay admitted, outside the slots
// nothing changes, and a pre-policy Pod's metadata stays writable.
func testSecretRefs(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	const want = "may not read a Secret"
	envFromSecret := func(c *corev1.Container) {
		c.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "patchy-preview-oidc"},
		}}}
	}
	for _, tc := range []struct {
		name string
		obj  client.Object
	}{
		{"pod env secretKeyRef", withSecretEnv(pod("patchy-preview-0", "secret-env", previewImage))},
		{"second slot pod env secretKeyRef", withSecretEnv(pod("patchy-preview-1", "secret-env", previewImage))},
		{"pod envFrom secretRef", mutatePod(pod("patchy-preview-0", "secret-envfrom", previewImage),
			func(p *corev1.Pod) { envFromSecret(&p.Spec.Containers[0]) })},
		{"deployment env secretKeyRef", mutateDeployment(deployment("patchy-preview-0", "secret-env", previewImage),
			func(d *appsv1.Deployment) {
				d.Spec.Template.Spec = withSecretEnv(&corev1.Pod{Spec: d.Spec.Template.Spec}).Spec
			})},
		{"deployment envFrom secretRef", mutateDeployment(deployment("patchy-preview-0", "secret-envfrom", previewImage),
			func(d *appsv1.Deployment) { envFromSecret(&d.Spec.Template.Spec.Containers[0]) })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := admin.Create(ctx, tc.obj, client.DryRunAll)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("want rejection containing %q, got %v", want, err)
			}
		})
	}

	for _, obj := range []client.Object{
		mutatePod(pod("patchy-preview-0", "plain-env", previewImage), func(p *corev1.Pod) {
			p.Spec.Containers[0].Env = []corev1.EnvVar{
				{Name: "PLAIN", Value: "x"},
				{Name: "FROM_CM", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "cm"}, Key: "k",
				}}},
				{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				}}},
			}
			p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "cm"},
			}}}
		}),
		withSecretEnv(pod("ordinary", "secret-env", "nginx:latest")),
	} {
		if err := admin.Create(ctx, obj, client.DryRunAll); err != nil {
			t.Errorf("%s/%s refused: %v", obj.GetNamespace(), obj.GetName(), err)
		}
	}

	// A Deployment admitted before gains a Secret reference: refused.
	d := deployment("patchy-preview-0", "secret-update", previewImage)
	if err := admin.Create(ctx, d); err != nil {
		t.Fatalf("create valid Deployment: %v", err)
	}
	d.Spec.Template.Spec = withSecretEnv(&corev1.Pod{Spec: d.Spec.Template.Spec}).Spec
	if err := admin.Update(ctx, d, client.DryRunAll); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Deployment update adding a Secret reference: want %q, got %v", want, err)
	}

	// The pre-policy Pod still reads a Secret; its metadata stays writable,
	// since only a changed spec is evaluated.
	legacy := &corev1.Pod{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: "patchy-preview-0", Name: legacyPod}, legacy); err != nil {
		t.Fatalf("get pre-policy Pod: %v", err)
	}
	legacy.Labels = map[string]string{"example.com/updated": "true"}
	if err := admin.Update(ctx, legacy, client.DryRunAll); err != nil {
		t.Errorf("metadata update of a pre-policy Pod refused: %v", err)
	}
}

// testLegacyObjectsCanStillBeDeleted is the finalizer case: an Ingress or
// Service that no longer conforms stays non-conforming (any change to its
// spec or annotations is refused), but a metadata-only UPDATE is admitted,
// and so once it is deleted is the UPDATE that removes its finalizer, so it
// never hangs Terminating and wedges its slot.
func testLegacyObjectsCanStillBeDeleted(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	key := func(name string) client.ObjectKey { return client.ObjectKey{Namespace: "patchy-preview-0", Name: name} }

	ing := &networkingv1.Ingress{}
	if err := admin.Get(ctx, key(legacyIngress), ing); err != nil {
		t.Fatalf("get legacy Ingress: %v", err)
	}
	annotated := ing.DeepCopy()
	annotated.Annotations["alb.ingress.kubernetes.io/healthcheck-path"] = "/healthz"
	if err := admin.Update(ctx, annotated, client.DryRunAll); err == nil ||
		!strings.Contains(err.Error(), "annotations are restricted") {
		t.Errorf("annotation change on a non-conforming Ingress: want the annotation denial, got %v", err)
	}
	rehosted := ing.DeepCopy()
	rehosted.Spec.Rules[0].Host = "other-1.preview.patchy.devthe.net"
	if err := admin.Update(ctx, rehosted, client.DryRunAll); err == nil ||
		!strings.Contains(err.Error(), "annotations are restricted") {
		t.Errorf("spec change on a non-conforming Ingress: want the annotation denial, got %v", err)
	}
	labelled := ing.DeepCopy()
	labelled.Labels = map[string]string{"example.com/updated": "true"}
	if err := admin.Update(ctx, labelled, client.DryRunAll); err != nil {
		t.Errorf("metadata-only update of a non-conforming Ingress refused: %v", err)
	}

	svc := &corev1.Service{}
	if err := admin.Get(ctx, key(legacyService), svc); err != nil {
		t.Fatalf("get legacy Service: %v", err)
	}
	svcAnnotated := svc.DeepCopy()
	svcAnnotated.Annotations["example.com/other"] = "true"
	if err := admin.Update(ctx, svcAnnotated, client.DryRunAll); err == nil ||
		!strings.Contains(err.Error(), "Service annotations are restricted") {
		t.Errorf("annotation change on a non-conforming Service: want the annotation denial, got %v", err)
	}

	for _, obj := range []client.Object{ing, svc} {
		if err := admin.Delete(ctx, obj); err != nil {
			t.Fatalf("delete %T %s: %v", obj, obj.GetName(), err)
		}
		if err := admin.Get(ctx, key(obj.GetName()), obj); err != nil {
			t.Fatalf("get terminating %T %s: %v", obj, obj.GetName(), err)
		}
		if obj.GetDeletionTimestamp() == nil {
			t.Fatalf("%T %s: no deletionTimestamp after delete", obj, obj.GetName())
		}
		obj.SetFinalizers(nil)
		if err := admin.Update(ctx, obj); err != nil {
			t.Fatalf("finalizer removal on terminating non-conforming %T %s refused: %v", obj, obj.GetName(), err)
		}
		if err := admin.Get(ctx, key(obj.GetName()), obj); !apierrors.IsNotFound(err) {
			t.Errorf("%T %s still present after its finalizer was removed: %v", obj, obj.GetName(), err)
		}
	}
}

// testTerminatingObjectsStayGuarded: being deleted is no exemption. A
// conforming Service or Ingress held Terminating by a finalizer still has
// every change to its spec or annotations evaluated (a NodePort or an
// external IP on a terminating Service would be programmed on every node
// until the finalizer goes), while the finalizer's own removal stays admitted.
func testTerminatingObjectsStayGuarded(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	const finalizer = "example.com/hold"
	svc := service("patchy-preview-0", "preview-terminating", corev1.ServiceTypeClusterIP)
	svc.Finalizers = []string{finalizer}
	ing := ingress("patchy-preview-0", "terminating-ingress", "alb-preview", previewHost)
	ing.Finalizers = []string{finalizer}
	for _, obj := range []client.Object{svc, ing} {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("create finalized %T %s: %v", obj, obj.GetName(), err)
		}
		if err := admin.Delete(ctx, obj); err != nil {
			t.Fatalf("delete %T %s: %v", obj, obj.GetName(), err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatalf("get terminating %T %s: %v", obj, obj.GetName(), err)
		}
		if obj.GetDeletionTimestamp() == nil {
			t.Fatalf("%T %s: no deletionTimestamp after delete", obj, obj.GetName())
		}
	}

	for _, tc := range []struct {
		name string
		obj  client.Object
		want string
	}{
		{"service type NodePort", mutateService(svc.DeepCopy(), func(s *corev1.Service) {
			s.Spec.Type = corev1.ServiceTypeNodePort
		}), "internal ClusterIP"},
		{"service external IP", mutateService(svc.DeepCopy(), func(s *corev1.Service) {
			s.Spec.ExternalIPs = []string{"203.0.113.10"}
		}), "internal ClusterIP"},
		{"service annotation", mutateService(svc.DeepCopy(), func(s *corev1.Service) {
			s.Annotations = map[string]string{"example.com/other": "true"}
		}), "Service annotations are restricted"},
		{"ingress class", func() client.Object {
			i := ing.DeepCopy()
			class := "alb"
			i.Spec.IngressClassName = &class
			return i
		}(), "must use alb-preview"},
		{"ingress annotation", func() client.Object {
			i := ing.DeepCopy()
			i.Annotations = map[string]string{"alb.ingress.kubernetes.io/security-groups": "open"}
			return i
		}(), "annotations are restricted"},
	} {
		t.Run("terminating "+tc.name, func(t *testing.T) {
			err := admin.Update(ctx, tc.obj, client.DryRunAll)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want rejection containing %q, got %v", tc.want, err)
			}
		})
	}

	for _, obj := range []client.Object{svc, ing} {
		obj.SetFinalizers(nil)
		if err := admin.Update(ctx, obj); err != nil {
			t.Fatalf("finalizer removal on terminating %T %s refused: %v", obj, obj.GetName(), err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
			t.Errorf("%T %s still present after its finalizer was removed: %v", obj, obj.GetName(), err)
		}
	}
}
