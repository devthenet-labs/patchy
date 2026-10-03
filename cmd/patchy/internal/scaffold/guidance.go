// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"fmt"
	"strings"
)

// Present is what the repository already has that the guidance depends
// on.
type Present struct {
	// Dockerfile is a runtime Dockerfile at the repository root.
	Dockerfile bool
	// GoMod is a go.mod at the repository root.
	GoMod bool
	// Toolchain is the toolchain-v<N> tag the repository's own
	// .patchy/agent.yaml declares, kept by a forced scaffold
	// (KeepToolchain); empty when the scaffold writes ToolchainTag.
	Toolchain string
	// ScaffoldCI is an earlier full scaffold's .github/workflows/ci.yml,
	// which tests the application and builds its runtime image as `test`.
	// It matters only to Options.Existing, whose runtime build is a
	// workflow of its own that the dispatcher follows instead.
	ScaffoldCI bool
}

// ScaffoldCIMarker is what marks a .github/workflows/ci.yml as a full
// scaffold's: only its runtime build validates the image archive with the
// publishers' validate_oci.py.
const ScaffoldCIMarker = ".github/actions/publish/validate_oci.py"

// NextSteps is what the repository owner does after the files are written:
// the registry repositories and roles to create (with patchy's reference
// terraform module, pinned to the CLI's release), the repository variables
// to set from that module's output, the order publishing is switched on
// in, and the Project's preview fields. For Options.Existing it ends with
// what the application itself must be adapted to, given what is present.
func NextSteps(o Options, present Present) string {
	var b strings.Builder
	repo := o.Repo.String()
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	gate := func(name string) { w("       gh variable set %s --repo %s --body true", name, repo) }
	tag := ToolchainTag
	if present.Toolchain != "" {
		tag = present.Toolchain
	}

	w("Next steps for %s (image name %s):", repo, o.ImageName)
	w("")
	w("  1. Create its two ECR repositories, with immutable tags, and one publisher role for")
	w("     each. patchy's reference terraform module does that; in your terraform root:")
	writeModule(w, o)
	w("     It creates")
	w("       agent    %s/%s", o.Registry, o.AgentRepository())
	w("       runtime  %s/%s", o.Registry, o.RuntimeRepository())
	w("     and roles that each trust only this repository's %s branch and their own", o.Branch)
	w("     publisher workflow, .github/workflows/publish-agent.yml or publish-runtime.yml.")
	w("     With the platform module (deploy/terraform/aws, at the same ref), add an apps")
	w("     entry keyed %s instead: its github_variables_dotenv output holds the same", o.ImageName)
	w("     variables under that key.")
	w("  2. Set the publishers' repository variables (none is secret) from the module's")
	w("     output, on this repository only: an organization variable reaches every")
	w("     repository with these workflows, a template repository and its copies included.")
	w("       terraform output -raw %s | gh variable set -f - --repo %s", moduleName(o)+"_variables", repo)
	w("     Without the module, .github/actions/publish/README.md lists the variables. Then")
	w("     switch on the agent image's publisher:")
	gate(AgentPublishEnabled)
	w("  3. Commit and push to %s. The push builds the agent image and publishes it as", o.Branch)
	w("     %s, the tag .patchy/agent.yaml declares. If it was pushed before step 2, run", tag)
	w(`       gh workflow run "agent image" --repo %s`, repo)
	w("     and confirm the tag exists before creating the Project:")
	w("       aws ecr describe-images --region %s --repository-name %s --image-ids imageTag=%s",
		o.Region(), o.AgentRepository(), tag)
	w("  4. When the Project previews this repository, publish its runtime images:")
	gate(PreviewPublishEnabled)
	w("     The Project's preview fields for it:")
	w("       imageRepository: %s/%s", o.Registry, o.RuntimeRepository())
	if o.Existing {
		w("       port: <the one port your Dockerfile EXPOSEs>")
		w("       readinessPath: <a path that answers 200 once the service is ready>")
	} else {
		w("       port: %d", Port)
		w("       readinessPath: %s", ReadinessPath)
	}
	w("")
	w("To change the agent toolchain later, edit .patchy/Dockerfile and bump the tag in")
	w(".patchy/agent.yaml (toolchain-v2, ...) in the same commit: tags are immutable.")
	w("If the registry moves, change ECR_REGISTRY, AGENT_IMAGE_REPOSITORY and the image in")
	w(".patchy/agent.yaml together. .github/actions/publish/README.md describes the publishers.")
	if o.Existing {
		b.WriteString(adapt(o, present))
	}
	return b.String()
}

