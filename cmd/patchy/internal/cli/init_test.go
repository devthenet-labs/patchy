// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/scaffold"
)

// testRegistryHost is the ECR registry the init app tests scaffold for.
const testRegistryHost = "123456789012.dkr.ecr.eu-west-2.amazonaws.com"

// agentBaseDigest is the digest fakeAgentBases resolves a known tag to.
var agentBaseDigest = "sha256:" + strings.Repeat("e", 64)

// fakeAgentBases is a registry holding the tags in known, recording every
// reference it is asked to resolve.
type fakeAgentBases struct {
	known map[string]bool
	asked *[]string
}

func (f fakeAgentBases) Tags(context.Context, string) ([]string, error) {
	return nil, errors.New("init app never lists tags")
}

func (f fakeAgentBases) Digest(_ context.Context, ref string) (string, error) {
	if f.asked != nil {
		*f.asked = append(*f.asked, ref)
	}
	if !f.known[ref] {
		return "", errors.New("MANIFEST_UNKNOWN")
	}
	return agentBaseDigest, nil
}

// releaseDeps are init app's dependencies for the release v0.12.14 of the
// CLI, whose agent base the registry holds.
func releaseDeps() initAppDeps {
	return initAppDeps{
		registry: fakeAgentBases{known: map[string]bool{agentBaseRepository + ":v0.12.14": true}},
		version:  "0.12.14",
	}
}

func TestResolveAgentBase(t *testing.T) {
	pinned := "registry.example.com/patchy/agent-base:v1@sha256:" + strings.Repeat("a", 64)
	known := map[string]bool{agentBaseRepository + ":v0.12.14": true, "registry.example.com/base:v2": true}
	cases := []struct {
		name, version, override string
		want, wantErr           string
		asked                   []string
	}{
		{"a release takes its own agent base", "0.12.14", "",
			agentBaseRepository + ":v0.12.14@" + agentBaseDigest, "", []string{agentBaseRepository + ":v0.12.14"}},
		{"a v-prefixed release too", "v0.12.14", "",
			agentBaseRepository + ":v0.12.14@" + agentBaseDigest, "", []string{agentBaseRepository + ":v0.12.14"}},
		{"an unpublished release cannot resolve", "9.9.9", "", "",
			"did not resolve in the registry: MANIFEST_UNKNOWN; pass --agent-base <image>@sha256:<digest>",
			[]string{agentBaseRepository + ":v9.9.9"}},
		{"a development build has none", "dev", "", "",
			`development build (version "dev")`, nil},
		{"a pre-release has none", "0.13.0-rc.1", "", "", "development build", nil},
		{"a git describe build has none", "v0.12.14-3-gabcdef0", "", "", "development build", nil},
		{"a pinned override is used as given", "dev", pinned, pinned, "", nil},
		{"a tag override is pinned", "dev", "registry.example.com/base:v2",
			"registry.example.com/base:v2@" + agentBaseDigest, "", []string{"registry.example.com/base:v2"}},
		{"an unknown tag override says to pin one", "0.12.14", "registry.example.com/base:v3", "",
			"agent base registry.example.com/base:v3 did not resolve", []string{"registry.example.com/base:v3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			got, err := resolveAgentBase(context.Background(), fakeAgentBases{known: known, asked: &asked},
				tc.version, tc.override)
			switch {
			case tc.wantErr == "" && (err != nil || got != tc.want):
				t.Errorf("resolveAgentBase = %q, %v; want %q", got, err, tc.want)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("resolveAgentBase error = %v, want %q", err, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), "--agent-base"):
				t.Errorf("error does not say to pass --agent-base: %v", err)
			}
			if strings.Join(asked, " ") != strings.Join(tc.asked, " ") {
				t.Errorf("asked the registry for %v, want %v", asked, tc.asked)
			}
		})
	}
}

// initApp runs init app into dir with flags and deps, returning stdout and
// stderr.
func initApp(t *testing.T, f *initAppFlags, dir string, deps initAppDeps) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	opts := &Options{Out: &out, ErrOut: &errOut}
	if f.lang == "" {
		f.lang = string(scaffold.LangGo)
	}
	if f.agentPrefix == "" {
		f.agentPrefix = scaffold.DefaultAgentPrefix
	}
	err := runInitApp(context.Background(), opts, f, dir, deps)
	return out.String(), errOut.String(), err
}

