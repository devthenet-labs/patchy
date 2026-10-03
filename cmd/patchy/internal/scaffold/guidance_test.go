// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// ghVariableSet is one printed `gh variable set` command for a gate, so it
// pastes into a shell as one command.
var ghVariableSet = regexp.MustCompile(`^       gh variable set ([A-Z_]+) --repo [A-Za-z0-9._/-]+ --body true$`)

// TestNextStepsSetTheVariablesFromTheModule: the configuration comes from
// the reference module's dotenv output in one command, on the repository
// (never the organization), and only the two gates are set by hand: the
// agent's first, the preview's last, once the Project previews the
// repository.
func TestNextStepsSetTheVariablesFromTheModule(t *testing.T) {
	for _, existing := range []bool{false, true} {
		o := testOptions(existing)
		steps := NextSteps(o, Present{})
		var set []string
		for line := range strings.SplitSeq(steps, "\n") {
			if !strings.Contains(line, "gh variable set") || strings.Contains(line, "gh variable set -f -") {
				continue
			}
			m := ghVariableSet.FindStringSubmatch(line)
			if m == nil {
				t.Errorf("not a pasteable gh command: %q", line)
				continue
			}
			set = append(set, m[1])
		}
		if want := []string{AgentPublishEnabled, PreviewPublishEnabled}; !slices.Equal(set, want) {
			t.Errorf("existing=%v: variables set by hand %v, want %v", existing, set, want)
		}
		for _, wantLine := range []string{
			"       terraform output -raw patchy_app_hello_web_variables | gh variable set -f - --repo acme/Hello.Web\n",
			// The platform module's root output, keyed by the slug: the
			// standalone module's output does not exist in that root.
			"       terraform output -json github_variables_dotenv | jq -r '.\"hello-web\"' | " +
				"gh variable set -f - --repo acme/Hello.Web\n",
			`       output "patchy_app_hello_web_variables" {`,
			"value = module.patchy_app_hello_web.github_variables_dotenv",
			"on this repository only: an organization variable reaches every",
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
		// The dotenv command comes before the agent gate: the publisher
		// fails closed on any variable it reads unset.
		if strings.Index(steps, "gh variable set -f -") > strings.Index(steps, AgentPublishEnabled+" --repo") {
			t.Errorf("existing=%v: the agent gate is set before the configuration:\n%s", existing, steps)
		}
	}
}

// TestNextStepsPinTheModuleToTheRelease: a release CLI names the reference
// module at its own tag, with every value it knows filled in and the gh
// command beside each value only GitHub knows; a development build says
// to pin the ref.
func TestNextStepsPinTheModuleToTheRelease(t *testing.T) {
	o := testOptions(false)
	o.Release = "0.12.16"
	steps := NextSteps(o, Present{})
	for _, want := range []string{
		`       module "patchy_app_hello_web" {` + "\n" + `         source = "` + ModuleSource + `?ref=v0.12.16"` + "\n",
		"git::https://github.com/devthenet-labs/patchy.git//deploy/terraform/aws/modules/app?ref=v0.12.16",
		`         slug = "hello-web"`,
		`           owner            = "acme"`,
		`           name             = "Hello.Web"`,
		`           repository_id    = "<id>" # gh api repos/acme/Hello.Web --jq .id`,
		`           owner_id         = "<id>" # gh api repos/acme/Hello.Web --jq .owner.id`,
		`           default_branch   = "main"`,
		`# gh api repos/acme/Hello.Web/actions/oidc/customization/sub --jq .sub_claim_prefix`,
		`github_oidc_provider_arn = "arn:aws:iam::123456789012:oidc-provider/token.actions.githubusercontent.com"`,
		"add an apps\n     entry keyed hello-web instead: its github_variables_dotenv output",
	} {
		if !strings.Contains(steps, want) {
			t.Errorf("next steps lack %q:\n%s", want, steps)
		}
	}
	for _, unwanted := range []string{"development build", "agent_path_prefix", "vX.Y.Z"} {
		if strings.Contains(steps, unwanted) {
			t.Errorf("a release's next steps hold %q:\n%s", unwanted, steps)
		}
	}

	o.Release = ""
	o.AgentPrefix = "acme/agents"
	dev := NextSteps(o, Present{})
	for _, want := range []string{"modules/app?ref=vX.Y.Z\"", "development build: pin ref",
		`agent_path_prefix        = "acme/agents"`} {
		if !strings.Contains(dev, want) {
			t.Errorf("a development build's next steps lack %q:\n%s", want, dev)
		}
	}
}

// TestNextStepsModuleNamesAreIdentifiers: the module and output names are
// terraform identifiers whatever the slug, which may start with a digit.
func TestNextStepsModuleNamesAreIdentifiers(t *testing.T) {
	identifier := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
	for _, slug := range []string{"hello-web", "3d-viewer", "a"} {
		o := testOptions(false)
		o.ImageName = slug
		if name := moduleName(o); !identifier.MatchString(name) || !identifier.MatchString(name+"_variables") {
			t.Errorf("slug %s: module name %q is not a terraform identifier", slug, name)
		}
	}
}

// TestNextStepsNameTheDeclaredTag: the agent image is published as the tag
// .patchy/agent.yaml declares, the first one for a new declaration and the
// kept one when a forced scaffold kept the repository's own.
func TestNextStepsNameTheDeclaredTag(t *testing.T) {
	for _, tc := range []struct {
		present Present
		tag     string
	}{
		{Present{}, ToolchainTag},
		{Present{Toolchain: "toolchain-v3"}, "toolchain-v3"},
	} {
		steps := NextSteps(testOptions(false), tc.present)
		for _, want := range []string{
			tc.tag + ", the tag .patchy/agent.yaml declares",
			"--repository-name patchy/app-envs/hello-web --image-ids imageTag=" + tc.tag + "\n",
		} {
			if !strings.Contains(steps, want) {
				t.Errorf("next steps for %+v lack %q:\n%s", tc.present, want, steps)
			}
		}
		if tc.tag != ToolchainTag && strings.Contains(steps, ToolchainTag+",") {
			t.Errorf("next steps for a kept %s still name %s:\n%s", tc.tag, ToolchainTag, steps)
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
	if strings.Contains(full, "an earlier init app's") {
		t.Errorf("--existing names an earlier scaffold's ci.yml that is not there:\n%s", full)
	}

	// Over an earlier full scaffold (a template's copy), its ci.yml keeps
	// building a runtime image nothing publishes: say so, and how to undo it.
	over := NextSteps(o, Present{Dockerfile: true, GoMod: true, ScaffoldCI: true})
	for _, want := range []string{".github/workflows/ci.yml is an earlier init app's",
		`nothing publishes
    now (publish-images.yml follows "runtime image")`, "leaves\n    out runtime-image.yml",
		"delete runtime-image.yml and scaffold\n    again without --existing",
		"removing .patchy/agent.yaml and .patchy/Dockerfile"} {
		if !strings.Contains(over, want) {
			t.Errorf("--existing over an earlier scaffold lacks %q:\n%s", want, over)
		}
	}
}