// moduleName is the terraform name the next steps give the app's module
// block, and with a _variables suffix its dotenv output: an identifier
// whatever the slug starts with.
func moduleName(o Options) string {
	return "patchy_app_" + strings.ReplaceAll(o.ImageName, "-", "_")
}

// writeModule prints the reference app module's block for o, pinned to the
// CLI's release, and the root output that exports its repository
// variables. What only GitHub knows (the numeric IDs and the OIDC subject
// prefix: init app makes no GitHub call) is a placeholder beside the gh
// command that reads it.
func writeModule(w func(string, ...any), o Options) {
	ref := "vX.Y.Z"
	if o.Release != "" {
		ref = "v" + o.Release
	}
	api := "gh api repos/" + o.Repo.String()
	name := moduleName(o)
	w(`       module "%s" {`, name)
	w(`         source = "%s?ref=%s"`, ModuleSource, ref)
	w(``)
	w(`         slug = "%s"`, o.ImageName)
	w(`         github = {`)
	w(`           owner            = "%s"`, o.Repo.Owner)
	w(`           name             = "%s"`, o.Repo.Name)
	w(`           repository_id    = "<id>" # %s --jq .id`, api)
	w(`           owner_id         = "<id>" # %s --jq .owner.id`, api)
	w(`           default_branch   = "%s"`, o.Branch)
	w(`           sub_claim_prefix = "<prefix>" # %s/actions/oidc/customization/sub --jq .sub_claim_prefix`, api)
	w(`         }`)
	w(`         # The account's GitHub Actions OIDC provider, created once per account.`)
	w(`         github_oidc_provider_arn = "arn:aws:iam::%s:oidc-provider/token.actions.githubusercontent.com"`,
		o.Account())
	if o.AgentPrefix != DefaultAgentPrefix {
		w(`         agent_path_prefix        = "%s"`, o.AgentPrefix)
	}
	w(`       }`)
	w(`       output "%s_variables" {`, name)
	w(`         value = module.%s.github_variables_dotenv`, name)
	w(`       }`)
	if o.Release == "" {
		w("     This CLI is a development build: pin ref to the patchy release you deploy.")
	}
}

// adapt is what Options.Existing leaves to the owner: the runtime image and
// the agent image's build context are theirs.
func adapt(o Options, present Present) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	wf := o.runtimeWorkflow()
	w("")
	w("Adapt the application (--existing wrote only .patchy/ and the CI publishers):")
	if present.ScaffoldCI {
		w("  - .github/workflows/ci.yml is an earlier init app's, without --existing (a")
		w("    template's copy, say). It still builds a runtime image that nothing publishes")
		w("    now (publish-images.yml follows %q), and its actionlint list leaves", wf.name)
		w("    out %s. For a template's copy, delete %s and scaffold", wf.file, wf.file)
		w("    again without --existing, removing .patchy/agent.yaml and .patchy/Dockerfile")
		w("    first if the image name changes (--force then rewrites the application's files")
		w("    too). Otherwise drop ci.yml's runtime build and lint %s there too.", wf.file)
	}
	if present.Dockerfile {
		w("  - ./Dockerfile is the runtime image %s builds, at the repository root.", wf.file)
	} else {
		w("  - Add ./Dockerfile, the runtime image %s builds at the repository root.", wf.file)
	}
	w("    A preview runs it as uid 65532 with a read-only root filesystem, no writable /tmp")
	w("    and every capability dropped: declare USER 65532:65532, write nothing to disk,")
	w("    EXPOSE one port and answer the readiness path with 200 once ready. It must build")
	w("    for linux/amd64 and stay under 128 MiB as an OCI archive.")
	if present.GoMod {
		w("  - .patchy/Dockerfile copies go.mod and go.sum from the build context: keep both")
		w("    out of .dockerignore's exclusions. It pins Go %s (GOTOOLCHAIN=local); if go.mod", GoVersion)
		w("    asks for a newer Go, change its toolchain stage.")
	} else {
		w("  - There is no go.mod at the repository root, and .patchy/Dockerfile builds the")
		w("    agent's Go module cache from one: adapt it to where the module lives.")
	}
	w("  - publish-images.yml starts on the workflows named %q and \"agent image\": rename", wf.name)
	w("    any workflow of yours that already has one of those names.")
	w("  - Your own CI keeps testing the application; %s only tests the publishers", wf.file)
	w("    and builds the runtime image. The publishers' tests write __pycache__ under")
	w("    .github/actions/publish when run locally: add __pycache__/ to .gitignore.")
	return b.String()
}
