// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

var update = flag.Bool("update", false, "rewrite the golden trees under testdata/golden")

// docAgentBase is an agent base in documentation values: a registry no one
// runs, pinned by a digest no registry holds. The golden trees are linted
// and their tests run (hack/scaffold-check.sh), but never built.
const docAgentBase = "registry.example.com/patchy/agent-base:v1.2.3@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// testOptions are the options the golden trees are rendered from, in
// documentation values: a repository name that is no image name, so the
// slug is visibly derived.
func testOptions(existing bool) Options {
	return Options{
		Repo:        Repo{Owner: "acme", Name: "Hello.Web"},
		Lang:        LangGo,
		ImageName:   "hello-web",
		Registry:    "123456789012.dkr.ecr.eu-west-2.amazonaws.com",
		AgentPrefix: DefaultAgentPrefix,
		Branch:      DefaultBranch,
		Images:      Images{AgentBase: docAgentBase, Go: GoImage, Runtime: RuntimeImage},
		Existing:    existing,
	}
}

// plan renders o and fails the test on an error.
func plan(t *testing.T, o Options) []File {
	t.Helper()
	files, err := Plan(o)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return files
}

// byPath indexes files by path.
func byPath(files []File) map[string]string {
	m := make(map[string]string, len(files))
	for _, f := range files {
		m[f.Path] = string(f.Data)
	}
	return m
}

// TestGolden pins every generated file, byte for byte, for a new
// application and for --existing. hack/scaffold-check.sh lints the same
// trees and runs their own tests, so a template change that breaks the
// generated workflows, scripts or service fails there.
func TestGolden(t *testing.T) {
	for name, existing := range map[string]bool{"app": false, "existing": true} {
		t.Run(name, func(t *testing.T) {
			got := byPath(plan(t, testOptions(existing)))
			dir := filepath.Join("testdata", "golden", name)
			if *update {
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				if err := Write(dir, plan(t, testOptions(existing)), false); err != nil {
					t.Fatal(err)
				}
			}
			want := map[string]string{}
			err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				data, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(dir, p)
				if err != nil {
					return err
				}
				want[filepath.ToSlash(rel)] = string(data)
				return nil
			})
			if err != nil {
				t.Fatalf("read golden tree %s (run with -update to create): %v", dir, err)
			}
			for p, data := range got {
				switch w, ok := want[p]; {
				case !ok:
					t.Errorf("%s is generated but not in %s (run with -update to accept)", p, dir)
				case w != data:
					t.Errorf("%s differs from %s (run with -update to accept):\n--- want\n%s\n--- got\n%s", p, dir, w, data)
				}
			}
			for p := range want {
				if _, ok := got[p]; !ok {
					t.Errorf("%s is in %s but no longer generated (run with -update to accept)", p, dir)
				}
			}
		})
	}
}

// TestPlanFileSets: a new application gets the whole contract; --existing
// only .patchy/ and the CI publishers, with the runtime image built by a
// workflow of its own that the guard and the dispatcher name.
func TestPlanFileSets(t *testing.T) {
	app := byPath(plan(t, testOptions(false)))
	for _, p := range []string{
		".patchy/agent.yaml", ".patchy/Dockerfile", "Dockerfile", ".dockerignore", ".gitignore", "README.md",
		"go.mod", "main.go", ".github/workflows/ci.yml", ".github/workflows/agent-image.yml",
		".github/workflows/publish-images.yml", ".github/workflows/publish-runtime.yml",
		".github/workflows/publish-agent.yml", ".github/actions/publish/guard.cjs",
		".github/actions/publish/validate_oci.py", ".github/actions/publish/copy-image.sh",
		".github/actions/publish/check-config.sh", ".github/actions/publish/README.md",
	} {
		if _, ok := app[p]; !ok {
			t.Errorf("a new application lacks %s", p)
		}
	}
	if !strings.Contains(app[".github/workflows/publish-images.yml"], "workflows: [test, agent image]") {
		t.Errorf("the dispatcher does not listen for test")
	}

	existing := byPath(plan(t, testOptions(true)))
	for p := range existing {
		if !strings.HasPrefix(p, ".patchy/") && !strings.HasPrefix(p, ".github/") {
			t.Errorf("--existing writes %s, outside .patchy/ and .github/", p)
		}
	}
	if _, ok := existing[".github/workflows/ci.yml"]; ok {
		t.Error("--existing writes ci.yml, which an application's own CI may already be")
	}
	for p, want := range map[string]string{
		".github/workflows/runtime-image.yml":  "name: runtime image\n",
		".github/workflows/publish-images.yml": "workflows: [runtime image, agent image]",
		".github/actions/publish/guard.cjs":    "runtime: '.github/workflows/runtime-image.yml'",
	} {
		if !strings.Contains(existing[p], want) {
			t.Errorf("--existing %s lacks %q", p, want)
		}
	}
	if strings.Contains(existing[".github/workflows/runtime-image.yml"], "go test") {
		t.Error("--existing runtime-image.yml tests the application, which its own CI does")
	}
}

