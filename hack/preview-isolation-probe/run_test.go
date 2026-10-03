// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package probe_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The site these tests name: an image in account 222222222222, which is not
// the account of any credentials the run has (it has none; aws is a stub).
const (
	imageAccount = "222222222222"
	imageRepo    = "patchy/previews/hello-web"
	image        = imageAccount + ".dkr.ecr.us-east-1.amazonaws.com/" + imageRepo
	headSHA      = "0123456789abcdef0123456789abcdef01234567"
	imageDigest  = "sha256:89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567"
)

// stub stands in for gh, aws and kubectl. It logs each call's argv, one
// tab-separated line per call, and answers the two GitHub reads and the ECR
// read from the environment. Every kubectl call fails, so a run that gets
// past the GitHub and ECR checks stops at its first cluster read, the
// NodeClass check, having created nothing.
const stub = `#!/bin/sh
name=${0##*/}
{ printf '%s' "$name"; for arg in "$@"; do printf '\t%s' "$arg"; done; printf '\n'; } >>"$STUB_LOG"
case "$name $1 $2" in
"gh repo view") printf '%s\n' "$STUB_REPO_JSON" ;;
"gh pr view") printf '%s\n' "$STUB_PR_JSON" ;;
"aws ecr describe-images") printf '%s\n' "$STUB_DIGEST" ;;
*) exit 1 ;;
esac
`

// probeRun is one run.sh run against the stubs.
type probeRun struct {
	calls          [][]string // each stub call's argv, the command name first
	stdout, stderr string
	code           int
}

// called returns the argv of every call to name with first argument verb.
func (r probeRun) called(name, verb string) [][]string {
	var out [][]string
	for _, call := range r.calls {
		if call[0] == name && len(call) > 1 && call[1] == verb {
			out = append(out, call)
		}
	}
	return out
}

// runProbe runs run.sh with args, gh answering repoJSON for the repository
// and prJSON for the PR. The caller's PROBE_* variables are dropped so they
// cannot leak into the run.
func runProbe(t *testing.T, repoJSON, prJSON string, args ...string) probeRun {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH; run.sh requires it")
	}
	bin := t.TempDir()
	for _, name := range []string{"gh", "aws", "kubectl"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(t.TempDir(), "calls")
	env := []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"STUB_LOG=" + log,
		"STUB_REPO_JSON=" + repoJSON,
		"STUB_PR_JSON=" + prJSON,
		"STUB_DIGEST=" + imageDigest,
	}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "PROBE_") && !strings.HasPrefix(kv, "STUB_") {
			env = append(env, kv)
		}
	}
	cmd := exec.Command(bash, append([]string{"run.sh"}, args...)...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	var run probeRun
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run.sh: %v", err)
		}
		run.code = exit.ExitCode()
	}
	run.stdout, run.stderr = stdout.String(), stderr.String()
	logged, err := os.ReadFile(log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(logged), "\n"), "\n") {
		if line != "" {
			run.calls = append(run.calls, strings.Split(line, "\t"))
		}
	}
	return run
}

// siteArgs names the test site; the PR number comes last.
func siteArgs(extra ...string) []string {
	site := []string{"--repository", "acme/hello-web", "--image", image, "--taint-key", "preview.example.com/preview-only"}
	return slices.Concat(site, extra, []string{"7"})
}

func repoJSON(defaultBranch string) string {
	return `{"defaultBranchRef":{"name":"` + defaultBranch + `"}}`
}

func prJSON(base string) string {
	return `{"state":"OPEN","isCrossRepository":false,"headRefName":"test/preview-isolation",` +
		`"headRefOid":"` + headSHA + `","baseRefName":"` + base + `"}`
}

// flag returns the value after name in argv, or "" when it is absent.
func flag(argv []string, name string) string {
	if i := slices.Index(argv, name); i >= 0 && i+1 < len(argv) {
		return argv[i+1]
	}
	return ""
}

