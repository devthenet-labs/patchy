// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// TestSettingsValidate: the image prefix is the operator's
// (preview.imagePathPrefix), so Validate checks its shape, never a fixed
// path: a registry host, one or more lowercase path segments, and the
// trailing slash an image repository continues from. A custom prefix was
// refused while the controller pinned /patchy/previews/.
func TestSettingsValidate(t *testing.T) {
	for _, tc := range []struct {
		prefix string
		ok     bool
	}{
		{"123456789012.dkr.ecr.us-east-1.amazonaws.com/patchy/previews/", true},
		{"registry.example/acme/previews/", true},
		{"registry.example/previews/", true},
		{"123456789012.dkr.ecr.us-east-1.amazonaws.com/team_a/runtime.images/", true},
		{"registry.example:5000/acme/", true},
		{"registry.example/acme/previews", false}, // no trailing slash
		{"", false},
		{"registry.example/", false}, // a host alone: every repository on it
		{"/patchy/previews/", false}, // no host
		{"registry.example", false},  // no path, no slash
		{"registry.example/acme//previews/", false},
		{"registry.example/Acme/previews/", false},
		{"Registry.Example/acme/previews/", false},
		{"registry.example/acme/previews:x/", false},
		{"registry.example/acme@sha256/", false},
		{"registry.example/acme/*/", false},
		{"registry.example/acme /", false},
		{"registry.example/-acme/", false},
		{"registry.example/acme-/", false},
		{"registry.example/acme/../previews/", false},
		{"-registry.example/acme/", false},
		{"https://registry.example/acme/", false},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			s := testSettings()
			s.ImagePrefix = tc.prefix
			err := s.Validate()
			if tc.ok != (err == nil) {
				t.Fatalf("Validate with image prefix %q = %v, want ok=%v", tc.prefix, err, tc.ok)
			}
			if err != nil && !strings.Contains(err.Error(), "image prefix") {
				t.Errorf("error %q does not name the image prefix", err)
			}
		})
	}
	// The prefix is not the only setting checked.
	s := testSettings()
	s.SlotCount = 5
	if s.Validate() == nil {
		t.Error("Validate accepted five slots")
	}
}

// TestImageUnderCustomPrefix: with an operator's own prefix, a component's
// image repository is that exact prefix plus one DNS-label leaf. Another
// registry, the default path, an agent-environment path, a nested path, a
// tag or digest, and a leaf that is not a DNS label are all refused; the
// last two leaves (app-, -app) are what the CRD admitted before its leaf was
// tightened.
func TestImageUnderCustomPrefix(t *testing.T) {
	s := testSettings()
	s.ImagePrefix = "registry.example/acme/previews/"
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	preview := func(repo string) *v1alpha1.Preview {
		_, p := testPreview("demo-1", time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
		p.Spec.Components[0].ImageRepository = repo
		p.Spec.TTL = metav1.Duration{Duration: 72 * time.Hour}
		return p
	}
	for _, tc := range []struct {
		repo string
		ok   bool
	}{
		{"registry.example/acme/previews/demo", true},
		{"registry.example/acme/previews/a", true},
		{"registry.example/acme/previews/web-app-2", true},
		{"evil.example/acme/previews/demo", false},
		{"registry.example/patchy/previews/demo", false},
		{"registry.example/acme/app-envs/demo", false},
		{"registry.example/acme/previews-evil/demo", false},
		{"registry.example/acme/previews/team/demo", false},
		{"registry.example/acme/previews/demo:latest", false},
		{"registry.example/acme/previews/demo@sha256:" + strings.Repeat("a", 64), false},
		{"registry.example/acme/previews/", false},
		{"registry.example/acme/previews/app-", false},
		{"registry.example/acme/previews/-app", false},
		{"registry.example/acme/previews/Demo", false},
		{"registry.example/acme/previews/demo_app", false},
		{"registry.example/acme/previews/demo.app", false},
	} {
		t.Run(tc.repo, func(t *testing.T) {
			err := s.validatePreview(preview(tc.repo))
			if tc.ok != (err == nil) {
				t.Errorf("validatePreview with image repository %q = %v, want ok=%v", tc.repo, err, tc.ok)
			}
		})
	}
	// What is admitted renders exactly under the prefix.
	p := preview("registry.example/acme/previews/demo")
	if got, want := s.deployment(p, 0, 0).Spec.Template.Spec.Containers[0].Image,
		"registry.example/acme/previews/demo:sha-"+testSHA; got != want {
		t.Errorf("image = %q, want %q", got, want)
	}
}
