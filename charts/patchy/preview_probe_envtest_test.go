// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// probeDir is the live isolation probe (hack/preview-isolation-probe), whose
// sibling run creates a stand-in component Service in slot 0.
const probeDir = "../../hack/preview-isolation-probe"

// testIsolationProbeService runs the isolation probe's own command for its
// sibling Service, word for word from run.sh, through kubectl against the
// rendered slot policies. Regression: the probe client-side applied that
// Service, and apply's last-applied annotation is refused by the slot Service
// policy, which admits no annotation but a health-check path, so the sibling
// run could never pass. The same Service applied is refused here too, which
// shows the check can fail.
func testIsolationProbeService(t *testing.T, env *envtest.Environment) {
	t.Helper()
	user, err := env.AddUser(envtest.User{Name: "probe-operator", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatalf("add kubectl user: %v", err)
	}
	kubectl, err := user.Kubectl()
	if err != nil {
		t.Fatalf("kubectl: %v", err)
	}
	dir, err := filepath.Abs(probeDir)
	if err != nil {
		t.Fatal(err)
	}
	args := probeServiceCommand(t, dir)
	// A server-side dry run: the policies decide, and nothing is stored.
	if _, stderr, err := kubectl.Run(append(args, "--dry-run=server")...); err != nil {
		t.Errorf("run.sh's sibling Service command (kubectl %s) is refused: %v: %s",
			strings.Join(args, " "), err, readAll(t, stderr))
	}
	_, stderr, err := kubectl.Run("apply", "-n", "patchy-preview-0", "-f",
		filepath.Join(dir, "sibling-service.yaml"), "--dry-run=server")
	if out := readAll(t, stderr); err == nil || !strings.Contains(out, "restricted to a safe healthcheck path") {
		t.Errorf("the sibling Service client-side applied: %v: %s; want refused for its last-applied annotation",
			err, out)
	}
}

// probeServiceCommand is the kubectl invocation run.sh creates the sibling
// Service with: its one line naming sibling-service.yaml, split into words,
// with $probe_dir expanded to dir. A line with any other shell syntax fails
// the test rather than being run differently from how bash would run it.
func probeServiceCommand(t *testing.T, dir string) []string {
	t.Helper()
	script, err := os.ReadFile(filepath.Join(dir, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var commands []string
	for line := range strings.SplitSeq(string(script), "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "sibling-service.yaml") && !strings.HasPrefix(line, "#") {
			commands = append(commands, line)
		}
	}
	if len(commands) != 1 {
		t.Fatalf("run.sh names sibling-service.yaml on %d command lines, want 1: %q", len(commands), commands)
	}
	words := strings.Fields(commands[0])
	if words[0] != "kubectl" {
		t.Fatalf("run.sh's sibling Service command %q is not a kubectl command", commands[0])
	}
	args := make([]string, 0, len(words)-1)
	for _, word := range words[1:] {
		word = strings.Trim(word, `"`)
		if strings.ContainsAny(strings.ReplaceAll(word, "$probe_dir", ""), "$`'\"\\|;&<>(){}*?") {
			t.Fatalf("run.sh's sibling Service command %q has shell syntax this test cannot reproduce", commands[0])
		}
		args = append(args, strings.ReplaceAll(word, "$probe_dir", dir))
	}
	return args
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
