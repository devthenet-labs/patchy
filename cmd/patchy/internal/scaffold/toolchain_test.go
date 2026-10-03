// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

// agentRef is the agent repository testOptions publish to, with a tag.
func agentRef(tag string) string {
	o := testOptions(false)
	return o.Registry + "/" + o.AgentRepository() + ":" + tag
}

// writeFiles writes data under dir, by slash-separated path.
func writeFiles(t *testing.T, dir string, data map[string]string) {
	t.Helper()
	for p, content := range data {
		target := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// paths lists the files' paths.
func paths(files []File) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// TestKeepToolchain: an existing toolchain file is taken out of what a
// forced scaffold writes, and a kept declaration must still publish to the
// options' agent repository.
func TestKeepToolchain(t *testing.T) {
	bumped := "# mine\nimage: " + agentRef("toolchain-v3") + "\n"
	cases := []struct {
		name     string
		existing map[string]string
		kept     []string
		tag      string
		wantErr  string
	}{
		{name: "a fresh directory keeps nothing"},
		{name: "a bumped toolchain is kept whole",
			existing: map[string]string{runnerimage.AgentYAMLPath: bumped, AgentDockerfilePath: "FROM mine\n"},
			kept:     ToolchainPaths, tag: "toolchain-v3"},
		{name: "a declaration alone is kept",
			existing: map[string]string{runnerimage.AgentYAMLPath: bumped},
			kept:     []string{runnerimage.AgentYAMLPath}, tag: "toolchain-v3"},
		{name: "a recipe alone is kept",
			existing: map[string]string{AgentDockerfilePath: "FROM mine\n"},
			kept:     []string{AgentDockerfilePath}},
		{name: "the rest of the tree is not the toolchain",
			existing: map[string]string{"Dockerfile": "FROM runtime\n", ".patchy/other": "x"}},
		{name: "a declaration of another registry",
			existing: map[string]string{runnerimage.AgentYAMLPath: "image: 210987654321.dkr.ecr.eu-west-2." +
				"amazonaws.com/patchy/app-envs/hello-web:toolchain-v3\n"},
			wantErr: "not a toolchain-v<N> tag of 123456789012.dkr.ecr.eu-west-2.amazonaws.com/patchy/app-envs/hello-web"},
		{name: "a declaration of another image name",
			existing: map[string]string{runnerimage.AgentYAMLPath: "image: 123456789012.dkr.ecr.eu-west-2." +
				"amazonaws.com/patchy/app-envs/hello-web-old:toolchain-v3\n"},
			wantErr: "the agent publisher would refuse it"},
		{name: "a tag the publisher refuses",
			existing: map[string]string{runnerimage.AgentYAMLPath: "image: " + agentRef("latest") + "\n"},
			wantErr:  "not a toolchain-v<N> tag"},
		{name: "a zeroth toolchain",
			existing: map[string]string{runnerimage.AgentYAMLPath: "image: " + agentRef("toolchain-v0") + "\n"},
			wantErr:  "not a toolchain-v<N> tag"},
		{name: "a digest beside the tag",
			existing: map[string]string{runnerimage.AgentYAMLPath: "image: " + agentRef("toolchain-v3") +
				"@sha256:" + strings.Repeat("c", 64) + "\n"},
			wantErr: "not a toolchain-v<N> tag"},
		{name: "a declaration patchy cannot read",
			existing: map[string]string{runnerimage.AgentYAMLPath: "build: .\n"},
			wantErr:  "cannot be read: `.patchy/agent.yaml` has a `build` key"},
		{name: "an oversize declaration",
			existing: map[string]string{runnerimage.AgentYAMLPath: "image: " + agentRef("toolchain-v3") + "\n#" +
				strings.Repeat("x", runnerimage.MaxDeclarationBytes)},
			wantErr: "the limit is 65536 bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, tc.existing)
			files := plan(t, testOptions(false))
			rest, kept, err := KeepToolchain(dir, files, testOptions(false))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("KeepToolchain = %v, want %q", err, tc.wantErr)
				}
				if !strings.Contains(err.Error(), "remove it and .patchy/Dockerfile") {
					t.Errorf("the refusal does not say how to generate the toolchain again: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("KeepToolchain: %v", err)
			}
			if !slices.Equal(kept.Paths, tc.kept) || kept.Tag != tc.tag {
				t.Errorf("kept %v with tag %q, want %v with tag %q", kept.Paths, kept.Tag, tc.kept, tc.tag)
			}
			want := slices.DeleteFunc(paths(files), func(p string) bool { return slices.Contains(tc.kept, p) })
			if got := paths(rest); !slices.Equal(got, want) {
				t.Errorf("left %v to write, want %v", got, want)
			}
			if len(files) != len(paths(plan(t, testOptions(false)))) {
				t.Error("KeepToolchain changed the files it was given")
			}
		})
	}
}

// TestKeepToolchainLeavesUnsafePathsToWrite: a toolchain path behind a
// link is never read or kept; it stays for Write to refuse, force or not.
func TestKeepToolchainLeavesUnsafePathsToWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on windows")
	}
	outside := t.TempDir()
	writeFiles(t, outside, map[string]string{"agent.yaml": "image: " + agentRef("toolchain-v3") + "\n"})
	dir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".patchy")); err != nil {
		t.Fatal(err)
	}
	files := plan(t, testOptions(false))
	rest, kept, err := KeepToolchain(dir, files, testOptions(false))
	if err != nil || len(kept.Paths) != 0 || len(rest) != len(files) {
		t.Fatalf("KeepToolchain = %d files, kept %v, %v; want every file left and nothing kept", len(rest), kept, err)
	}
	if err := Write(dir, rest, true); err == nil || !strings.Contains(err.Error(), "never overwritten") {
		t.Errorf("Write through a linked .patchy = %v, want a refusal", err)
	}
}

// TestToolchainTagIsPublishable: the first tag is one the agent publisher
// accepts.
func TestToolchainTagIsPublishable(t *testing.T) {
	if !toolchainTagPattern.MatchString(ToolchainTag) {
		t.Errorf("ToolchainTag %q is not a toolchain-v<N> tag", ToolchainTag)
	}
}
