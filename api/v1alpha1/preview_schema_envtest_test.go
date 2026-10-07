// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"fmt"
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
	component := func(name, path string) patchyv1.PreviewComponent {
		return patchyv1.PreviewComponent{
			Name: name, ImageRepository: "registry.example/patchy/previews/" + name,
			Revision: strings.Repeat("a", 40), Port: 8080, ReadinessPath: "/healthz", Path: path,
		}
	}
	components := func(n int) []patchyv1.PreviewComponent {
		out := []patchyv1.PreviewComponent{component("c0", "")}
		for i := 1; i < n; i++ {
			out = append(out, component(fmt.Sprintf("c%d", i), fmt.Sprintf("/c%d", i)))
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		mutate func(*patchyv1.Preview)
	}{
		{"short-sha", func(p *patchyv1.Preview) { p.Spec.Components[0].Revision = "abc" }},
		{"overlong-ttl", func(p *patchyv1.Preview) { p.Spec.TTL.Duration = 73 * time.Hour }},
		// A repeated component: the same name (the list key) at the same path.
		{"repeated-component", func(p *patchyv1.Preview) {
			p.Spec.Components = append(p.Spec.Components, p.Spec.Components[0])
		}},
		{"unsafe-path", func(p *patchyv1.Preview) { p.Spec.Components[0].ReadinessPath = "/health?token=1" }},
		// The image prefix is the operator's (preview.imagePathPrefix), so the
		// schema checks the repository's shape and its leaf, and the
		// preview-controller and the slot admission policy refuse a
		// repository off the configured prefix, an agent-environment one
		// included (TestSettingsValidate, TestMultiComponentValidation and
		// the chart's preview policy envtests). The leaf is a DNS label: the
		// schema refused neither of the first two before it said so.
		{"image-leaf-trailing-hyphen", func(p *patchyv1.Preview) {
			p.Spec.Components[0].ImageRepository = "registry.example/acme/previews/demo-"
		}},
		{"image-leaf-leading-hyphen", func(p *patchyv1.Preview) {
			p.Spec.Components[0].ImageRepository = "registry.example/patchy/previews/-demo"
		}},
		{"image-without-path", func(p *patchyv1.Preview) {
			p.Spec.Components[0].ImageRepository = "registry.example/demo"
		}},
		{"image-with-tag", func(p *patchyv1.Preview) {
			p.Spec.Components[0].ImageRepository = "registry.example/patchy/previews/demo:latest"
		}},
		{"image-uppercase-segment", func(p *patchyv1.Preview) {
			p.Spec.Components[0].ImageRepository = "registry.example/Acme/previews/demo"
		}},
		{"image-empty-segment", func(p *patchyv1.Preview) {
			p.Spec.Components[0].ImageRepository = "registry.example/acme//demo"
		}},
		// Slice 3: up to four components, each under its own path.
		{"five-components", func(p *patchyv1.Preview) { p.Spec.Components = components(5) }},
		{"duplicate-component-name", func(p *patchyv1.Preview) {
			p.Spec.Components = []patchyv1.PreviewComponent{component("web", ""), component("web", "/api")}
		}},
		{"duplicate-component-path", func(p *patchyv1.Preview) {
			p.Spec.Components = []patchyv1.PreviewComponent{component("web", "/api"), component("api", "/api")}
		}},
		// An omitted path is "/", so it collides with an explicit one.
		{"omitted-path-beside-root", func(p *patchyv1.Preview) {
			p.Spec.Components = []patchyv1.PreviewComponent{component("web", ""), component("api", "/")}
		}},
		{"uppercase-component-path", func(p *patchyv1.Preview) { p.Spec.Components[0].Path = "/API" }},
		{"component-path-trailing-slash", func(p *patchyv1.Preview) { p.Spec.Components[0].Path = "/api/" }},
		{"relative-component-path", func(p *patchyv1.Preview) { p.Spec.Components[0].Path = "api" }},
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
	// An operator's own image prefix, nested segments with ECR's separators
	// included: the schema has no fixed patchy/previews/ path.
	for i, repo := range []string{
		"registry.example/acme/previews/demo",
		"123456789012.dkr.ecr.us-east-1.amazonaws.com/team_a/runtime.images/web-app",
		"registry.example:5000/x/a",
	} {
		custom := valid(fmt.Sprintf("preview-prefix-%d", i))
		custom.Spec.Components[0].ImageRepository = repo
		if err := c.Create(ctx, custom); err != nil {
			t.Errorf("Preview with image repository %s rejected: %v", repo, err)
		}
	}
	p.Spec.Components[0].Revision = strings.Repeat("b", 40)
	if err := c.Update(ctx, p); err != nil {
		t.Fatalf("PR-head update rejected: %v", err)
	}
	p.Spec.IntentRef.UID = "uid-other"
	if err := c.Update(ctx, p); err == nil {
		t.Fatal("API server allowed Preview to rebind to another Intent")
	}
	testPreviewProject(ctx, t, c, valid)

	for n, comps := range [][]patchyv1.PreviewComponent{
		components(2), components(4),
		{component("api", "/api"), component("web", "/")},
		{component("api", "/api/v1"), component("docs", "/api")},
	} {
		multi := valid(fmt.Sprintf("preview-multi-%d", n))
		multi.Spec.Components = comps
		if err := c.Create(ctx, multi); err != nil {
			t.Errorf("valid %d-component Preview %v rejected: %v", len(comps), comps, err)
		}
	}

	// A component's path is never defaulted: a single-repository Preview
	// written without one reads back without one, so a writer comparing
	// specs keeps it as it is.
	got := &patchyv1.Preview{}
	if err := c.Get(ctx, client.ObjectKey{Name: "preview-valid", Namespace: "default"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Components[0].Path != "" {
		t.Errorf("component path read back as %q, want it left out", got.Spec.Components[0].Path)
	}

	// Status: one record per component, with the revision it serves.
	slot := int32(0)
	got.Status = patchyv1.PreviewStatus{Phase: patchyv1.PreviewReady, Slot: &slot,
		URL: "https://preview-valid.preview.example", ObservedRevision: strings.Repeat("b", 40)}
	for i := range 4 {
		got.Status.Components = append(got.Status.Components, patchyv1.PreviewComponentStatus{
			Name: fmt.Sprintf("c%d", i), Revision: strings.Repeat("b", 40), ImageID: "repo@sha256:" + strings.Repeat("1", 64),
		})
	}
	want := got.Status.DeepCopy()
	if err := c.Status().Update(ctx, got); err != nil {
		t.Fatalf("four-component status rejected: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(got), got); err != nil {
		t.Fatal(err)
	}
	sameJSON(t, "preview status", got.Status, *want)
	for _, tc := range []struct {
		name   string
		mutate func(*patchyv1.PreviewStatus)
	}{
		{"five component records", func(s *patchyv1.PreviewStatus) {
			s.Components = append(s.Components, s.Components[0])
		}},
		{"a short component revision", func(s *patchyv1.PreviewStatus) { s.Components[0].Revision = "abc" }},
	} {
		bad := got.DeepCopy()
		tc.mutate(&bad.Status)
		if err := c.Status().Update(ctx, bad); err == nil {
			t.Errorf("status with %s accepted", tc.name)
		}
	}
}

// testPreviewProject pins spec.project's schema: optional (a Preview written
// before it existed still updates), set once onto such a Preview, and then
// neither changed nor removed, since the sign-in relay authorises a viewer
// against it alone. Its shape is a Project name's.
func testPreviewProject(ctx context.Context, t *testing.T, c client.Client, valid func(string) *patchyv1.Preview) {
	t.Helper()
	for _, bad := range []string{"Demo", "-demo", "demo-", strings.Repeat("a", 26), "a/b"} {
		p := valid("preview-project-bad")
		p.Spec.Project = bad
		if err := c.Create(ctx, p); err == nil {
			t.Errorf("Preview with spec.project %q accepted", bad)
			_ = c.Delete(ctx, p)
		}
	}
	legacy := valid("preview-project-legacy")
	if err := c.Create(ctx, legacy); err != nil {
		t.Fatalf("Preview without spec.project rejected: %v", err)
	}
	legacy.Spec.Components[0].Revision = strings.Repeat("c", 40)
	if err := c.Update(ctx, legacy); err != nil {
		t.Fatalf("update of a Preview without spec.project rejected: %v", err)
	}
	legacy.Spec.Project = "demo"
	if err := c.Update(ctx, legacy); err != nil {
		t.Fatalf("stamping spec.project once rejected: %v", err)
	}
	stamped := valid("preview-project-stamped")
	stamped.Spec.Project = "demo"
	if err := c.Create(ctx, stamped); err != nil {
		t.Fatalf("Preview with spec.project rejected: %v", err)
	}
	for _, p := range []*patchyv1.Preview{legacy, stamped} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
			t.Fatal(err)
		}
		p.Spec.Components[0].Revision = strings.Repeat("d", 40)
		if err := c.Update(ctx, p); err != nil {
			t.Fatalf("%s: PR-head update with spec.project kept rejected: %v", p.Name, err)
		}
		for _, change := range []string{"other", ""} {
			moved := p.DeepCopy()
			moved.Spec.Project = change
			if err := c.Update(ctx, moved); err == nil {
				t.Fatalf("%s: spec.project changed from %q to %q", p.Name, p.Spec.Project, change)
			} else if !strings.Contains(err.Error(), "spec.project is set once") {
				t.Errorf("%s: spec.project change refused for another reason: %v", p.Name, err)
			}
		}
	}
}
