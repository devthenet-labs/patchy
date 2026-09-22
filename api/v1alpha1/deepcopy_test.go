// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestRunnerImageDeepCopy proves the generated deepcopy isolates the
// runner-image records on every status that carries one: a copy shares no
// pointer with its original, so a controller mutating a cached object's copy
// cannot leak into the informer cache.
func TestRunnerImageDeepCopy(t *testing.T) {
	pinned := "ghcr.io/acme/go-agent-env@sha256:" + strings.Repeat("a", 64)
	now := metav1.NewTime(time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC))

	t.Run("repository status", func(t *testing.T) {
		orig := RepositoryStatus{
			ResolvedSHA: strings.Repeat("b", 40),
			RunnerImage: &RunnerImage{
				Declared:   "ghcr.io/acme/go-agent-env:1.26",
				Manifest:   ".patchy/agent.yaml",
				Image:      pinned,
				SearchPath: "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin",
				Verified:   true,
				ResolvedAt: now.DeepCopy(),
			},
		}
		dup := orig.DeepCopy()
		if !reflect.DeepEqual(dup, &orig) {
			t.Fatalf("DeepCopy() = %+v, want %+v", dup, &orig)
		}
		if dup.RunnerImage == orig.RunnerImage {
			t.Fatal("RunnerImage pointer is shared with the copy")
		}
		if dup.RunnerImage.ResolvedAt == orig.RunnerImage.ResolvedAt {
			t.Fatal("RunnerImage.ResolvedAt pointer is shared with the copy")
		}
		dup.RunnerImage.Image = "ghcr.io/evil/env@sha256:" + strings.Repeat("c", 64)
		dup.RunnerImage.ResolvedAt.Time = now.Add(time.Hour)
		if orig.RunnerImage.Image != pinned || !orig.RunnerImage.ResolvedAt.Equal(&now) {
			t.Errorf("mutating the copy changed the original: %+v", orig.RunnerImage)
		}
	})

	t.Run("repository status without a runner image", func(t *testing.T) {
		if got := (&RepositoryStatus{}).DeepCopy().RunnerImage; got != nil {
			t.Errorf("DeepCopy().RunnerImage = %+v, want nil", got)
		}
	})

	ref := func() *RunnerImageRef {
		return &RunnerImageRef{Image: pinned, Source: RunnerImageSourceRepository, Manifest: ".patchy/agent.yaml"}
	}
	holders := []struct {
		name string
		// build places r on a fresh holder and returns the original's
		// pointer beside the deep copy's.
		build func(r *RunnerImageRef) (orig, dup *RunnerImageRef)
	}{
		{"investigation status", func(r *RunnerImageRef) (*RunnerImageRef, *RunnerImageRef) {
			s := InvestigationStatus{RunnerImage: r}
			return s.RunnerImage, s.DeepCopy().RunnerImage
		}},
		{"remediation status", func(r *RunnerImageRef) (*RunnerImageRef, *RunnerImageRef) {
			s := RemediationStatus{RunnerImage: r}
			return s.RunnerImage, s.DeepCopy().RunnerImage
		}},
		{"investigation summary", func(r *RunnerImageRef) (*RunnerImageRef, *RunnerImageRef) {
			s := InvestigationSummary{Name: "finding-abc123-1-inv-1", Attempt: 1, RunnerImage: r}
			return s.RunnerImage, s.DeepCopy().RunnerImage
		}},
	}
	for _, h := range holders {
		t.Run(h.name, func(t *testing.T) {
			orig, dup := h.build(ref())
			if dup == orig {
				t.Fatal("RunnerImage pointer is shared with the copy")
			}
			if *dup != *orig {
				t.Fatalf("copy = %+v, want %+v", *dup, *orig)
			}
			dup.Source = RunnerImageSourceDefault
			dup.Manifest = ""
			if orig.Source != RunnerImageSourceRepository || orig.Manifest != ".patchy/agent.yaml" {
				t.Errorf("mutating the copy changed the original: %+v", *orig)
			}
		})
	}
}