// TestInitAppWritesAndRefusesToOverwrite: a scaffold writes the contract,
// lists it on stdout and the next steps on stderr; run again it writes
// nothing and says why, and --force overwrites.
func TestInitAppWritesAndRefusesToOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello-web")
	f := &initAppFlags{repo: "acme/Hello.Web", registry: testRegistryHost}
	out, errOut, err := initApp(t, f, dir, releaseDeps())
	if err != nil {
		t.Fatalf("init app: %v\n%s", err, errOut)
	}
	listed := strings.Fields(out)
	if len(listed) < 20 || listed[0] != ".dockerignore" {
		t.Errorf("stdout lists %v", listed)
	}
	for _, p := range listed {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("listed %s was not written: %v", p, err)
		}
	}
	for _, want := range []string{"wrote 24 files for acme/Hello.Web", "image name hello-web",
		"gh variable set AGENT_ROLE_ARN", "imageRepository: " + testRegistryHost + "/patchy/previews/hello-web"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	dockerfile, err := os.ReadFile(filepath.Join(dir, ".patchy", "Dockerfile"))
	if err != nil || !strings.Contains(string(dockerfile),
		"FROM "+agentBaseRepository+":v0.12.14@"+agentBaseDigest+"\n") {
		t.Errorf(".patchy/Dockerfile does not build FROM the pinned agent base: %v\n%s", err, dockerfile)
	}
	agentYAML, _ := os.ReadFile(filepath.Join(dir, ".patchy", "agent.yaml"))
	if !strings.Contains(string(agentYAML), "image: "+testRegistryHost+"/patchy/app-envs/hello-web:toolchain-v1\n") {
		t.Errorf(".patchy/agent.yaml:\n%s", agentYAML)
	}

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main // mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err = initApp(t, f, dir, releaseDeps())
	if err == nil || out != "" || !strings.Contains(err.Error(), "24 files already exist") ||
		!strings.Contains(err.Error(), "nothing was written; pass --force to overwrite, or --existing") {
		t.Errorf("second init app = %v, stdout %q; want a refusal", err, out)
	}
	if mine, _ := os.ReadFile(filepath.Join(dir, "main.go")); string(mine) != "package main // mine\n" {
		t.Errorf("a refused scaffold overwrote main.go:\n%s", mine)
	}

	f.force = true
	if _, errOut, err := initApp(t, f, dir, releaseDeps()); err != nil {
		t.Fatalf("init app --force: %v\n%s", err, errOut)
	}
	if mine, _ := os.ReadFile(filepath.Join(dir, "main.go")); string(mine) == "package main // mine\n" {
		t.Error("--force left main.go alone")
	}
}

