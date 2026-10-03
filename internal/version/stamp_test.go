// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package version

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The two builds that stamp this package, from the repository root.
const (
	goreleaserConfig = "../../.goreleaser.yaml"
	buildScript      = "../../hack/build.sh"
)

// stampPattern matches one -X stamp of this package, in either build's
// spelling of the module path (written out in .goreleaser.yaml, $module in
// hack/build.sh), capturing the variable and the rest of its line. Each
// stamp sits on a line of its own in both, and a goreleaser value can hold
// spaces (a template), so the value runs to the end of the line.
var stampPattern = regexp.MustCompile(
	`-X (?:github\.com/bitwise-media-group/patchy|\$module)/internal/version\.(\w+)=([^\n]*)`)

// stamps is every -X stamp of this package in text, variable to value,
// less the shell's line continuation and closing quote in hack/build.sh.
func stamps(text string) map[string]string {
	out := map[string]string{}
	for _, m := range stampPattern.FindAllStringSubmatch(text, -1) {
		out[m[1]] = strings.TrimRight(m[2], ` \"`)
	}
	return out
}

// declaredStrings is every package-level string variable version.go
// declares: the only names a -X stamp can set. The linker ignores a stamp
// naming anything else, without a word.
func declaredStrings(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "version.go", nil, 0)
	if err != nil {
		t.Fatalf("parse version.go: %v", err)
	}
	vars := map[string]bool{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			typed := identName(vs.Type) == "string"
			for i, name := range vs.Names {
				if typed || (i < len(vs.Values) && isStringLiteral(vs.Values[i])) {
					vars[name.Name] = true
				}
			}
		}
	}
	return vars
}

// identName is the name of a plain identifier expression, or "" for any
// other (or none).
func identName(expr ast.Expr) string {
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// isStringLiteral reports whether expr is a string literal.
func isStringLiteral(expr ast.Expr) bool {
	lit, ok := expr.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING
}

// goreleaserBuild is one build of .goreleaser.yaml: its id and ldflags.
type goreleaserBuild struct {
	ID      string   `json:"id"`
	Ldflags []string `json:"ldflags"`
}

// goreleaser is the part of .goreleaser.yaml the stamps live in and the
// image repositories the release pushes to.
type goreleaser struct {
	Builds    []goreleaserBuild `json:"builds"`
	DockersV2 []struct {
		Images []string `json:"images"`
	} `json:"dockers_v2"`
}

func readGoreleaser(t *testing.T) goreleaser {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(goreleaserConfig))
	if err != nil {
		t.Fatalf("read %s: %v", goreleaserConfig, err)
	}
	var cfg goreleaser
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", goreleaserConfig, err)
	}
	return cfg
}

// TestStampsNameDeclaredVariables: every variable either build stamps is
// one this package declares, so renaming one cannot leave a release
// stamping nothing (the CLI would then know no runner image).
func TestStampsNameDeclaredVariables(t *testing.T) {
	declared := declaredStrings(t)
	cfg := readGoreleaser(t)
	script, err := os.ReadFile(filepath.FromSlash(buildScript))
	if err != nil {
		t.Fatalf("read %s: %v", buildScript, err)
	}
	sources := map[string]string{buildScript: string(script)}
	for _, b := range cfg.Builds {
		sources[goreleaserConfig+" build "+b.ID] = strings.Join(b.Ldflags, "\n")
	}
	for source, text := range sources {
		got := stamps(text)
		if len(got) == 0 {
			t.Errorf("%s stamps nothing into internal/version", source)
		}
		for name := range got {
			if !declared[name] {
				t.Errorf("%s stamps version.%s, which version.go does not declare as a string variable", source, name)
			}
		}
	}
}

// TestReleaseStampsRepositoriesItPublishes: the CLI's release build stamps
// the runner and agent-base repositories from the same release registry
// its images are pushed under, so the CLI names repositories its own
// release published; hack/build.sh stamps the same two leaves under its
// registry.
func TestReleaseStampsRepositoriesItPublishes(t *testing.T) {
	cfg := readGoreleaser(t)
	var images []string
	for _, d := range cfg.DockersV2 {
		images = append(images, d.Images...)
	}
	idx := slices.IndexFunc(cfg.Builds, func(b goreleaserBuild) bool { return b.ID == "patchy" })
	if idx < 0 {
		t.Fatalf("%s has no patchy build", goreleaserConfig)
	}
	release := stamps(strings.Join(cfg.Builds[idx].Ldflags, "\n"))
	script, err := os.ReadFile(filepath.FromSlash(buildScript))
	if err != nil {
		t.Fatalf("read %s: %v", buildScript, err)
	}
	local := stamps(string(script))
	for _, tc := range []struct {
		variable, leaf string
	}{
		{"RunnerImageRepository", "claude-agent-runner"},
		{"AgentBaseRepository", "agent-base"},
	} {
		t.Run(tc.variable, func(t *testing.T) {
			want := "{{ .Env.PATCHY_IMAGE_REGISTRY }}/" + tc.leaf
			if got := release[tc.variable]; got != want {
				t.Errorf("the patchy build stamps %s=%q, want %q", tc.variable, got, want)
			}
			if !slices.Contains(images, want) {
				t.Errorf("no dockers_v2 entry pushes %q, the repository the CLI is stamped with; images %q",
					want, images)
			}
			if got, want := local[tc.variable], "$registry/"+tc.leaf; got != want {
				t.Errorf("%s stamps %s=%q, want %q", buildScript, tc.variable, got, want)
			}
		})
	}
}
