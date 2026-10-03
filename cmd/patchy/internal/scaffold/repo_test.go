// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRepo(t *testing.T) {
	for in, want := range map[string]Repo{
		"acme/Hello.Web":     {"acme", "Hello.Web"},
		"a/b":                {"a", "b"},
		"Acme-Labs/x_y-z.go": {"Acme-Labs", "x_y-z.go"},
	} {
		got, err := ParseRepo(in)
		if err != nil || got != want || got.String() != in {
			t.Errorf("ParseRepo(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "acme", "acme/", "/x", "acme/x/y", "-acme/x", "ac me/x", "acme/.", "acme/..",
		"acme/a b", "acme/" + strings.Repeat("x", 101), strings.Repeat("a", 40) + "/x"} {
		if _, err := ParseRepo(bad); err == nil {
			t.Errorf("ParseRepo(%q) accepted it", bad)
		}
	}
}

func TestValidateBranch(t *testing.T) {
	for _, ok := range []string{"main", "trunk", "release/v1", "dev_2", "v1.2"} {
		if err := ValidateBranch(ok); err != nil {
			t.Errorf("ValidateBranch(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-main", "a b", "a..b", "a//b", "a/", "x.lock", "it's", `a"b`, "a$b", "a\nb",
		"${{ github.token }}"} {
		if err := ValidateBranch(bad); err == nil {
			t.Errorf("ValidateBranch(%q) accepted it", bad)
		}
	}
}

func TestRepoFromURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/Hello.Web.git":       "acme/Hello.Web",
		"https://github.com/acme/Hello.Web":           "acme/Hello.Web",
		"https://github.com/acme/Hello.Web/":          "acme/Hello.Web",
		"ssh://git@github.com/acme/Hello.Web.git":     "acme/Hello.Web",
		"git@github.com:acme/Hello.Web.git":           "acme/Hello.Web",
		"https://ghe.example.com/team/acme/hello.git": "acme/hello",
		"github.com:acme/Hello.Web.git":               "acme/Hello.Web",
	} {
		got, err := repoFromURL(in)
		if err != nil || got.String() != want {
			t.Errorf("repoFromURL(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"https://github.com/acme", "git@github.com:hello.git", "/tmp/acme/repo",
		"../acme/repo", "file:///tmp/acme/repo", "C:/src/acme/repo"} {
		if _, err := repoFromURL(bad); err == nil {
			t.Errorf("repoFromURL(%q) accepted it", bad)
		}
	}
}

// gitDir writes a .git directory with config (and origin's HEAD, when
// head is set) into a new directory and returns that directory.
func gitDir(t *testing.T, config, head string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "refs", "remotes", "origin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if head != "" {
		if err := os.WriteFile(filepath.Join(dir, ".git", "refs", "remotes", "origin", "HEAD"), []byte(head),
			0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestOriginRepo(t *testing.T) {
	config := "[core]\n\tbare = false\n" +
		"[remote \"upstream\"]\n\turl = https://github.com/other/fork.git\n" +
		"[remote \"origin\"]\n\turl = git@github.com:acme/Hello.Web.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n" +
		"[branch \"main\"]\n\tremote = origin\n"
	got, err := OriginRepo(gitDir(t, config, ""))
	if err != nil || got.String() != "acme/Hello.Web" {
		t.Errorf("OriginRepo = %v, %v; want acme/Hello.Web", got, err)
	}

	if _, err := OriginRepo(gitDir(t, "[remote \"upstream\"]\n\turl = https://github.com/a/b\n", "")); err == nil ||
		!strings.Contains(err.Error(), "no origin remote") {
		t.Errorf("OriginRepo without origin = %v", err)
	}
	if _, err := OriginRepo(t.TempDir()); !errors.Is(err, errNoGitDir) {
		t.Errorf("OriginRepo without .git = %v, want errNoGitDir", err)
	}
}

func TestOriginBranch(t *testing.T) {
	for head, want := range map[string]string{
		"ref: refs/remotes/origin/main\n":  "main",
		"ref: refs/remotes/origin/trunk\n": "trunk",
		"ref: refs/remotes/origin/a b\n":   "",
		"0123456789abcdef\n":               "",
		"":                                 "",
	} {
		got, ok := OriginBranch(gitDir(t, "", head))
		if got != want || ok != (want != "") {
			t.Errorf("OriginBranch(%q) = %q, %v; want %q", head, got, ok, want)
		}
	}
}
