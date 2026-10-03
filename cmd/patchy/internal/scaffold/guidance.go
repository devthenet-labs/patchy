// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"fmt"
	"strings"
)

// Present is what an existing application already has that the guidance
// for Options.Existing depends on.
type Present struct {
	// Dockerfile is a runtime Dockerfile at the repository root.
	Dockerfile bool
	// GoMod is a go.mod at the repository root.
	GoMod bool
}

// NextSteps is what the repository owner does after the files are written:
// the registry repositories and roles to create, the repository variables
// to set (with the gh commands that set them), the order publishing is
// switched on in, and the Project's preview fields. For Options.Existing
// it ends with what the application itself must be adapted to, given what
// is present.
func NextSteps(o Options, present Present) string {
	var b strings.Builder
	repo := o.Repo.String()
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	set := func(v Variable) {
		value := v.Value
		switch {
		case v.Shell != "":
			value = `"$(` + v.Shell + `)"`
		case value == "":
			value = "'<" + v.Placeholder + ">'"
		}
		w("       gh variable set %s --repo %s --body %s", v.Name, repo, value)
	}
	gates := map[string]Variable{}

	w("Next steps for %s (image name %s):", repo, o.ImageName)
	w("")
	w("  1. Create its two ECR repositories, with immutable tags, and one publisher role for")
	w("     each (the patchy terraform module's app module, or your own):")
	w("       agent    %s/%s", o.Registry, o.AgentRepository())
	w("       runtime  %s/%s", o.Registry, o.RuntimeRepository())
	w("     Each role trusts only this repository's %s branch and its own publisher", o.Branch)
	w("     workflow, .github/workflows/publish-agent.yml or publish-runtime.yml.")
	w("  2. Set the publishers' repository variables (none is secret):")
	for _, v := range Variables(o) {
		if v.Name == AgentPublishEnabled || v.Name == PreviewPublishEnabled {
			gates[v.Name] = v
			continue
		}
		set(v)
	}
	set(gates[AgentPublishEnabled])
	w("  3. Commit and push to %s. The push builds the agent image and publishes it as", o.Branch)
	w("     %s, the tag .patchy/agent.yaml declares. If it was pushed before step 2, run", ToolchainTag)
	w(`       gh workflow run "agent image" --repo %s`, repo)
	w("     and confirm the tag exists before creating the Project:")
	w("       aws ecr describe-images --region %s --repository-name %s --image-ids imageTag=%s",
		o.Region(), o.AgentRepository(), ToolchainTag)
	w("  4. When the Project previews this repository, publish its runtime images:")
	set(gates[PreviewPublishEnabled])
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

// adapt is what Options.Existing leaves to the owner: the runtime image and
// the agent image's build context are theirs.
func adapt(o Options, present Present) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	wf := o.runtimeWorkflow()
	w("")
	w("Adapt the application (--existing wrote only .patchy/ and the CI publishers):")
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