// TestPlanDeclaration: .patchy/agent.yaml declares the agent image at its
// first toolchain tag, read the way source-controller reads it.
func TestPlanDeclaration(t *testing.T) {
	for _, existing := range []bool{false, true} {
		o := testOptions(existing)
		data := []byte(byPath(plan(t, o))[runnerimage.AgentYAMLPath])
		decl, err := runnerimage.Declare(runnerimage.Files{
			AgentYAML: runnerimage.File{Present: true, Size: int64(len(data)), Data: data},
		})
		if err != nil {
			t.Fatalf("Declare: %v", err)
		}
		want := "123456789012.dkr.ecr.eu-west-2.amazonaws.com/patchy/app-envs/hello-web:toolchain-v1"
		if decl.Image != want || o.AgentImage() != want {
			t.Errorf("declared %q (AgentImage %q), want %q", decl.Image, o.AgentImage(), want)
		}
	}
}

// TestPlanBranch: the default branch reaches every place that pins one, and
// main appears in none of them when it is not the default branch.
func TestPlanBranch(t *testing.T) {
	o := testOptions(false)
	o.Branch = "trunk"
	files := byPath(plan(t, o))
	for p, want := range map[string]string{
		".github/workflows/ci.yml":              "branches: ['trunk']",
		".github/workflows/agent-image.yml":     "github.ref == 'refs/heads/trunk'",
		".github/workflows/publish-agent.yml":   "github.ref == 'refs/heads/trunk'",
		".github/workflows/publish-runtime.yml": "github.ref == 'refs/heads/trunk'",
		".github/actions/publish/guard.cjs":     "const BRANCH = 'trunk';",
		".github/actions/publish/README.md":     "publish-agent.yml@refs/heads/trunk",
		"README.md":                             "publish-runtime.yml@refs/heads/trunk",
	} {
		if !strings.Contains(files[p], want) {
			t.Errorf("%s lacks %q", p, want)
		}
	}
	// "main" survives only as the publishers' name for a default-branch
	// build (IMAGE_SOURCE) and the main-<commit> tag the registry keeps.
	for p, data := range files {
		for _, main := range []string{"refs/heads/main", "branches: ['main']", "BRANCH = 'main'"} {
			if strings.Contains(data, main) {
				t.Errorf("%s still names %s", p, main)
			}
		}
	}
}

