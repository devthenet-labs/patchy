// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// describeRepository is a Repository snapshot of fnd-1 controlled by the
// finding whose UID is owner.
func describeRepository(owner types.UID, ri *v1alpha1.RunnerImage) *v1alpha1.Repository {
	isController := true
	return &v1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fnd-1-src", Namespace: testNamespace,
			Labels: map[string]string{v1alpha1.LabelFinding: "fnd-1"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1.GroupVersion.String(), Kind: "Finding", Name: "fnd-1", UID: owner,
				Controller: &isController,
			}},
		},
		Spec:   v1alpha1.RepositorySpec{URL: "https://github.com/acme/orders"},
		Status: v1alpha1.RepositoryStatus{ResolvedSHA: "abc123", RunnerImage: ri},
	}
}

// TestDescribeRunnerImage: describe finding reads the runner image off the
// finding's own Repository, never one left behind by an earlier finding of
// the same name, and describe repository renders a detail view rather than
// raw YAML.
func TestDescribeRunnerImage(t *testing.T) {
	rejected := &v1alpha1.RunnerImage{Declared: "docker.io/library/golang:1.26", Manifest: ".patchy/agent.yaml",
		Rejected: "NotAllowlisted", Message: "image `docker.io/library/golang:1.26` is not under an allowlisted path"}
	withUID := func(f *v1alpha1.Finding) { f.UID = "fnd-uid-1" }
	cases := []struct {
		name   string
		noun   string
		owner  types.UID
		want   []string
		absent []string
	}{
		{"finding", "finding", "fnd-uid-1", []string{"Runner image", "Declared in", ".patchy/agent.yaml",
			"NotAllowlisted — image `docker.io/library/golang:1.26`", "Source"}, nil},
		{"finding with a foreign repository", "finding", "an-earlier-finding", nil,
			[]string{"Runner image", "NotAllowlisted"}},
		{"repository", "repository", "fnd-uid-1", []string{"Repository fnd-1-src", "abc123", "Runner image",
			"NotAllowlisted"}, []string{"apiVersion:"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, testFinding("fnd-1", v1alpha1.PhaseQueued, withUID),
				describeRepository(tc.owner, rejected))
			name := "fnd-1"
			if tc.noun == "repository" {
				name = "fnd-1-src"
			}
			if err := runDescribe(t.Context(), h.opts, tc.noun, name); err != nil {
				t.Fatalf("describe %s %s: %v", tc.noun, name, err)
			}
			got := h.out.String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q:\n%s", want, got)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(got, absent) {
					t.Errorf("output contains %q:\n%s", absent, got)
				}
			}
		})
	}
}
