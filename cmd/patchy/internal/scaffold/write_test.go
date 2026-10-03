// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// readTree reads every regular file under dir, by slash-separated path.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWriteCreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	files := []File{{Path: "a/b/c.txt", Data: []byte("c")}, {Path: "top", Data: []byte("t")}}
	if err := Write(dir, files, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readTree(t, dir); got["a/b/c.txt"] != "c" || got["top"] != "t" || len(got) != 2 {
		t.Errorf("tree = %v", got)
	}
}

// TestWriteRefusesToOverwrite: one existing file stops the whole write, so
// nothing is half-scaffolded, and --force overwrites it.
func TestWriteRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []File{{Path: ".patchy/agent.yaml", Data: []byte("new")}, {Path: "Dockerfile", Data: []byte("new")}}
	err := Write(dir, files, false)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || !slices.Equal(conflict.Paths, []string{"Dockerfile"}) || len(conflict.Unsafe) != 0 {
		t.Fatalf("Write = %v, want a conflict on Dockerfile", err)
	}
	if !strings.Contains(err.Error(), "1 file already exists: Dockerfile") {
		t.Errorf("error = %q", err)
	}
	if got := readTree(t, dir); got["Dockerfile"] != "mine" || len(got) != 1 {
		t.Errorf("a refused write changed the tree: %v", got)
	}

	if err := Write(dir, files, true); err != nil {
		t.Fatalf("Write --force: %v", err)
	}
	if got := readTree(t, dir); got["Dockerfile"] != "new" || got[".patchy/agent.yaml"] != "new" {
		t.Errorf("--force tree = %v", got)
	}
}

// TestWriteNeverFollowsLinks: a symbolic link, at the file or on the way
// to it, and a path through a regular file are refused even with --force,
// so a scaffold never writes outside the directory or over something that
// is not a plain file.
func TestWriteNeverFollowsLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on windows")
	}
	outside := t.TempDir()
	dir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".github")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target"), filepath.Join(dir, "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".patchy"), []byte("a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "README.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := []File{
		{Path: ".github/workflows/ci.yml", Data: []byte("x")},
		{Path: ".patchy/agent.yaml", Data: []byte("x")},
		{Path: "Dockerfile", Data: []byte("x")},
		{Path: "README.md", Data: []byte("x")},
		{Path: "main.go", Data: []byte("x")},
	}
	for _, force := range []bool{false, true} {
		err := Write(dir, files, force)
		var conflict *ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("force=%v: Write = %v, want a conflict", force, err)
		}
		want := []string{".github/workflows/ci.yml", ".patchy/agent.yaml", "Dockerfile", "README.md"}
		if !slices.Equal(conflict.Unsafe, want) || len(conflict.Paths) != 0 {
			t.Errorf("force=%v: unsafe %v, paths %v; want unsafe %v", force, conflict.Unsafe, conflict.Paths, want)
		}
		if !strings.Contains(err.Error(), "never overwritten") {
			t.Errorf("error = %q", err)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("a write escaped through a link: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.go")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused write still wrote main.go: %v", err)
	}
}

func TestConflictErrorCountsFiles(t *testing.T) {
	err := &ConflictError{Paths: []string{"a", "b"}}
	if got := err.Error(); got != "2 files already exist: a, b" {
		t.Errorf("Error() = %q", got)
	}
}
