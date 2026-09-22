// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// AgentYAMLPath is the explicit, strict declaration file, relative to the
	// repository root.
	AgentYAMLPath = ".patchy/agent.yaml"
	// DevcontainerPath is the fallback declaration file, relative to the
	// repository root. Only this path is read; .devcontainer.json and
	// .devcontainer/<name>/devcontainer.json are not.
	DevcontainerPath = ".devcontainer/devcontainer.json"
	// MaxDeclarationBytes caps either declaration file. A larger file is
	// recorded as present but never read.
	MaxDeclarationBytes = 64 << 10
)

// File is one declaration file as found in the pinned tree.
type File struct {
	// Present is true when the archive carries the file at its honoured path.
	Present bool
	// Size is the size the archive records for the entry.
	Size int64
	// Data is the file's bytes, nil when absent or over MaxDeclarationBytes.
	Data []byte
}

// Files are the two declaration files ReadFiles looks for.
type Files struct {
	AgentYAML    File
	Devcontainer File
}

// ReadFiles walks a gzip-compressed tar stream (the repository archive
// source-controller stores) and returns the two declaration files, reading
// nothing else. Entries are expected under a single leading path component,
// the archive prefix GitHub emits and the prepare script strips; regular
// files only, symlinks and deeper paths are ignored. The walk stops as soon
// as both files have been seen, so the cost is bounded by their position in
// the archive, not by its size. A corrupt stream is a plain error, not a
// Rejection: the artifact, not the declaration, is at fault.
func ReadFiles(r io.Reader) (Files, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return Files{}, fmt.Errorf("runnerimage: open archive: %w", err)
	}
	defer gz.Close() //nolint:errcheck // read-only stream; nothing to flush
	tr := tar.NewReader(gz)
	var files Files
	for !files.AgentYAML.Present || !files.Devcontainer.Present {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Files{}, fmt.Errorf("runnerimage: read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		var slot *File
		switch archivePath(hdr.Name) {
		case AgentYAMLPath:
			slot = &files.AgentYAML
		case DevcontainerPath:
			slot = &files.Devcontainer
		default:
			continue
		}
		if slot.Present {
			continue
		}
		slot.Present = true
		slot.Size = hdr.Size
		if hdr.Size > MaxDeclarationBytes {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, MaxDeclarationBytes+1))
		if err != nil {
			return Files{}, fmt.Errorf("runnerimage: read %s: %w", hdr.Name, err)
		}
		if int64(len(data)) > MaxDeclarationBytes {
			// The header lied about the size; treat it as the oversize case.
			slot.Size = int64(len(data))
			continue
		}
		slot.Data = data
	}
	return files, nil
}

// archivePath strips the single leading component of an archive entry name
// and returns the repository-relative path, or "" when the name has no
// prefix component to strip.
func archivePath(name string) string {
	name = strings.TrimPrefix(name, "./")
	i := strings.IndexByte(name, '/')
	if i <= 0 {
		return ""
	}
	return name[i+1:]
}
