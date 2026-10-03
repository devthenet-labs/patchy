// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	patchyv1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// schemaTrees is n extra trees for a plan run, keyed app0..app<n-1>.
func schemaTrees(n int) []patchyv1.IntentRunTree {
	out := make([]patchyv1.IntentRunTree, n)
	for k := range out {
		key := fmt.Sprintf("app%d", k)
		out[k] = patchyv1.IntentRunTree{Name: key, URL: "https://github.com/acme/" + key,
			RepositoryRef: patchyv1.LocalObjectReference{Name: patchyv1.IntentRunTreeRepositoryName("r", key)}}
	}
	return out
}

// treeRecords is the launch record of n extra trees.
func treeRecords(n int) []patchyv1.IntentRunTreeStatus {
	out := make([]patchyv1.IntentRunTreeStatus, n)
	for k := range out {
		out[k] = patchyv1.IntentRunTreeStatus{Name: fmt.Sprintf("app%d", k), BaseSHA: schemaSHA,
			ArtifactDigest: strings.Repeat("f", 64)}
	}
	return out
}

// testIntentRunTreesSchema exercises a plan run's extra trees (slice 3):
// only a plan run reads them, at most seven, each a different repository
// from the others and from the planning repository, keyed like a Project
// repository, immutable with the rest of the spec; and the launch record of
// what each tree was round-trips and is bounded.
func testIntentRunTreesSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	for n, tt := range []struct {
		name    string
		stage   patchyv1.IntentStage
		mutate  func(*patchyv1.IntentRunSpec)
		wantErr bool
	}{
		{"a plan run reading seven more trees", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) {
			s.Trees = schemaTrees(patchyv1.MaxIntentRunTrees)
		}, false},
		{"a plan run reading eight more trees", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) {
			s.Trees = schemaTrees(patchyv1.MaxIntentRunTrees + 1)
		}, true},
		{"a freely named tree", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) {
			s.Trees = []patchyv1.IntentRunTree{{Name: "web", URL: "https://github.com/acme/Acme.Web_App",
				RepositoryRef: patchyv1.LocalObjectReference{Name: "r-src-web"}}}
		}, false},
		{"trees on a build run", patchyv1.IntentStageBuild, func(s *patchyv1.IntentRunSpec) {
			s.Trees = schemaTrees(1)
		}, true},
		{"trees on a revise run", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Trees = schemaTrees(1)
		}, true},
		{"a tree key twice", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) {
			s.Trees = append(schemaTrees(1), patchyv1.IntentRunTree{Name: "app0", URL: "https://github.com/acme/other",
				RepositoryRef: patchyv1.LocalObjectReference{Name: "r-src-other"}})
		}, true},
		{"a tree key that is not a DNS label", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) {
			s.Trees = schemaTrees(1)
			s.Trees[0].Name = "Web"
		}, true},
		{"a tree key past the name budget", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) {
			s.Trees = schemaTrees(1)
			s.Trees[0].Name = strings.Repeat("k", patchyv1.MaxRepositoryKeyLength+1)
		}, true},
		{"a tree that is the planning repository", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) {
			s.Trees = schemaTrees(1)
			s.Trees[0].URL = "https://GitHub.com/Acme/Shop.git"
		}, true},
		{"two trees for one repository", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) {
			s.Trees = schemaTrees(2)
			s.Trees[1].URL = "https://github.com/Acme/App0"
		}, true},
		{"credentials in a tree url", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) {
			s.Trees = schemaTrees(1)
			s.Trees[0].URL = "https://x:token@github.com/acme/app0"
		}, true},
	} {
		t.Run("create with "+tt.name, func(t *testing.T) {
			r := schemaIntentRun(fmt.Sprintf("target-1-trees-%d", n), tt.stage)
			tt.mutate(&r.Spec)
			if err := c.Create(ctx, r); (err != nil) != tt.wantErr {
				t.Errorf("Create(intent run: %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}

	key := client.ObjectKey{Name: "target-1-trees-0", Namespace: "default"} // the seven-tree plan run
	t.Run("spec trees are immutable", func(t *testing.T) {
		r := &patchyv1.IntentRun{}
		if err := c.Get(ctx, key, r); err != nil {
			t.Fatalf("Get(plan run) = %v", err)
		}
		r.Spec.Trees = r.Spec.Trees[:1]
		if err := c.Update(ctx, r); err == nil {
			t.Error("Update(plan run spec trees) = nil, want immutability rejection")
		}
	})

	t.Run("the launch record round-trips", func(t *testing.T) {
		r := &patchyv1.IntentRun{}
		if err := c.Get(ctx, key, r); err != nil {
			t.Fatalf("Get(plan run) = %v", err)
		}
		r.Status = patchyv1.IntentRunStatus{Phase: patchyv1.RunRunning, BaseSHA: schemaSHA,
			Trees: treeRecords(patchyv1.MaxIntentRunTrees)}
		want := r.Status.DeepCopy()
		if err := c.Status().Update(ctx, r); err != nil {
			t.Fatalf("Status().Update(tree records) = %v, want nil", err)
		}
		if err := c.Get(ctx, key, r); err != nil {
			t.Fatalf("Get(plan run) = %v", err)
		}
		sameJSON(t, "intent run tree records", r.Status, *want)
	})

	for _, tt := range []struct {
		name   string
		mutate func(*patchyv1.IntentRunStatus)
	}{
		{"eight tree records", func(s *patchyv1.IntentRunStatus) {
			s.Trees = treeRecords(patchyv1.MaxIntentRunTrees + 1)
		}},
		{"a tree recorded twice", func(s *patchyv1.IntentRunStatus) { s.Trees = append(s.Trees, s.Trees[0]) }},
		{"a tree without its commit", func(s *patchyv1.IntentRunStatus) { s.Trees[0].BaseSHA = "" }},
		{"a tree at a short commit", func(s *patchyv1.IntentRunStatus) { s.Trees[0].BaseSHA = "abc123" }},
		// The digest is recorded as the Repository holds it: bare hex.
		{"a tree digest with a prefix", func(s *patchyv1.IntentRunStatus) {
			s.Trees[0].ArtifactDigest = "sha256:" + strings.Repeat("f", 64)
		}},
		{"a short tree digest", func(s *patchyv1.IntentRunStatus) { s.Trees[0].ArtifactDigest = "abc" }},
	} {
		t.Run("status with "+tt.name, func(t *testing.T) {
			r := &patchyv1.IntentRun{}
			if err := c.Get(ctx, key, r); err != nil {
				t.Fatalf("Get(plan run) = %v", err)
			}
			r.Status.Trees = treeRecords(1)
			tt.mutate(&r.Status)
			if err := c.Status().Update(ctx, r); err == nil {
				t.Errorf("Status().Update(intent run: %s) = nil, want rejection", tt.name)
			}
		})
	}
}