// TestInitAppExisting: --existing adds only .patchy/ and the CI
// publishers beside the application's own files, and says what to adapt.
func TestInitAppExisting(t *testing.T) {
	dir := t.TempDir()
	own := map[string]string{"Dockerfile": "FROM scratch\n", "go.mod": "module example.com/shop\n",
		"README.md": "# shop\n", ".github/workflows/ci.yml": "name: ci\n"}
	for p, data := range own {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := &initAppFlags{repo: "acme/shop", registry: testRegistryHost, existing: true}
	out, errOut, err := initApp(t, f, dir, releaseDeps())
	if err != nil {
		t.Fatalf("init app --existing: %v\n%s", err, errOut)
	}
	for _, p := range strings.Fields(out) {
		if !strings.HasPrefix(p, ".patchy/") && !strings.HasPrefix(p, ".github/") {
			t.Errorf("--existing wrote %s", p)
		}
	}
	for p, data := range own {
		if got, _ := os.ReadFile(filepath.Join(dir, p)); string(got) != data {
			t.Errorf("--existing changed the application's %s", p)
		}
	}
	for _, want := range []string{"Adapt the application", "./Dockerfile is the runtime image runtime-image.yml builds",
		"keep both", ".github/workflows/runtime-image.yml"} {
		if !strings.Contains(errOut+out, want) {
			t.Errorf("output lacks %q:\n%s\n%s", want, out, errOut)
		}
	}
}

// TestInitAppReadsTheCheckout: without --repo the repository and default
// branch come from .git, and the image name from the repository name.
func TestInitAppReadsTheCheckout(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "refs", "remotes", "origin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, data := range map[string]string{
		".git/config": "[remote \"origin\"]\n\turl = https://github.com/acme/My_Service.git\n",
		".git/refs/remotes/origin/HEAD": "ref: refs/remotes/origin/trunk\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, errOut, err := initApp(t, &initAppFlags{registry: testRegistryHost}, dir, releaseDeps())
	if err != nil {
		t.Fatalf("init app: %v\n%s", err, errOut)
	}
	if !strings.Contains(errOut, "Next steps for acme/My_Service (image name my-service)") {
		t.Errorf("stderr:\n%s", errOut)
	}
	guard, _ := os.ReadFile(filepath.Join(dir, ".github", "actions", "publish", "guard.cjs"))
	if !strings.Contains(string(guard), "const BRANCH = 'trunk';") {
		t.Error("the default branch was not read from origin's HEAD")
	}
}

// TestInitAppRefusals: a bad invocation is a usage error that writes
// nothing and, when only the flags are wrong, asks no registry.
func TestInitAppRefusals(t *testing.T) {
	cases := []struct {
		name    string
		flags   initAppFlags
		deps    initAppDeps
		missing bool
		want    string
		usage   bool
	}{
		{"no repository to read", initAppFlags{registry: testRegistryHost}, releaseDeps(), false,
			"pass --repo owner/name", true},
		{"bad image name", initAppFlags{repo: "acme/x", registry: testRegistryHost, imageName: "My.App"},
			releaseDeps(), false, "--image-name", true},
		{"registry not ECR", initAppFlags{repo: "acme/x", registry: "ghcr.io/acme"}, releaseDeps(), false,
			"must be an ECR registry", true},
		{"agent prefix overlapping previews", initAppFlags{repo: "acme/x", registry: testRegistryHost,
			agentPrefix: "patchy/previews/agents"}, releaseDeps(), false, "overlaps the preview prefix", true},
		{"unknown language", initAppFlags{repo: "acme/x", registry: testRegistryHost, lang: "cobol"},
			releaseDeps(), false, `--lang "cobol" has no templates`, true},
		{"--existing needs the application", initAppFlags{repo: "acme/x", registry: testRegistryHost,
			existing: true}, releaseDeps(), true, "does not exist; --existing", true},
		{"a development build needs --agent-base", initAppFlags{repo: "acme/x", registry: testRegistryHost},
			initAppDeps{registry: fakeAgentBases{}, version: "dev"}, false, "pass --agent-base", false},
		{"an unpinned malformed agent base", initAppFlags{repo: "acme/x", registry: testRegistryHost,
			agentBase: "registry.example.com/base@sha256:short"}, releaseDeps(), false, "agent base:", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.missing {
				dir = filepath.Join(dir, "absent")
			}
			var asked []string
			if reg, ok := tc.deps.registry.(fakeAgentBases); ok {
				reg.asked = &asked
				tc.deps.registry = reg
			}
			_, _, err := initApp(t, &tc.flags, dir, tc.deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("init app = %v, want %q", err, tc.want)
			}
			if isUsage(err) != tc.usage {
				t.Errorf("usage error = %v, want %v: %v", isUsage(err), tc.usage, err)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("a refused init app wrote %v", entries)
			}
			if tc.usage && tc.flags.agentBase == "" && len(asked) != 0 {
				t.Errorf("bad flags still asked the registry for %v", asked)
			}
		})
	}
}

// TestInitAppNeedsARegistry: --registry is required, through the real
// command tree.
func TestInitAppNeedsARegistry(t *testing.T) {
	_, err := execDev(t, "init", "app", t.TempDir(), "--repo", "acme/x")
	if err == nil || !strings.Contains(err.Error(), `required flag(s) "registry" not set`) {
		t.Errorf("init app without --registry = %v", err)
	}
}
