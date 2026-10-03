// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConflictError is a refusal to write: the paths in the way.
type ConflictError struct {
	// Paths already exist as regular files; force overwrites them.
	Paths []string
	// Unsafe are paths force cannot overwrite either: not a regular file,
	// or reached through a symbolic link or a non-directory.
	Unsafe []string
}

func (e *ConflictError) Error() string {
	var parts []string
	if n := len(e.Paths); n > 0 {
		verb, s := "exists", ""
		if n > 1 {
			verb, s = "exist", "s"
		}
		parts = append(parts, fmt.Sprintf("%d file%s already %s: %s", n, s, verb, strings.Join(e.Paths, ", ")))
	}
	if len(e.Unsafe) > 0 {
		parts = append(parts, "not a regular file, or behind a symbolic link, so never overwritten: "+
			strings.Join(e.Unsafe, ", "))
	}
	return strings.Join(parts, "; ")
}

// Write writes files under dir, creating directories as needed. It checks
// every path before writing any: an existing file is a conflict unless
// force is set, and a path that is not a regular file, or that a symbolic
// link or a non-directory leads to, is a conflict whatever force says. A
// conflict writes nothing and returns a *ConflictError.
func Write(dir string, files []File, force bool) error {
	conflict := &ConflictError{}
	for _, f := range files {
		switch state, err := inspect(dir, f.Path); {
		case err != nil:
			return err
		case state == stateUnsafe:
			conflict.Unsafe = append(conflict.Unsafe, f.Path)
		case state == stateFile && !force:
			conflict.Paths = append(conflict.Paths, f.Path)
		}
	}
	if len(conflict.Paths) > 0 || len(conflict.Unsafe) > 0 {
		return conflict
	}
	for _, f := range files {
		target := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, f.Data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// pathState is what inspect found at a path.
type pathState int

const (
	stateAbsent pathState = iota
	stateFile
	stateUnsafe
)

// inspect reports what is at rel under dir: nothing, a regular file, or
// something unsafe to write through (a symbolic link anywhere on the way,
// a parent that is not a directory, or a final entry that is not a
// regular file).
func inspect(dir, rel string) (pathState, error) {
	parts := strings.Split(rel, "/")
	current := dir
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return stateAbsent, nil
		}
		if err != nil {
			return 0, err
		}
		last := i == len(parts)-1
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return stateUnsafe, nil
		case !last && !info.IsDir():
			return stateUnsafe, nil
		case last && !info.Mode().IsRegular():
			return stateUnsafe, nil
		}
	}
	return stateFile, nil
}
