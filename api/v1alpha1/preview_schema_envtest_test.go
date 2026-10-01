// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchyv1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

func testPreviewSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	valid := func(name string) *patchyv1.Preview {
		return &patchyv1.Preview{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: patchyv1.PreviewSpec{
				IntentRef: patchyv1.ObjectReference{Name: name, UID: types.UID("uid-1")},
				HostLabel: name,
				Components: []patchyv1.PreviewComponent{{
					Name: "demo", ImageRepository: "registry.example/patchy/previews/demo",
					Revision: strings.Repeat("a", 40), Port: 8080, ReadinessPath: "/health",
				}},
				TTL: metav1.Duration{Duration: 72 * time.Hour},
			}}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*patchyv1.Preview)
	}{
		{"short-sha", func(p *patchyv1.Preview) { p.Spec.Components[0].Revision = "abc" }},
		{"overlong-ttl", func(p *patchyv1.Preview) { p.Spec.TTL.Duration = 73 * time.Hour }},
		{"second-component", func(p *patchyv1.Preview) {
			p.Spec.Components = append(p.Spec.Components, p.Spec.Components[0])
		}},
		{"unsafe-path", func(p *patchyv1.Preview) { p.Spec.Components[0].ReadinessPath = "/health?token=1" }},
		{"wrong-image-repository", func(p *patchyv1.Preview) {
			p.Spec.Components[0].ImageRepository = "registry.example/patchy/app-envs/demo"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := valid("preview-" + tc.name)
			tc.mutate(p)
			if err := c.Create(ctx, p); err == nil {
				t.Fatal("API server accepted invalid Preview")
			}
		})
	}
	p := valid("preview-valid")
	if err := c.Create(ctx, p); err != nil {
		t.Fatalf("valid Preview rejected: %v", err)
	}
	p.Spec.Components[0].Revision = strings.Repeat("b", 40)
	if err := c.Update(ctx, p); err != nil {
		t.Fatalf("PR-head update rejected: %v", err)
	}
	p.Spec.IntentRef.UID = "uid-other"
	if err := c.Update(ctx, p); err == nil {
		t.Fatal("API server allowed Preview to rebind to another Intent")
	}
}
