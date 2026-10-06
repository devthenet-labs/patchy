// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build !unix

package agentrun

import "os"

// openRegular opens path for reading when it is a regular file, or returns
// nil. agent-runner runs in a Linux pod; this keeps the package building
// where the CLI is (cmd/patchy imports it).
func openRegular(path string) *os.File {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil
	}
	return file
}
