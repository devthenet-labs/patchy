// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// ghVariableSet is one printed `gh variable set` command: a bare value, a
// placeholder in single quotes holding no quote, or a command substitution
// in double quotes holding no double quote, so each line pastes into a
// shell as one command.
var ghVariableSet = regexp.MustCompile(`^       gh variable set ([A-Z_]+) --repo [A-Za-z0-9._/-]+ --body ` +
	`([A-Za-z0-9._/:-]+|'<[^'<>]+>'|"\$\([^"]+\)")$`)

func TestNextStepsSetsEveryVariableInOrder(t *testing.T) {
	for _, existing := range []bool{false, true} {
		o := testOptions(existing)
		steps := NextSteps(o, Present{})
		var set []string
		for line := range strings.SplitSeq(steps, "\n") {
			if !strings.Contains(line, "gh variable set") {
				continue
			}
			m := ghVariableSet.FindStringSubmatch(line)
			if m == nil {
				t.Errorf("not a pasteable gh command: %q", line)
				continue
			}
			set = append(set, m[1])
		}
		vars := Variables(o)
		want := make([]string, 0, len(vars))
		for _, v := range vars {
			want = append(want, v.Name)
		}
		// The configuration first, then the agent gate, then the preview
		// gate, last, once the Project previews the repository.
		if !slices.Equal(set, want) || set[len(set)-2] != AgentPublishEnabled || set[len(set)-1] != PreviewPublishEnabled {
			t.Errorf("existing=%v: variables set in order %v, want %v", existing, set, want)
		}
		for _, wantLine := range []string{
			"agent    123456789012.dkr.ecr.eu-west-2.amazonaws.com/patchy/app-envs/hello-web",
			"runtime  123456789012.dkr.ecr.eu-west-2.amazonaws.com/patchy/previews/hello-web",
			"imageRepository: 123456789012.dkr.ecr.eu-west-2.amazonaws.com/patchy/previews/hello-web",
			`gh workflow run "agent image" --repo acme/Hello.Web`,
			"bump the tag in\n.patchy/agent.yaml (toolchain-v2, ...)",
		} {
			if !strings.Contains(steps, wantLine) {
				t.Errorf("existing=%v: next steps lack %q:\n%s", existing, wantLine, steps)
			}
		}
	}
}

// TestNextStepsAdaptExisting: --existing ends with what the application
// must be adapted to, which depends on what it already has.
func TestNextStepsAdaptExisting(t *testing.T) {
	app := NextSteps(testOptions(false), Present{})
	if strings.Contains(app, "Adapt the application") || !strings.Contains(app, "port: 8080") {
		t.Errorf("a new application's next steps:\n%s", app)
	}

	o := testOptions(true)
	bare := NextSteps(o, Present{})
	for _, want := range []string{"Adapt the application", "Add ./Dockerfile", "no go.mod at the repository root",
		`named "runtime image"`, "USER 65532:65532", "port: <the one port your Dockerfile EXPOSEs>"} {
		if !strings.Contains(bare, want) {
			t.Errorf("--existing without a Dockerfile or go.mod lacks %q:\n%s", want, bare)
		}
	}
	full := NextSteps(o, Present{Dockerfile: true, GoMod: true})
	for _, want := range []string{"./Dockerfile is the runtime image runtime-image.yml builds", "keep both"} {
		if !strings.Contains(full, want) {
			t.Errorf("--existing with a Dockerfile and go.mod lacks %q:\n%s", want, full)
		}
	}
}
