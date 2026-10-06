// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite golden files")

// renderSingle is every object a single-component Preview renders into a
// slot: its Deployment, Service and Ingress.
func renderSingle(s Settings, p *v1alpha1.Preview, slot int32) []client.Object {
	return []client.Object{s.deployment(p, 0, slot), s.service(p, 0, slot), s.ingress(p, slot)}
}

// TestGoldenSingleComponent pins every object a single-component Preview
// renders, byte for byte. The golden was captured from origin/main's
// preview-controller before multi-component previews: a single-component
// Preview must keep rendering exactly these objects under exactly these
// names (preview-<p>), so a live preview is not re-rendered across the
// upgrade and a rolled-back controller can still clean it up. One change
// since is deliberate: the Deployment's strategy moved from Recreate to a
// rolling update, so a redeploy keeps the previous revision serving. The
// Pod template is unchanged, so a live Deployment is patched in place and
// its Pods are not restarted (TestUpgradeMovesALiveDeploymentToRolling...).
func TestGoldenSingleComponent(t *testing.T) {
	_, p := testPreview("preview-demo-5", time.Date(2026, 10, 3, 7, 0, 0, 0, time.UTC))
	objects := renderSingle(testSettings(), p, 1)
	docs := make([]string, 0, len(objects))
	for _, obj := range objects {
		raw, err := yaml.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, string(raw))
	}
	got := strings.Join(docs, "---\n")
	path := filepath.Join("testdata", "single_component.yaml")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("update golden %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("single-component render differs from %s:\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}
