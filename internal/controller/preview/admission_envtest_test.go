// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/bitwise-media-group/patchy/internal/kube"
)

// TestRealPolicyDenialIsARefusal holds admissionRefused to what a real API
// server returns when the chart's slot policies deny an Ingress write: the
// required sign-in policy (an Ingress without its slot's set) and the slot
// Ingress policy (another slot's set). Each must read as a refusal, so the
// reconcile waits rather than spending a retry; neither is a quota refusal.
func TestRealPolicyDenialIsARefusal(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	c, slot0Set := startSignInPolicies(t)
	ctx := t.Context()
	path := networkingv1.PathTypePrefix
	ing := func(ns string, annotations map[string]string) *networkingv1.Ingress {
		class := "alb-preview"
		return &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: "preview-demo-1", Namespace: ns, Annotations: annotations},
			Spec: networkingv1.IngressSpec{IngressClassName: &class, Rules: []networkingv1.IngressRule{{
				Host: "demo-1.preview.patchy.devthe.net",
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &path,
						Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
							Name: "preview-demo-1", Port: networkingv1.ServiceBackendPort{Number: 80},
						}}}},
				}},
			}}},
		}
	}
	for _, tc := range []struct {
		name, want string
		obj        *networkingv1.Ingress
	}{
		{"no sign-in", "preview sign-in is required", ing("patchy-preview-0", nil)},
		// Either slot policy may name the denial; both say "pinned".
		{"another slot's set", "pinned", ing("patchy-preview-1", slot0Set)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			deadline := time.Now().Add(30 * time.Second)
			for {
				err = c.Create(ctx, tc.obj.DeepCopy(), client.DryRunAll)
				if err != nil && strings.Contains(err.Error(), tc.want) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("want a denial containing %q, got %v", tc.want, err)
				}
				time.Sleep(200 * time.Millisecond)
			}
			if !admissionRefused(err) || quotaExceeded(err) {
				t.Errorf("admissionRefused = %v, quotaExceeded = %v for %v (reason %s); want a refusal",
					admissionRefused(err), quotaExceeded(err), err, kerrors.ReasonForError(err))
			}
			if !errors.Is(ingressWrite(err), errIngressRefused) {
				t.Errorf("ingressWrite(%v) is not errIngressRefused", err)
			}
		})
	}
	// The same Ingress with its slot's set is admitted: the denials above
	// were the policies, not the object.
	if err := c.Create(ctx, ing("patchy-preview-0", slot0Set), client.DryRunAll); err != nil {
		t.Errorf("slot 0's set in slot 0 refused: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "patchy-preview-0", Name: "absent"},
		&networkingv1.Ingress{}); admissionRefused(err) {
		t.Errorf("a NotFound read as a refusal: %v", err)
	}
}

// startSignInPolicies starts an API server holding slots 0 and 1 and the
// chart's slot policies in the require stage, and returns a client and slot
// 0's rendered set.
func startSignInPolicies(t *testing.T) (client.Client, map[string]string) {
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
	c, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, ns := range []string{"patchy-preview-0", "patchy-preview-1"} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	const fixtures = "../../../hack/testdata/chart-render/"
	out, err := exec.CommandContext(ctx, "helm", "template", "patchy", "../../../charts/patchy",
		"--namespace", "patchy", "-f", fixtures+"preview-foundation.yaml", "-f", fixtures+"intent-controller.yaml",
		"-f", fixtures+"preview-controller.yaml", "-f", fixtures+"preview-auth.yaml",
		"--set", "previewAuth.stage=require", "--set", "previewAuth.permitConfirmedGeneration=1",
		"--show-only", "templates/preview-admission.yaml").Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(out)), 4096)
	var slot0Set map[string]string
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(u); err != nil {
			break
		}
		if u.GetKind() != "ValidatingAdmissionPolicy" && u.GetKind() != "ValidatingAdmissionPolicyBinding" {
			continue
		}
		if u.GetName() == "patchy-preview-ingress-auth" {
			slot0Set = firstSet(t, u, "patchy-preview-0")
		}
		if err := c.Create(ctx, u); err != nil {
			t.Fatalf("install %s %s: %v", u.GetKind(), u.GetName(), err)
		}
	}
	if len(slot0Set) != 6 {
		t.Fatalf("slot 0's rendered set has %d keys, want 6", len(slot0Set))
	}
	return c, slot0Set

}

// firstSet is slot ns's first admitted set in a rendered policy's want
// variable.
func firstSet(t *testing.T, u *unstructured.Unstructured, ns string) map[string]string {
	t.Helper()
	vars, _, _ := unstructured.NestedSlice(u.Object, "spec", "variables")
	for _, v := range vars {
		m, _ := v.(map[string]any)
		if m["name"] != "want" {
			continue
		}
		var want map[string][]map[string]string
		if err := json.Unmarshal([]byte(m["expression"].(string)), &want); err != nil {
			t.Fatal(err)
		}
		if sets := want[ns]; len(sets) > 0 {
			return sets[0]
		}
	}
	return nil
}
