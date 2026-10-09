// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build unix

package agentrun

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestOpenRegular: only a regular file at the path itself is opened. The
// agent can write where the CLI keeps a command's output, so a symbolic link
// (which could point at a secret), a FIFO (which would block the reader) or a
// directory planted there is refused rather than followed or waited on.
func TestOpenRegular(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "task.output")
	if err := os.WriteFile(regular, []byte("chunk"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.output")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo.output")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "dir.output")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}

	f := openRegular(regular)
	if f == nil {
		t.Fatal("openRegular(regular file) = nil, want it opened")
	}
	body, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || string(body) != "chunk" {
		t.Errorf("read %q, %v; want chunk", body, err)
	}

	for name, path := range map[string]string{
		"symlink":   link,
		"fifo":      fifo,
		"directory": sub,
		"missing":   filepath.Join(dir, "absent.output"),
	} {
		if f := openRegular(path); f != nil {
			_ = f.Close()
			t.Errorf("openRegular(%s) opened it, want nil", name)
		}
	}
}