// TestImageLookupIsInTheImageAccount: the ECR check must look in the account
// named in --image, which the probe Pod pulls from. Regression: it passed
// only --region, so it read the default registry of whichever account the
// caller's credentials were in, and the gate could pass or fail against a
// registry the cluster never pulls from.
func TestImageLookupIsInTheImageAccount(t *testing.T) {
	run := runProbe(t, repoJSON("main"), prJSON("main"), siteArgs()...)
	lookups := run.called("aws", "ecr")
	if len(lookups) != 1 {
		t.Fatalf("aws ecr calls = %q, want one describe-images; stderr:\n%s", lookups, run.stderr)
	}
	got := lookups[0]
	for name, want := range map[string]string{
		"--registry-id":     imageAccount,
		"--region":          "us-east-1",
		"--repository-name": imageRepo,
		"--image-ids":       "imageTag=sha-" + headSHA,
	} {
		if v := flag(got, name); v != want {
			t.Errorf("describe-images %s = %q, want %q (argv %q)", name, v, want, got)
		}
	}
	// The digest was accepted and the run went on to its first cluster read.
	if !strings.Contains(run.stderr, "Using disposable PR #7 head "+headSHA+" ("+imageDigest+")") ||
		!strings.Contains(run.stderr, "preview NodeClass is not DefaultDeny") || run.code != 1 {
		t.Errorf("run did not reach the NodeClass check after the lookup: exit %d, stderr:\n%s", run.code, run.stderr)
	}
}

// TestPRMustBeIntoTheDefaultBranch: the disposable PR must be open into the
// repository's own default branch, read from GitHub. Regression: the guard
// required a base of "main", so a repository whose default branch is another
// name could never be probed, and nothing could get past that.
func TestPRMustBeIntoTheDefaultBranch(t *testing.T) {
	for _, tc := range []struct {
		name, defaultBranch, base string
		want                      string // "" when the guard admits the PR
	}{
		{"main into main", "main", "main", ""},
		{"trunk into trunk", "trunk", "trunk", ""},
		{"master into master", "master", "master", ""},
		{"trunk default, into main", "trunk", "main", "test/preview-* PR into trunk"},
		{"main default, into a release branch", "main", "release/1.x", "test/preview-* PR into main"},
		{"no default branch", "", "main", "could not read the default branch of acme/hello-web"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runProbe(t, repoJSON(tc.defaultBranch), prJSON(tc.base), siteArgs()...)
			if repos := run.called("gh", "repo"); len(repos) != 1 || flag(repos[0], "view") != "acme/hello-web" {
				t.Errorf("gh repo calls = %q, want one view of acme/hello-web", repos)
			}
			lookups := run.called("aws", "ecr")
			if tc.want == "" {
				if len(lookups) != 1 {
					t.Errorf("PR into %s with default %s was refused: exit %d, stderr:\n%s",
						tc.base, tc.defaultBranch, run.code, run.stderr)
				}
				return
			}
			if run.code != 1 || !strings.Contains(run.stderr, tc.want) {
				t.Errorf("exit %d, stderr:\n%s\nwant exit 1 with %q", run.code, run.stderr, tc.want)
			}
			if len(lookups) != 0 {
				t.Errorf("a refused PR still reached ECR: %q", lookups)
			}
		})
	}
}

// TestDryRunNamesTheImageAccountAndCallsNothing: the dry run prints the
// account the lookup will be bound to and calls none of gh, aws or kubectl.
func TestDryRunNamesTheImageAccountAndCallsNothing(t *testing.T) {
	run := runProbe(t, repoJSON("main"), prJSON("main"), siteArgs("--dry-run")...)
	if run.code != 0 {
		t.Fatalf("dry run exit %d, stderr:\n%s", run.code, run.stderr)
	}
	if want := "(account " + imageAccount + ", region us-east-1)"; !strings.Contains(run.stdout, want) {
		t.Errorf("dry run printed:\n%s\nwant the registry line to say %q", run.stdout, want)
	}
	if len(run.calls) != 0 {
		t.Errorf("dry run called %q", run.calls)
	}
}
