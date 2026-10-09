// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package helmchart

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"

	"github.com/bitwise-media-group/patchy/internal/mirror/ocireg"
)

// tgzFromHeaders builds a tgz from raw headers (body = content for regular
// files), for archive shapes makeTgz cannot express.
func tgzFromHeaders(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		e.hdr.Size = int64(len(e.body))
		if err := tw.WriteHeader(e.hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// pushableChart is a chart helm's registry client accepts (demoChartFiles
// carries a subchart directory without a Chart.yaml, which helm refuses).
var pushableChart = map[string]string{
	"demo/Chart.yaml":        "apiVersion: v2\nname: demo\nversion: 1.0.0\nappVersion: v2.5.0\n",
	"demo/values.yaml":       "image: ghcr.io/example/app\n",
	"demo/templates/cm.yaml": "kind: ConfigMap\n",
}

// quietRegistry starts an in-memory registry without request logging.
func quietRegistry(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(s.Close)
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

type tarEntry struct {
	hdr  *tar.Header
	body string
}

func TestExtractEntryShapes(t *testing.T) {
	tgz := tgzFromHeaders(t, []tarEntry{
		{hdr: &tar.Header{
			Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "x"},
		}},
		{hdr: &tar.Header{Typeflag: tar.TypeDir, Name: "demo/empty/", Mode: 0o755}},
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "demo/zero-mode.txt", Mode: 0}, body: "zero"},
		{hdr: &tar.Header{Typeflag: tar.TypeReg, Name: "demo/exec.sh", Mode: 0o4755}, body: "#!/bin/sh\n"},
	})
	dest := t.TempDir()
	if err := Extract(tgz, dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dest, "demo", "empty")); err != nil || !info.IsDir() {
		t.Errorf("empty dir not materialised: %v", err)
	}
	info, err := os.Stat(filepath.Join(dest, "demo", "zero-mode.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o600 != 0o600 {
		t.Errorf("zero-mode file should default to 0644, got %v", info.Mode().Perm())
	}
	info, err = os.Stat(filepath.Join(dest, "demo", "exec.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetuid != 0 {
		t.Errorf("setuid bit must be stripped, got %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(dest, "pax_global_header")); !os.IsNotExist(err) {
		t.Errorf("pax global header must not materialise a file: %v", err)
	}
}

func TestExtractErrors(t *testing.T) {
	good := makeTgz(t, map[string]string{"demo/Chart.yaml": strings.Repeat("x", 4096)})
	// Truncating inside the gzip stream yields a read error mid-archive.
	truncated := good[:len(good)/2]

	hardlink := tgzFromHeaders(t, []tarEntry{
		{hdr: &tar.Header{Typeflag: tar.TypeLink, Name: "demo/hard", Linkname: "demo/Chart.yaml"}},
	})

	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{"not gzip", []byte("plain text, not an archive"), "open archive"},
		{"truncated", truncated, ""},
		{"hardlink", hardlink, "unsupported type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Extract(tc.data, t.TempDir())
			if err == nil {
				t.Fatal("want error")
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestExtractIntoUnwritableTarget(t *testing.T) {
	dest := t.TempDir()
	// A regular file where the archive needs a directory.
	if err := os.WriteFile(filepath.Join(dest, "demo"), []byte("blocker"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgz := makeTgz(t, map[string]string{"demo/Chart.yaml": "name: demo\n"})
	if err := Extract(tgz, dest); err == nil {
		t.Error("want error when a file blocks the destination directory")
	}
	dirTgz := tgzFromHeaders(t, []tarEntry{{hdr: &tar.Header{Typeflag: tar.TypeDir, Name: "demo/sub/", Mode: 0o755}}})
	if err := Extract(dirTgz, dest); err == nil {
		t.Error("want error when a file blocks a directory entry")
	}
}

func TestTreeDiffMissingRoot(t *testing.T) {
	existing := t.TempDir()
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := TreeDiff(missing, existing); err == nil || !strings.Contains(err.Error(), "walk") {
		t.Errorf("TreeDiff(missing, x) = %v, want walk error", err)
	}
	if _, err := TreeDiff(existing, missing); err == nil || !strings.Contains(err.Error(), "walk") {
		t.Errorf("TreeDiff(x, missing) = %v, want walk error", err)
	}
}

func TestTreeDiffReportsSortedLabels(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	write := func(dir, rel, content string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(a, "same.txt", "x")
	write(b, "same.txt", "x")
	write(a, "z/only-a", "a")
	write(b, "m/only-b", "b")
	write(a, "c.txt", "1")
	write(b, "c.txt", "2")
	// Symlinks are not regular files and are ignored by the comparison.
	if err := os.Symlink("same.txt", filepath.Join(a, "link")); err != nil {
		t.Fatal(err)
	}

	got, err := TreeDiff(a, b)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"c.txt: differs", "m/only-b: only in " + b, "z/only-a: only in " + a}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("TreeDiff =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestChartYAMLAndAppVersion(t *testing.T) {
	dir := t.TempDir()
	write := func(chart, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, chart), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, chart, "Chart.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("noapp", "apiVersion: v2\nname: noapp\nversion: 1.0.0\n")
	write("padded", "name: padded\nappVersion: \"  1.2.3 \"\n")
	write("numeric", "name: numeric\nappVersion: 1.5\n")
	write("broken", "name: [unclosed\n")

	tests := []struct {
		chart   string
		want    string
		wantErr string
	}{
		{"noapp", "", ""},
		{"padded", "1.2.3", ""},
		// A non-string appVersion (YAML float) is not a usable pin.
		{"numeric", "", ""},
		{"broken", "", "parse Chart.yaml"},
		{"absent", "", "read Chart.yaml"},
	}
	for _, tc := range tests {
		t.Run(tc.chart, func(t *testing.T) {
			got, err := AppVersion(dir, tc.chart)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %v does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("AppVersion = %q, %v; want %q", got, err, tc.want)
			}
		})
	}

	m, err := ChartYAML(dir, "noapp")
	if err != nil || m["name"] != "noapp" || m["version"] != "1.0.0" {
		t.Errorf("ChartYAML = %v, %v", m, err)
	}
}

func TestPullRejectsUnknownScheme(t *testing.T) {
	p := &Puller{}
	for _, repo := range []string{"git://example.com/charts", "example.com/charts", ""} {
		_, _, err := p.Pull(context.Background(), repo, "demo", "1.0.0")
		if err == nil || !strings.Contains(err.Error(), "neither oci:// nor https://") {
			t.Errorf("Pull(%q) = %v", repo, err)
		}
	}
}

func TestPullOCIRewrite(t *testing.T) {
	host := quietRegistry(t)

	tgz := makeTgz(t, pushableChart)
	pushChart(t, host+"/proxy/upstream/demo:1.0.0", tgz)

	p := &Puller{
		Registry: ocireg.New(authn.NewMultiKeychain()),
		Rewrites: map[string]string{"upstream.example.invalid": host + "/proxy/upstream"},
	}
	data, _, err := p.Pull(context.Background(), "oci://upstream.example.invalid", "demo", "1.0.0")
	if err != nil {
		t.Fatalf("Pull through rewrite: %v", err)
	}
	if !bytes.Equal(data, tgz) {
		t.Error("rewritten pull returned different bytes")
	}

	// Absent version surfaces as a wrapped pull error.
	if _, _, err := p.Pull(context.Background(), "oci://upstream.example.invalid", "demo", "9.9.9"); err == nil ||
		!strings.Contains(err.Error(), "pull chart demo 9.9.9") {
		t.Errorf("Pull absent = %v", err)
	}
}

// pushChart pushes tgz as a helm OCI chart via Push, exercising the same
// path the mirror publishes with.
func pushChart(t *testing.T, ref string, tgz []byte) string {
	t.Helper()
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	digest, err := Push(tgz, ref)
	if err != nil {
		t.Fatalf("Push(%s): %v", ref, err)
	}
	return digest
}

func TestPushPublishesHelmArtifact(t *testing.T) {
	host := quietRegistry(t)
	ref := host + "/charts/demo:1.0.0"

	tgz := makeTgz(t, pushableChart)
	digest := pushChart(t, ref, tgz)
	if !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("Push digest = %q", digest)
	}

	reg := ocireg.New(authn.NewMultiKeychain())
	got, err := reg.Digest(context.Background(), ref)
	if err != nil || got != digest {
		t.Errorf("registry digest = %q, %v; want %q", got, err, digest)
	}
	mt, err := reg.ConfigMediaType(context.Background(), ref)
	if err != nil || mt != ChartConfigMediaType {
		t.Errorf("config media type = %q, %v", mt, err)
	}
	layer, err := reg.LayerBytes(context.Background(), ref, ChartContentLayerMediaType)
	if err != nil || !bytes.Equal(layer, tgz) {
		t.Errorf("content layer differs from the pushed archive (err %v)", err)
	}
	tags, err := crane.ListTags(host + "/charts/demo")
	if err != nil || strings.Join(tags, ",") != "1.0.0" {
		t.Errorf("tags = %v, %v", tags, err)
	}
}

func TestPushWithDockerConfig(t *testing.T) {
	host := quietRegistry(t)

	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"auths":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", cfgDir)
	if got := dockerConfigPath(); got != filepath.Join(cfgDir, "config.json") {
		t.Errorf("dockerConfigPath = %q", got)
	}
	if _, err := Push(makeTgz(t, pushableChart), host+"/charts/demo:1.0.0"); err != nil {
		t.Fatalf("Push with credentials file: %v", err)
	}
}

func TestPushErrors(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	host := quietRegistry(t)

	if _, err := Push(makeTgz(t, pushableChart), "Not A Reference!!"); err == nil ||
		!strings.Contains(err.Error(), "parse") {
		t.Errorf("bad ref: %v", err)
	}
	if _, err := Push([]byte("not a chart"), host+"/charts/bad:1.0.0"); err == nil ||
		!strings.Contains(err.Error(), "push chart to") {
		t.Errorf("invalid archive: %v", err)
	}
}

func TestDockerConfigPath(t *testing.T) {
	t.Run("DOCKER_CONFIG without config.json", func(t *testing.T) {
		t.Setenv("DOCKER_CONFIG", t.TempDir())
		if got := dockerConfigPath(); got != "" {
			t.Errorf("dockerConfigPath = %q, want empty", got)
		}
	})
	t.Run("config.json is a directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "config.json"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("DOCKER_CONFIG", dir)
		if got := dockerConfigPath(); got != "" {
			t.Errorf("dockerConfigPath = %q, want empty", got)
		}
	})
	t.Run("falls back to HOME/.docker", func(t *testing.T) {
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".docker"), 0o755); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(home, ".docker", "config.json")
		if err := os.WriteFile(want, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("DOCKER_CONFIG", "")
		t.Setenv("HOME", home)
		if got := dockerConfigPath(); got != want {
			t.Errorf("dockerConfigPath = %q, want %q", got, want)
		}
	})
}

func TestRepoPullErrors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/broken/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("entries: [not, a, map\n"))
	})
	mux.HandleFunc("/nourls/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("entries:\n  demo:\n    - version: 1.0.0\n      urls: []\n"))
	})
	mux.HandleFunc("/badurl/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("entries:\n  demo:\n    - version: 1.0.0\n      urls: [\"http://[::1\"]\n"))
	})
	mux.HandleFunc("/gone/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("entries:\n  demo:\n    - version: v1.0.0\n      urls: [\"demo-1.0.0.tgz\"]\n"))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)

	// HTTP nil: the default client reaches the local test server.
	p := &Puller{}
	tests := []struct {
		name, repo, wantErr string
	}{
		{"missing index", s.URL + "/absent", "404 Not Found"},
		{"unparseable index", s.URL + "/broken/", "parse " + s.URL + "/broken/index.yaml"},
		{"entry without urls", s.URL + "/nourls", "has no urls"},
		{"malformed archive url", s.URL + "/badurl", "parse archive url"},
		// The index matches 1.0.0 against v1.0.0; the archive itself 404s.
		{"archive missing", s.URL + "/gone", "GET " + s.URL + "/gone/demo-1.0.0.tgz: 404"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := p.Pull(context.Background(), tc.repo, "demo", "1.0.0")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Pull error %v does not contain %q", err, tc.wantErr)
			}
		})
	}

	if _, err := p.Versions(context.Background(), s.URL+"/absent", "demo"); err == nil ||
		!strings.Contains(err.Error(), "fetch") {
		t.Errorf("Versions missing index = %v", err)
	}
	if _, err := p.Versions(context.Background(), s.URL+"/broken", "demo"); err == nil ||
		!strings.Contains(err.Error(), "parse") {
		t.Errorf("Versions broken index = %v", err)
	}
	if v, err := p.Versions(context.Background(), s.URL+"/gone", "other"); err != nil || len(v) != 0 {
		t.Errorf("Versions of absent chart = %v, %v; want empty", v, err)
	}
}

func TestGetBadRequestURL(t *testing.T) {
	p := &Puller{}
	if _, err := p.get(context.Background(), "http://bad host/"); err == nil {
		t.Error("want error for an unbuildable request URL")
	}
	if _, err := resolveURL("http://[::1", "x.tgz"); err == nil || !strings.Contains(err.Error(), "parse repo url") {
		t.Errorf("resolveURL bad repo = %v", err)
	}
	got, err := resolveURL("https://charts.example.com/stable/", "../other/x.tgz")
	if err != nil || got != "https://charts.example.com/other/x.tgz" {
		t.Errorf("resolveURL relative = %q, %v", got, err)
	}
}
