// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build unix

package agentrun

import (
	"os"
	"syscall"
)

// openRegular opens path for reading when it is a regular file, or returns
// nil. It never follows a symbolic link at the path's last element nor
// waits on a FIFO: the agent can write where the CLI keeps a command's
// output, so what is there is taken for no more than it is.
func openRegular(path string) *os.File {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil
	}
	return file
}
