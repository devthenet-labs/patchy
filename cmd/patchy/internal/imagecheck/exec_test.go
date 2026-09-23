// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package imagecheck

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
)

// floodEnv makes the test binary, re-run by TestExecCommanderKeepsABoundedTail,
// flood stdout and stderr the way a hostile image's `yes` would, then print
// one last line.
const floodEnv = "PATCHY_IMAGECHECK_FLOOD"

// TestExecCommanderKeepsABoundedTail: a command that writes without end
// cannot grow the CLI's memory with it. Only the last line is ever read,
// so the tail that holds it is all that is kept.
func TestExecCommanderKeepsABoundedTail(t *testing.T) {
	if os.Getenv(floodEnv) != "" {
		for _, f := range []*os.File{os.Stdout, os.Stderr} {
			w := bufio.NewWriter(f)
			for range 1 << 20 {
				_, _ = w.WriteString("y\n")
			}
			_, _ = w.WriteString("the last line\n")
			_ = w.Flush()
		}
		os.Exit(0)
	}
	t.Setenv(floodEnv, "1")
	res, err := ExecCommander{}.Run(context.Background(), os.Args[0], "-test.run=^TestExecCommanderKeepsABoundedTail$")
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("Run = %+v, %v", res.ExitCode, err)
	}
	for stream, out := range map[string]string{"stdout": res.Stdout, "stderr": res.Stderr} {
		if len(out) > outputTail {
			t.Errorf("%s kept %d bytes, want at most %d", stream, len(out), outputTail)
		}
		if got := lastLine(out); got != "the last line" {
			t.Errorf("%s last line = %q, want the command's own last line", stream, got)
		}
		if !strings.HasSuffix(out, "y\nthe last line\n") {
			t.Errorf("%s does not end with what the command wrote last: %q", stream, out[max(0, len(out)-40):])
		}
	}
}
