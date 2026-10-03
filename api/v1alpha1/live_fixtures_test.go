// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	patchyv1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// The live fixtures are the objects devthenet-dev held before the slice-3
// schema change: both Projects (one with the spec.preview shorthand), three
// Intents, two IntentRuns, and the Preview the slice-2 writer kept. Every one
// must stay valid, readable by the Go types, and unchanged by the new
// schema's defaults, so neither a stored object nor an unchanged controller
// writing its old shape is refused or rewritten after the upgrade.
const liveFixtures = "testdata/live"

// liveFixtureCount pins the fixture set, so a fixture lost from the
// directory fails the test rather than silently shrinking it.
const liveFixtureCount = 8

func readLiveFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(liveFixtures, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != liveFixtureCount {
		t.Fatalf("%d live fixtures, want %d", len(paths), liveFixtureCount)
	}
	out := make(map[string][]byte, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(p)] = b
	}
	return out
}

// decodeLive decodes a fixture into its Go type, refusing a field the type
// does not know: a controller reading the live object sees all of it.
func decodeLive(t *testing.T, name string, raw []byte, into any) {
	t.Helper()
	if err := yaml.UnmarshalStrict(raw, into); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// TestLiveFixturesDeriveTheirPreview: the shared derivation, applied to the
// live preview-demo Project and an Intent of it in review at its last head,
// gives exactly the component the slice-2 writer wrote — so preview-controller,
// which now re-derives through it, accepts every Preview the unchanged writer
// keeps, and the writer will not rewrite them once it derives too.
func TestLiveFixturesDeriveTheirPreview(t *testing.T) {
	fixtures := readLiveFixtures(t)
	var project patchyv1.Project
	decodeLive(t, "project", fixtures["project_preview-demo.yaml"], &project)
	var in patchyv1.Intent
	decodeLive(t, "intent", fixtures["intent_preview-demo-5.yaml"], &in)
	var preview patchyv1.Preview
	decodeLive(t, "preview", fixtures["preview_preview-demo-5.yaml"], &preview)

	in.Status.Phase = patchyv1.IntentInReview
	in.Status.PullRequests[0].State = "open"
	got, ok := patchyv1.DesiredPreviewComponents(&project, &in)
	if !ok || !reflect.DeepEqual(got, preview.Spec.Components) {
		t.Errorf("derived %+v, %v; the slice-2 writer wrote %+v", got, ok, preview.Spec.Components)
	}
	var target patchyv1.Project
	decodeLive(t, "project", fixtures["project_target.yaml"], &target)
	if previews := patchyv1.EffectivePreviews(&target); len(previews) != 0 {
		t.Errorf("target, never previewed, previews %+v", previews)
	}
}

// testLiveObjectsRoundTrip creates every live fixture under the current
// schema, writes its status back through the subresource, and reads both
// back unchanged: nothing refused, nothing pruned, and no new default
// injected (in particular no path on the Preview's component, which an
// unchanged writer comparing specs would otherwise rewrite every pass).
// Then it updates each unchanged, so every transition rule (oldSelf) is
// evaluated against the stored object too.
func testLiveObjectsRoundTrip(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "patchy"}}); err != nil &&
		!kerrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace patchy: %v", err)
	}
	for name, raw := range readLiveFixtures(t) {
		t.Run(name, func(t *testing.T) {
			js, err := yaml.YAMLToJSON(raw)
			if err != nil {
				t.Fatal(err)
			}
			want := &unstructured.Unstructured{}
			if err := want.UnmarshalJSON(js); err != nil {
				t.Fatal(err)
			}
			obj := want.DeepCopy()
			if err := c.Create(ctx, obj); err != nil {
				t.Fatalf("Create(%s) = %v, want the live object admitted", name, err)
			}
			if status, ok := want.Object["status"]; ok {
				obj.Object["status"] = status
				if err := c.Status().Update(ctx, obj); err != nil {
					t.Fatalf("Status().Update(%s) = %v, want the live status admitted", name, err)
				}
			}
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(want.GroupVersionKind())
			if err := c.Get(ctx, client.ObjectKeyFromObject(want), got); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"spec", "status"} {
				sameJSON(t, name+" "+field, got.Object[field], want.Object[field])
			}
			if err := c.Update(ctx, got); err != nil {
				t.Errorf("Update(unchanged %s) = %v, want nil", name, err)
			}
			if err := c.Status().Update(ctx, got); err != nil {
				t.Errorf("Status().Update(unchanged %s) = %v, want nil", name, err)
			}
		})
	}
}
