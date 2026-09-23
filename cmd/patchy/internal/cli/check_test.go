// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/imagecheck"
)

// pushCheckImage starts an in-memory registry, pushes a linux/amd64 image
// with env and volumes as org/app:v1 and returns its reference.
func pushCheckImage(t *testing.T, env []string, volumes map[string]struct{}) (ref, host string) {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.ConfigFile(mutate.MediaType(empty.Image, types.OCIManifestSchema1), &v1.ConfigFile{
		OS: "linux", Architecture: "amd64", Config: v1.Config{Env: env, Volumes: volumes},
	})
	if err != nil {
		t.Fatal(err)
	}
	tag, err := name.NewTag(u.Host + "/org/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	return tag.String(), u.Host
}

func TestCheckImage(t *testing.T) {
	good, goodHost := pushCheckImage(t, []string{"PATH=/usr/bin:/bin"}, nil)
	volume, _ := pushCheckImage(t, []string{"PATH=/usr/bin"}, map[string]struct{}{"/cache": {}})
	cases := []struct {
		name    string
		args    []string
		wantErr string
		lines   []string
	}{
		{"accepted", []string{"check", "image", good, "--allow", goodHost + "/org/"}, "",
			[]string{"PASS  reference", "PASS  allowlist", "PASS  path       /patchy/bin:/usr/bin:/bin",
				"SKIP  signature  unsigned (allowed only with --repository-image-allow-unsigned)"}},
		{"VOLUME fails the check", []string{"check", "image", volume}, "1 check failed",
			[]string{"FAIL  volume", "VOLUME `/cache`", "SKIP  allowlist"}},
		{"disallowed registry", []string{"check", "image", good, "--allow", "ghcr.io/acme/"}, "1 check failed",
			[]string{"FAIL  allowlist", "is not under an allowlisted registry path (ghcr.io/acme/)"}},
		{"host-only allowlist entry is a usage error", []string{"check", "image", good, "--allow", "ghcr.io"},
			"--allow: registry allowlist entry `ghcr.io` must be `host/path/`", nil},
		{"unreadable key is a usage error", []string{"check", "image", good, "--cosign-key", "/nonexistent.pub"},
			"--cosign-key:", nil},
		{"one reference only", []string{"check", "image"}, "accepts 1 arg", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execDev(t, tc.args...)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("check image: %v\n%s", err, out)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("check image error = %v, want %q\n%s", err, tc.wantErr, out)
			}
			for _, want := range tc.lines {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestCheckImageUsageExitCode(t *testing.T) {
	good, _ := pushCheckImage(t, nil, nil)
	_, err := execDev(t, "check", "image", good, "--allow", "ghcr.io")
	if code := exitCode(err); code != ExitUsage {
		t.Errorf("exit code = %d, want %d for a malformed --allow", code, ExitUsage)
	}
	_, err = execDev(t, "check", "image", good, "--allow", "ghcr.io/acme/")
	if code := exitCode(err); code != ExitError {
		t.Errorf("exit code = %d, want %d for a failed check", code, ExitError)
	}
}

func TestCheckImageJSON(t *testing.T) {
	good, _ := pushCheckImage(t, []string{"PATH=/usr/bin"}, nil)
	out, err := execDev(t, "check", "image", good, "-o", "json")
	if err != nil {
		t.Fatalf("check image -o json: %v", err)
	}
	var report imagecheck.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not a JSON report: %v\n%s", err, out)
	}
	if report.Reference != good || !strings.Contains(report.Image, "@sha256:") || len(report.Checks) != 9 ||
		len(report.Platforms) != 1 || report.Platforms[0].Platform != "linux/amd64" {
		t.Errorf("report = %+v", report)
	}
}

// noDocker is a Commander on a workstation with no docker CLI.
type noDocker struct{}

func (noDocker) LookPath(string) (string, error) { return "", errors.New("not found") }

func (noDocker) Run(context.Context, string, ...string) (imagecheck.Result, error) {
	return imagecheck.Result{}, errors.New("docker is not installed")
}

// TestCheckImageRunWithoutDocker: --run on a workstation without docker
// skips the sandbox checks, cleanly, and the static verdict stands.
func TestCheckImageRunWithoutDocker(t *testing.T) {
	good, _ := pushCheckImage(t, []string{"PATH=/usr/bin"}, nil)
	var out bytes.Buffer
	opts := &Options{Out: &out, ErrOut: io.Discard, Output: "table"}
	f := &checkImageFlags{run: true, maxBytes: 1 << 30}
	if err := runCheckImage(context.Background(), opts, f, good, noDocker{}); err != nil {
		t.Fatalf("runCheckImage: %v\n%s", err, out.String())
	}
	for _, want := range []string{"SKIP  runner", "SKIP  preflight", "SKIP  bash", "SKIP  git",
		"docker CLI not found on PATH"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestCheckImageRunSkippedForUnrunnablePath: an image whose PATH the
// resolver rejects is never started, since no pod would run it.
func TestCheckImageRunSkippedForUnrunnablePath(t *testing.T) {
	bad, _ := pushCheckImage(t, []string{"PATH=relative"}, nil)
	var out bytes.Buffer
	opts := &Options{Out: &out, ErrOut: io.Discard, Output: "table"}
	f := &checkImageFlags{run: true, maxBytes: 1 << 30}
	err := runCheckImage(context.Background(), opts, f, bad, noDocker{})
	if err == nil || !strings.Contains(out.String(), "SKIP  preflight  the image's PATH is rejected") {
		t.Errorf("err = %v, output:\n%s", err, out.String())
	}
}

func TestSandboxDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	dir, err := sandboxDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || !strings.HasPrefix(filepath.Base(dir), "check-image-") {
		t.Fatalf("sandboxDir = %q (%v), want a fresh check-image- directory", dir, err)
	}
	// The directory is bind-mounted as /patchy/bin into a container running
	// as uid 65532, which native Linux docker holds to the host inode's owner
	// and mode: anything short of o+rx and 65532 cannot reach agent-runner.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Errorf("sandboxDir mode = %v, want 0755 so uid 65532 can execute what is mounted from it",
			info.Mode().Perm())
	}
}

// fakeDockerHost is a working linux/amd64 docker host whose image under
// test passes every sandbox check, its git answering `git --version` with
// gitOutput.
type fakeDockerHost struct{ gitOutput string }

// hostileGitOutput is what a hostile image's git prints: a cursor-up-and-
// erase that would paint a forged PASS over the lines above it, an OSC 52
// clipboard write, and a C1 CSI (U+009B) that encoding/json passes through
// unescaped.
const hostileGitOutput = "\x1b[2A\x1b[2K\rPASS  preflight  looks fine\x1b]52;c;ZWNobyBwd25lZA==\x07\u009b31m\n"

func (fakeDockerHost) LookPath(file string) (string, error) { return "/usr/local/bin/" + file, nil }

func (f fakeDockerHost) Run(_ context.Context, _ string, args ...string) (imagecheck.Result, error) {
	switch args[0] {
	case "version":
		return imagecheck.Result{Stdout: "linux/amd64\n"}, nil
	case "create":
		return imagecheck.Result{Stdout: "c0ffee\n"}, nil
	case "run":
		switch {
		case slices.Contains(args, "git"):
			return imagecheck.Result{Stdout: f.gitOutput}, nil
		case slices.Contains(args, "/patchy/bin/agent-runner"):
			return imagecheck.Result{Stdout: "preflight passed\n"}, nil
		}
	}
	return imagecheck.Result{}, nil
}

// TestCheckImageEscapesContainerOutput: what the image under test prints
// reaches the report inert. Its control characters are shown escaped, so
// it can neither move the cursor over the lines above it nor drive the
// terminal, and the table carries no control byte but its line breaks.
func TestCheckImageEscapesContainerOutput(t *testing.T) {
	good, _ := pushCheckImage(t, []string{"PATH=/usr/bin"}, nil)
	for _, output := range []string{"table", "json", "yaml"} {
		t.Run(output, func(t *testing.T) {
			var out bytes.Buffer
			opts := &Options{Out: &out, ErrOut: io.Discard, Output: output}
			f := &checkImageFlags{run: true, maxBytes: 1 << 30, runnerImage: "runner:test"}
			if err := runCheckImage(context.Background(), opts, f, good, fakeDockerHost{hostileGitOutput}); err != nil {
				t.Fatalf("runCheckImage: %v\n%s", err, out.String())
			}
			for i, r := range out.String() {
				if r != '\n' && r != '\t' && unicode.IsControl(r) {
					t.Fatalf("output carries control character %U at byte %d:\n%q", r, i, out.String())
				}
			}
			if output == "table" && !strings.Contains(out.String(), `\x1b[2A\x1b[2K`) {
				t.Errorf("the table does not show the escape sequence the image printed:\n%s", out.String())
			}
		})
	}
}

// pushCheckIndex pushes an image index serving linux/amd64 and linux/arm64
// as org/multi:v1 and returns its reference.
func pushCheckIndex(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	archs := []string{"amd64", "arm64"}
	adds := make([]mutate.IndexAddendum, 0, len(archs))
	for _, arch := range archs {
		img, err := mutate.ConfigFile(mutate.MediaType(empty.Image, types.OCIManifestSchema1), &v1.ConfigFile{
			OS: "linux", Architecture: arch, Config: v1.Config{Env: []string{"PATH=/usr/bin"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		adds = append(adds, mutate.IndexAddendum{Add: img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: arch}}})
	}
	idx := mutate.AppendManifests(mutate.IndexMediaType(empty.Index, types.OCIImageIndex), adds...)
	tag, err := name.NewTag(u.Host + "/org/multi:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(tag, idx); err != nil {
		t.Fatal(err)
	}
	return tag.String()
}

// TestCheckImageRunsEveryPlatform: --run on an index runs the sandbox checks
// once per platform the index serves, the docker host's first, and every
// sandbox line names the platform it ran as.
func TestCheckImageRunsEveryPlatform(t *testing.T) {
	ref := pushCheckIndex(t)
	var out bytes.Buffer
	opts := &Options{Out: &out, ErrOut: io.Discard, Output: "table"}
	f := &checkImageFlags{run: true, maxBytes: 1 << 30, runnerImage: "runner:test"}
	if err := runCheckImage(context.Background(), opts, f, ref, fakeDockerHost{"git version 2.51.0\n"}); err != nil {
		t.Fatalf("runCheckImage: %v\n%s", err, out.String())
	}
	var sandboxLines []string
	for line := range strings.Lines(out.String()) {
		if fields := strings.Fields(line); len(fields) > 2 && slices.Contains([]string{"runner", "preflight", "bash",
			"git"}, fields[1]) {
			sandboxLines = append(sandboxLines, fields[0]+" "+fields[1]+" "+fields[2])
		}
	}
	want := []string{
		"PASS runner linux/amd64", "PASS preflight linux/amd64", "PASS bash linux/amd64", "PASS git linux/amd64",
		"PASS runner linux/arm64", "PASS preflight linux/arm64", "PASS bash linux/arm64", "PASS git linux/arm64",
	}
	if !slices.Equal(sandboxLines, want) {
		t.Errorf("sandbox lines = %q, want %q\n%s", sandboxLines, want, out.String())
	}
	if !strings.Contains(out.String(), "PASS  runner     linux/arm64  agent-runner and claude from runner:test; the "+
		"docker host is not linux/arm64, so it runs emulated") {
		t.Errorf("the emulated platform's runner line is not marked emulated:\n%s", out.String())
	}
}