// TestNothingHardCodesTheAccount: the account, region and registry reach
// .patchy/agent.yaml, which must name the full image, and the READMEs that
// list the values to set, and nothing else; no workflow or script carries
// a role ARN or a numeric ID. The registry here appears in no fixture of
// the generated tests, so any hit is a template baking it in.
func TestNothingHardCodesTheAccount(t *testing.T) {
	for _, existing := range []bool{false, true} {
		o := testOptions(existing)
		o.Registry = "210987654321.dkr.ecr.ap-southeast-2.amazonaws.com"
		numericID := regexp.MustCompile(`\b[0-9]{6,}\b`)
		roleARN := regexp.MustCompile(`arn:aws[a-z-]*:iam::[0-9]{12}:`)
		for _, f := range plan(t, o) {
			data := string(f.Data)
			if f.Path != runnerimage.AgentYAMLPath && !strings.HasSuffix(f.Path, ".md") {
				for _, value := range []string{"210987654321", "ap-southeast-2"} {
					if strings.Contains(data, value) {
						t.Errorf("%s hard-codes %s", f.Path, value)
					}
				}
			}
			if strings.HasPrefix(f.Path, ".github/workflows/") || f.Path == ".github/actions/publish/guard.cjs" ||
				f.Path == ".github/actions/publish/copy-image.sh" || f.Path == ".github/actions/publish/check-config.sh" {
				if roleARN.MatchString(data) {
					t.Errorf("%s hard-codes a role ARN", f.Path)
				}
				if m := numericID.FindString(data); m != "" {
					t.Errorf("%s hard-codes the numeric ID %s", f.Path, m)
				}
			}
		}
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Options)
		wantErr string
	}{
		{"valid", func(*Options) {}, ""},
		{"no repository", func(o *Options) { o.Repo = Repo{} }, "must be owner/name"},
		{"unknown language", func(o *Options) { o.Lang = "cobol" }, `language "cobol" has no templates`},
		{"image name not a slug", func(o *Options) { o.ImageName = "Hello.Web" }, "lowercase letters and digits"},
		{"registry not ECR", func(o *Options) { o.Registry = "ghcr.io" }, "must be an ECR registry"},
		{"registry with a path", func(o *Options) { o.Registry += "/team" }, "must be an ECR registry"},
		{"agent prefix is the preview prefix", func(o *Options) { o.AgentPrefix = PreviewPrefix }, "overlaps"},
		{"agent prefix inside the preview prefix", func(o *Options) { o.AgentPrefix = PreviewPrefix + "/agents" },
			"overlaps"},
		{"agent prefix around the preview prefix", func(o *Options) { o.AgentPrefix = "patchy" }, "overlaps"},
		{"agent prefix not a path", func(o *Options) { o.AgentPrefix = "Team//agents/" }, "must be a registry path"},
		{"empty agent prefix", func(o *Options) { o.AgentPrefix = "" }, "must be a registry path"},
		{"bad branch", func(o *Options) { o.Branch = "a b" }, `branch "a b"`},
		{"agent base by tag", func(o *Options) { o.Images.AgentBase = "ghcr.io/acme/agent-base:v1" },
			"agent base: image \"ghcr.io/acme/agent-base:v1\" must be pinned by digest"},
		{"runtime image by tag", func(o *Options) { o.Images.Runtime = "gcr.io/distroless/static:nonroot" },
			"runtime image: image"},
		{"go image missing", func(o *Options) { o.Images.Go = "" }, "Go image: image reference is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := testOptions(false)
			tc.mutate(&o)
			err := o.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate = %v, want an error containing %q", err, tc.wantErr)
			}
			if _, perr := Plan(o); perr == nil {
				t.Error("Plan rendered options Validate refuses")
			}
		})
	}
}

// TestValidateReportsEveryProblem: one run names every bad option, not just
// the first.
func TestValidateReportsEveryProblem(t *testing.T) {
	o := testOptions(false)
	o.Registry, o.ImageName, o.Images.AgentBase = "", "", ""
	err := o.Validate()
	if err == nil {
		t.Fatal("Validate accepted three bad options")
	}
	for _, want := range []string{"registry", "image name", "agent base"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

// TestPlanIsSorted: Plan returns paths in order, each once, so the CLI's
// listing and the golden comparison are stable.
func TestPlanIsSorted(t *testing.T) {
	files := plan(t, testOptions(false))
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	if !slices.IsSorted(paths) || len(slices.Compact(slices.Clone(paths))) != len(paths) {
		t.Errorf("paths are not sorted and unique: %v", paths)
	}
}
