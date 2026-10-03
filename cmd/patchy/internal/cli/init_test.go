// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// The release registry is stamped in at build time, which go test does not
// do: pin the one the goldens were generated with.
func init() {
	if agentBaseRepository == "" {
		agentBaseRepository = "ghcr.io/devthenet-labs/patchy/agent-base"
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

// TestResolveAgentBaseUnstamped: a release-versioned CLI built without the
// release registry stamped in (a plain go build) knows no agent base, says
// so, and asks the registry nothing.
func TestResolveAgentBaseUnstamped(t *testing.T) {
	saved := agentBaseRepository
	agentBaseRepository = ""
	t.Cleanup(func() { agentBaseRepository = saved })
	var asked []string
	_, err := resolveAgentBase(context.Background(), fakeAgentBases{asked: &asked}, "0.12.14", "")
	if err == nil || !strings.Contains(err.Error(), "built without the release registry") ||
		!strings.Contains(err.Error(), "--agent-base") {
		t.Errorf("resolveAgentBase error = %v, want the unstamped build named and --agent-base suggested", err)
	}
	if len(asked) != 0 {
		t.Errorf("asked the registry for %v, want nothing", asked)
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
		"//deploy/terraform/aws/modules/app?ref=v0.12.14\"",
		"terraform output -raw patchy_app_hello_web_variables | gh variable set -f - --repo acme/Hello.Web",
		"imageRepository: " + testRegistryHost + "/patchy/previews/hello-web"} {
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

// bumpedToolchain is a scaffold whose owner has since moved its agent
// toolchain on to toolchain-v3 with a recipe of their own, beside a
// publisher guard that a newer CLI would rewrite.
type bumpedToolchain struct {
	dir, agentYAML, dockerfile, guard string
	flags                             *initAppFlags
}

const (
	// bumpedDeclaration is bumpedToolchain's .patchy/agent.yaml.
	bumpedDeclaration = "image: " + testRegistryHost + "/patchy/app-envs/shop:toolchain-v3\n"
	// bumpedRecipe is bumpedToolchain's .patchy/Dockerfile.
	bumpedRecipe = "FROM mine\n"
	// staleGuard is bumpedToolchain's guard.cjs.
	staleGuard = "// stale\n"
)

// newBumpedToolchain scaffolds an existing application, then bumps its
// toolchain and leaves its guard stale.
func newBumpedToolchain(t *testing.T) bumpedToolchain {
	t.Helper()
	dir := t.TempDir()
	b := bumpedToolchain{
		dir:        dir,
		agentYAML:  filepath.Join(dir, ".patchy", "agent.yaml"),
		dockerfile: filepath.Join(dir, ".patchy", "Dockerfile"),
		guard:      filepath.Join(dir, ".github", "actions", "publish", "guard.cjs"),
		flags:      &initAppFlags{repo: "acme/shop", registry: testRegistryHost, existing: true},
	}
	if _, errOut, err := initApp(t, b.flags, dir, releaseDeps()); err != nil {
		t.Fatalf("init app: %v\n%s", err, errOut)
	}
	for p, data := range map[string]string{
		b.agentYAML: bumpedDeclaration, b.dockerfile: bumpedRecipe, b.guard: staleGuard,
	} {
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// readString is the content of p, "" when it cannot be read.
func readString(p string) string {
	data, _ := os.ReadFile(p)
	return string(data)
}

// TestInitAppForceKeepsTheToolchain: re-running init app --force to pick up
// newer publishers overwrites them, but never rolls the repository's own
// agent toolchain back to toolchain-v1, and says so.
func TestInitAppForceKeepsTheToolchain(t *testing.T) {
	b := newBumpedToolchain(t)
	b.flags.force = true
	out, errOut, err := initApp(t, b.flags, b.dir, releaseDeps())
	if err != nil {
		t.Fatalf("init app --force: %v\n%s", err, errOut)
	}
	if readString(b.agentYAML) != bumpedDeclaration || readString(b.dockerfile) != bumpedRecipe {
		t.Errorf("--force rewrote the toolchain:\n%s\n%s", readString(b.agentYAML), readString(b.dockerfile))
	}
	if readString(b.guard) == staleGuard {
		t.Error("--force left the publishers' guard alone")
	}
	listed := strings.Fields(out)
	if slices.Contains(listed, ".patchy/agent.yaml") || slices.Contains(listed, ".patchy/Dockerfile") ||
		!slices.Contains(listed, ".github/actions/publish/guard.cjs") {
		t.Errorf("stdout lists %v; want the publishers and not the kept toolchain", listed)
	}
	for _, want := range []string{
		"kept .patchy/agent.yaml and .patchy/Dockerfile as they are: --force never rewrites the agent toolchain, " +
			"and .patchy/agent.yaml still declares toolchain-v3",
		"toolchain-v3, the tag .patchy/agent.yaml declares",
		"--image-ids imageTag=toolchain-v3\n",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut, "toolchain-v1") {
		t.Errorf("stderr still names toolchain-v1:\n%s", errOut)
	}
}

// TestInitAppForceRefusesAMovedToolchain: with options naming another
// agent repository, the kept declaration would no longer publish, so a
// forced scaffold writes nothing.
func TestInitAppForceRefusesAMovedToolchain(t *testing.T) {
	b := newBumpedToolchain(t)
	b.flags.force, b.flags.imageName = true, "shop-v2"
	out, _, err := initApp(t, b.flags, b.dir, releaseDeps())
	if err == nil || out != "" || !strings.Contains(err.Error(), "declares "+testRegistryHost+
		"/patchy/app-envs/shop:toolchain-v3, not a toolchain-v<N> tag of "+testRegistryHost+"/patchy/app-envs/shop-v2") {
		t.Errorf("init app --force --image-name shop-v2 = %v, stdout %q; want a refusal", err, out)
	}
	if readString(b.guard) != staleGuard {
		t.Error("a refused --force still wrote the publishers")
	}
}

// TestInitAppToolchainIsGeneratedOnlyWhenAbsent: without --force the
// refusal says the toolchain would be kept, and removing it is how it is
// generated again.
func TestInitAppToolchainIsGeneratedOnlyWhenAbsent(t *testing.T) {
	b := newBumpedToolchain(t)
	_, _, err := initApp(t, b.flags, b.dir, releaseDeps())
	if err == nil || !strings.Contains(err.Error(),
		"; --force keeps .patchy/agent.yaml and .patchy/Dockerfile as they are") {
		t.Errorf("init app over a scaffold = %v; want a refusal saying --force keeps the toolchain", err)
	}
	for _, p := range []string{b.agentYAML, b.dockerfile} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	b.flags.force = true
	if _, errOut, err := initApp(t, b.flags, b.dir, releaseDeps()); err != nil {
		t.Fatalf("init app --force without a toolchain: %v\n%s", err, errOut)
	}
	if got := readString(b.agentYAML); !strings.Contains(got, "/patchy/app-envs/shop:toolchain-v1\n") {
		t.Errorf("a removed toolchain was not generated again:\n%s", got)
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
	if strings.Contains(errOut, "an earlier init app's") {
		t.Errorf("the application's own ci.yml was taken for an earlier scaffold's:\n%s", errOut)
	}
}

// TestInitAppExistingOverAFullScaffold: --existing over a tree a full
// init app wrote (a template's copy) says that the old ci.yml still builds
// a runtime image nothing publishes, and how to scaffold it properly.
func TestInitAppExistingOverAFullScaffold(t *testing.T) {
	dir := t.TempDir()
	f := &initAppFlags{repo: "acme/Hello.Web", registry: testRegistryHost}
	if _, errOut, err := initApp(t, f, dir, releaseDeps()); err != nil {
		t.Fatalf("init app: %v\n%s", err, errOut)
	}
	f = &initAppFlags{repo: "acme/Hello.Web", registry: testRegistryHost, existing: true, force: true}
	_, errOut, err := initApp(t, f, dir, releaseDeps())
	if err != nil {
		t.Fatalf("init app --existing --force: %v\n%s", err, errOut)
	}
	for _, want := range []string{".github/workflows/ci.yml is an earlier init app's",
		"delete runtime-image.yml and scaffold\n    again without --existing"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
}

// TestInitAppPinsTheModuleOnlyForARelease: the reference module's ref is
// the CLI's own release, and a development build (here with a pinned
// --agent-base) is told to pin it instead.
func TestInitAppPinsTheModuleOnlyForARelease(t *testing.T) {
	pinned := "registry.example.com/patchy/agent-base:v1@sha256:" + strings.Repeat("a", 64)
	f := &initAppFlags{repo: "acme/Hello.Web", registry: testRegistryHost, agentBase: pinned}
	_, errOut, err := initApp(t, f, t.TempDir(), initAppDeps{registry: fakeAgentBases{}, version: "dev"})
	if err != nil {
		t.Fatalf("init app: %v\n%s", err, errOut)
	}
	if !strings.Contains(errOut, "modules/app?ref=vX.Y.Z\"") || !strings.Contains(errOut, "development build: pin ref") {
		t.Errorf("a development build's next steps:\n%s", errOut)
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
		".git/config":                   "[remote \"origin\"]\n\turl = https://github.com/acme/My_Service.git\n",
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
