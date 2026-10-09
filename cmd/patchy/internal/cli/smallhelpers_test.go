// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"strings"
	"testing"
)

// TestCheckProjectCommandMissingProject drives `check project` through the
// command tree with its real dependencies: a Project that does not exist is
// reported (exit 3) before any of them reaches GitHub, a registry or DNS.
func TestCheckProjectCommandMissingProject(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "gh-test-token")
	t.Setenv("GH_HOST", "ghe.example.test")
	h := newHarness(t)
	err := h.execRoot(t, "check", "project", "absent")
	if exitCode(err) != ExitNotFound {
		t.Fatalf("err = %v (exit %d), want exit %d", err, exitCode(err), ExitNotFound)
	}
	if !strings.Contains(err.Error(), "project absent in namespace patchy") {
		t.Errorf("err = %v, want the project and namespace named", err)
	}
	if strings.Contains(h.out.String()+h.errOut.String(), "gh-test-token") {
		t.Error("the GitHub token was printed")
	}
}

func TestCompletionDirectives(t *testing.T) {
	cases := []struct {
		args      []string
		wantFirst string
		directive string
	}{
		// init app's one argument is a directory.
		{[]string{"__complete", "init", "app", "--registry", "r", ""}, ":16", ":16"},
		{[]string{"__complete", "init", "app", "--registry", "r", "dir", ""}, ":4", ":4"},
		// -o completes from the format list, never files.
		{[]string{"__complete", "get", "findings", "-o", ""}, "table", ":4"},
		{[]string{"__complete", "get", ""}, "all", ":4"},
		{[]string{"__complete", "describe", ""}, "finding", ":4"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args[1:], " "), func(t *testing.T) {
			out, err := execDev(t, tc.args...)
			if err != nil {
				t.Fatalf("complete: %v", err)
			}
			if first := strings.SplitN(out, "\n", 2)[0]; first != tc.wantFirst {
				t.Errorf("first line = %q, want %q\n%s", first, tc.wantFirst, out)
			}
			if !strings.Contains(out, tc.directive+"\n") {
				t.Errorf("directive %s missing:\n%s", tc.directive, out)
			}
		})
	}
}

func TestKeptWording(t *testing.T) {
	if got := keptAs(1); got != "it is" {
		t.Errorf("keptAs(1) = %q", got)
	}
	for _, n := range []int{0, 2, 7} {
		if got := keptAs(n); got != "they are" {
			t.Errorf("keptAs(%d) = %q", n, got)
		}
	}
	if got := keptDeclares(""); got != "" {
		t.Errorf("keptDeclares(\"\") = %q", got)
	}
	if got := keptDeclares("1.2.3"); got != ", and .patchy/agent.yaml still declares 1.2.3" {
		t.Errorf("keptDeclares = %q", got)
	}
}

func TestSplitLast(t *testing.T) {
	cases := []struct{ in, parent, last string }{
		{"ghcr.io/acme/charts/demo", "ghcr.io/acme/charts", "demo"},
		{"demo", "", "demo"},
		{"a/", "a", ""},
	}
	for _, tc := range cases {
		parent, last := splitLast(tc.in)
		if parent != tc.parent || last != tc.last {
			t.Errorf("splitLast(%q) = %q, %q; want %q, %q", tc.in, parent, last, tc.parent, tc.last)
		}
	}
}

func TestRunName(t *testing.T) {
	if got := runName("fnd-1", "investigation", 2); got != "fnd-1-inv-2" {
		t.Errorf("runName inv = %q", got)
	}
	if got := runName("fnd-1", "remediation", 3); got != "fnd-1-rem-3" {
		t.Errorf("runName rem = %q", got)
	}
}
