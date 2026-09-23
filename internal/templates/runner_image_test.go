// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

const (
	testDigestRef = "ghcr.io/acme/go-env@sha256:" + "cd" + "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	// buildReason is runnerimage's not-applicable reason for a
	// devcontainer.json that builds its image, verbatim.
	buildReason = "`.devcontainer/devcontainer.json` builds its image (`build`); patchy does not build images. " +
		"Publish the image and set `image`, or declare one in `.patchy/agent.yaml`, which takes precedence."
	allowlistReason = "image `docker.io/library/golang:1.26` is not under an allowlisted registry path (ghcr.io/acme/)"
)

// TestRunnerImageCommentGoldens pins the comment for every outcome a
// repository owner can meet: an accepted image from either file, before and
// after runs used it or skipped it, a devcontainer.json patchy cannot use, a
// rejection under each onReject policy, and the two ways a run on an
// accepted image can end early.
func TestRunnerImageCommentGoldens(t *testing.T) {
	accepted := func(manifest, declared string) RunnerImageComment {
		return RunnerImageComment{Manifest: manifest, Declared: declared, Image: testDigestRef, Verified: true}
	}
	used := func(c RunnerImageComment) RunnerImageComment {
		c.Used = true
		return c
	}
	rejected := RunnerImageComment{
		Manifest: ".patchy/agent.yaml", Declared: "docker.io/library/golang:1.26",
		Rejected: "NotAllowlisted", Reason: allowlistReason,
	}
	tests := []struct {
		name string
		c    RunnerImageComment
	}{
		{"runner_image_yaml.md", used(accepted(".patchy/agent.yaml", "ghcr.io/acme/go-env:1.26"))},
		// No run has launched yet.
		{"runner_image_devcontainer.md", accepted(".devcontainer/devcontainer.json", "ghcr.io/acme/dev-env:2")},
		// Runs launched, all on the default runner image.
		{"runner_image_unused.md", func() RunnerImageComment {
			c := accepted(".patchy/agent.yaml", "ghcr.io/acme/go-env:1.26")
			c.Unused = true
			return c
		}()},
		{"runner_image_not_applicable.md", RunnerImageComment{
			Manifest: ".devcontainer/devcontainer.json", NotApplicable: true, Reason: buildReason,
		}},
		{"runner_image_rejected_default.md", rejected},
		{"runner_image_rejected_handoff.md", func() RunnerImageComment {
			c := rejected
			c.Parked = true
			return c
		}()},
		// A .patchy/agent.yaml that could not be read names no reference, so
		// the check command carries a placeholder.
		{"runner_image_invalid_declaration.md", RunnerImageComment{
			Manifest: ".patchy/agent.yaml", Rejected: "InvalidDeclaration",
			Reason: "`.patchy/agent.yaml` has unknown key `build`; `image` is the only key",
		}},
		{"runner_image_incompatible.md", func() RunnerImageComment {
			c := used(accepted(".patchy/agent.yaml", "ghcr.io/acme/go-env:1.26"))
			c.Verified = false
			c.Incompatible = &RunnerImageRun{Stage: "investigation", Attempt: 2,
				Detail: "preflight: /patchy/bin/claude --version: exit status 127: " +
					"/lib/ld-linux-aarch64.so.1: not found (a musl image cannot run the claude CLI)"}
			return c
		}()},
		{"runner_image_sandbox.md", func() RunnerImageComment {
			c := accepted(".patchy/agent.yaml", "ghcr.io/acme/go-env:1.26")
			c.Unused = true
			c.SandboxRefused = &RunnerImageRun{Stage: "remediation", Attempt: 1}
			return c
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RenderRunnerImageComment(tt.c)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !strings.HasPrefix(got, RunnerImageMarker+"\n") {
				t.Errorf("comment is not headed by the sticky marker:\n%s", got)
			}
			golden(t, tt.name, got)
		})
	}
}

// TestRunnerImageCommentNeutralizesDeclaredText: whatever the declaring file
// or the image says reaches the issue as code, never as markdown — no
// mention, link or heading escapes, even from a reference built to close a
// code span.
func TestRunnerImageCommentNeutralizesDeclaredText(t *testing.T) {
	hostile := "x` @acme/everyone [pwn](https://evil.example) `"
	got, err := RenderRunnerImageComment(RunnerImageComment{
		Manifest: ".patchy/agent.yaml", Declared: hostile, Rejected: "InvalidReference",
		Reason: "image reference `" + hostile + "` contains whitespace\n```\n# heading\n```",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "``"+" "+hostile+" "+"``") {
		t.Errorf("declared reference not rendered as one code span:\n%s", got)
	}
	if !strings.Contains(got, "````text\n") {
		t.Errorf("reason not fenced beyond its own backtick runs:\n%s", got)
	}
}

// TestCodeSpanProperty: for any text, code renders one CommonMark code span
// that decodes back to the text (line breaks as spaces), and no run of
// backticks inside it is as long as its delimiter, so nothing inside can
// close it early.
func TestCodeSpanProperty(t *testing.T) {
	cfg := textConfig(20260923)
	roundTrip := func(s string) bool {
		want := strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(s)
		got := code(s)
		if want == "" {
			return got == ""
		}
		if strings.ContainsAny(got, "\r\n") {
			return false
		}
		n := longestBacktickRun(got) // the delimiter is the longest run
		delim := strings.Repeat("`", n)
		if !strings.HasPrefix(got, delim) || !strings.HasSuffix(got, delim) || len(got) < 2*n {
			return false
		}
		inner := got[n : len(got)-n]
		if strings.HasPrefix(inner, "`") || strings.HasSuffix(inner, "`") || longestBacktickRun(inner) >= n {
			return false
		}
		// CommonMark strips one space from each side when both are present
		// and the content is not all spaces.
		if len(inner) >= 2 && inner[0] == ' ' && inner[len(inner)-1] == ' ' && strings.Trim(inner, " ") != "" {
			inner = inner[1 : len(inner)-1]
		}
		return inner == want
	}
	if err := quick.Check(roundTrip, cfg); err != nil {
		t.Error(err)
	}
}

// TestFenceProperty: for any text, fence renders a block whose fence is at
// least three backticks and longer than every backtick run in the text, so
// no line of it closes the block, and the text survives intact inside.
func TestFenceProperty(t *testing.T) {
	cfg := textConfig(20260924)
	contained := func(s string) bool {
		got := fence(s)
		open, rest, ok := strings.Cut(got, "text\n")
		if !ok || len(open) < 3 || strings.Trim(open, "`") != "" {
			return false
		}
		body, ok := strings.CutSuffix(rest, "\n"+open+"\n")
		if !ok || longestBacktickRun(body) >= len(open) {
			return false
		}
		return body == strings.TrimRight(s, "\r\n")
	}
	if err := quick.Check(contained, cfg); err != nil {
		t.Error(err)
	}
}

// textConfig generates short strings dense in the characters that matter to
// code spans and fences: backticks, spaces and line breaks.
func textConfig(seed int64) *quick.Config {
	alphabet := []byte("`` \n\ra-")
	return &quick.Config{
		MaxCount: 3000,
		Rand:     rand.New(rand.NewSource(seed)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			b := make([]byte, r.Intn(16))
			for i := range b {
				b[i] = alphabet[r.Intn(len(alphabet))]
			}
			args[0] = reflect.ValueOf(string(b))
		},
	}
}
