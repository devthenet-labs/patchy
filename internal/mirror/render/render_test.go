// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package render

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	release "helm.sh/helm/v4/pkg/release/v1"
)

// writeChart lays out a minimal chart under dir/name and returns its path.
func writeChart(t *testing.T, dir, name string, files map[string]string) string {
	t.Helper()
	chartDir := filepath.Join(dir, name)
	base := map[string]string{
		"Chart.yaml":  "apiVersion: v2\nname: " + name + "\nversion: 1.2.3\n",
		"values.yaml": "greeting: hello\n",
	}
	for k, v := range files {
		base[k] = v
	}
	for rel, content := range base {
		p := filepath.Join(chartDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return chartDir
}

const configMapTemplate = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-cm
  namespace: {{ .Release.Namespace }}
data:
  greeting: {{ .Values.greeting | quote }}
  kube: {{ .Capabilities.KubeVersion.Version | quote }}
  monitoring: {{ .Capabilities.APIVersions.Has "monitoring.coreos.com/v1" | quote }}
`

const hookTemplate = `apiVersion: v1
kind: Pod
metadata:
  name: {{ .Release.Name }}-test
  annotations:
    "helm.sh/hook": test
spec:
  containers:
  - name: t
    image: busybox
`

func TestRenderManifestCapabilitiesAndValues(t *testing.T) {
	dir := t.TempDir()
	chartDir := writeChart(t, dir, "demo", map[string]string{
		"templates/cm.yaml": configMapTemplate,
	})
	override := filepath.Join(dir, "override.yaml")
	if err := os.WriteFile(override, []byte("greeting: bonjour\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		in          Input
		wantContain []string
	}{
		{
			name: "chart defaults",
			in: Input{
				ChartDir: chartDir, ReleaseName: "demo", Namespace: "ns-a", KubeVersion: "1.34.0",
			},
			wantContain: []string{
				"# Source: demo/templates/cm.yaml",
				"name: demo-cm",
				"namespace: ns-a",
				`greeting: "hello"`,
				`kube: "v1.34.0"`,
				`monitoring: "false"`,
			},
		},
		{
			name: "values file and api versions",
			in: Input{
				ChartDir: chartDir, ReleaseName: "other", Namespace: "ns-b", KubeVersion: "1.30.2",
				APIVersions: []string{"monitoring.coreos.com/v1"},
				ValuesFiles: []string{override},
			},
			wantContain: []string{
				"name: other-cm",
				"namespace: ns-b",
				`greeting: "bonjour"`,
				`kube: "v1.30.2"`,
				`monitoring: "true"`,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Render(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			s := string(got)
			for _, w := range tc.wantContain {
				if !strings.Contains(s, w) {
					t.Errorf("output missing %q:\n%s", w, s)
				}
			}
			if !strings.HasSuffix(s, "\n") || strings.HasSuffix(s, "\n\n") {
				t.Errorf("output should end in exactly one newline: %q", s)
			}
		})
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	chartDir := writeChart(t, t.TempDir(), "demo", map[string]string{
		"templates/cm.yaml":   configMapTemplate,
		"templates/hook.yaml": hookTemplate,
	})
	in := Input{ChartDir: chartDir, ReleaseName: "demo", Namespace: "ns", KubeVersion: "1.34.0"}
	first, err := Render(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		again, err := Render(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("render not byte-stable:\n%s\nvs\n%s", first, again)
		}
	}
}

func TestRenderAppendsHooksAfterManifest(t *testing.T) {
	chartDir := writeChart(t, t.TempDir(), "demo", map[string]string{
		"templates/cm.yaml":   configMapTemplate,
		"templates/hook.yaml": hookTemplate,
	})
	got, err := Render(context.Background(), Input{
		ChartDir: chartDir, ReleaseName: "demo", Namespace: "ns", KubeVersion: "1.34.0",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	cm := strings.Index(s, "name: demo-cm")
	hookHeader := strings.Index(s, "---\n# Source: demo/templates/hook.yaml\n")
	if cm < 0 || hookHeader < 0 {
		t.Fatalf("missing manifest or hook section:\n%s", s)
	}
	if hookHeader < cm {
		t.Errorf("hook must follow the release manifest:\n%s", s)
	}
	if !strings.Contains(s[hookHeader:], "name: demo-test") {
		t.Errorf("hook body not rendered under its header:\n%s", s)
	}
	if strings.Count(s, "# Source: demo/templates/hook.yaml") != 1 {
		t.Errorf("hook should appear exactly once:\n%s", s)
	}
}

func TestRenderIncludesCRDs(t *testing.T) {
	crd := `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.com
spec:
  group: example.com
  names:
    kind: Widget
    plural: widgets
  scope: Namespaced
  versions:
  - name: v1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
`
	chartDir := writeChart(t, t.TempDir(), "demo", map[string]string{
		"crds/widget.yaml":  crd,
		"templates/cm.yaml": configMapTemplate,
	})
	got, err := Render(context.Background(), Input{
		ChartDir: chartDir, ReleaseName: "demo", Namespace: "ns", KubeVersion: "1.34.0",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(got), "name: widgets.example.com") {
		t.Errorf("CRDs should be included like helm template --include-crds:\n%s", got)
	}
}

func TestRenderErrors(t *testing.T) {
	dir := t.TempDir()
	good := writeChart(t, dir, "good", map[string]string{"templates/cm.yaml": configMapTemplate})
	failing := writeChart(t, dir, "failing", map[string]string{
		"templates/bad.yaml": `{{ fail "boom from template" }}`,
	})

	tests := []struct {
		name    string
		in      Input
		wantErr string
	}{
		{
			name:    "missing chart",
			in:      Input{ChartDir: filepath.Join(dir, "absent"), ReleaseName: "x", KubeVersion: "1.34.0"},
			wantErr: "load chart",
		},
		{
			name:    "invalid kube version",
			in:      Input{ChartDir: good, ReleaseName: "x", KubeVersion: "not-a-version"},
			wantErr: `invalid kube version "not-a-version"`,
		},
		{
			name: "missing values file",
			in: Input{
				ChartDir: good, ReleaseName: "x", KubeVersion: "1.34.0",
				ValuesFiles: []string{filepath.Join(dir, "nope.yaml")},
			},
			wantErr: "merge values",
		},
		{
			name:    "template failure",
			in:      Input{ChartDir: failing, ReleaseName: "x", Namespace: "ns", KubeVersion: "1.34.0"},
			wantErr: "boom from template",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Render(context.Background(), tc.in)
			if err == nil {
				t.Fatalf("expected error containing %q, got output:\n%s", tc.wantErr, out)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
			if out != nil {
				t.Errorf("expected nil output on error, got %q", out)
			}
		})
	}
}

func TestToV1Release(t *testing.T) {
	val := release.Release{Name: "by-value"}
	ptr := &release.Release{Name: "by-pointer"}

	got, err := toV1Release(val)
	if err != nil || got.Name != "by-value" {
		t.Errorf("value: got %v, %v", got, err)
	}
	got, err = toV1Release(ptr)
	if err != nil || got != ptr {
		t.Errorf("pointer: got %v, %v; want the same pointer", got, err)
	}
	if _, err := toV1Release("nope"); err == nil || !strings.Contains(err.Error(), "unsupported release type string") {
		t.Errorf("unsupported: got %v", err)
	}
}
