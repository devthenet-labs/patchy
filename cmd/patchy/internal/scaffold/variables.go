// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"fmt"
	"strings"
)

// The publishers' gates: repository variables an owner flips to "true".
const (
	// AgentPublishEnabled gates the agent image's publisher. It is
	// independent of previews: a repository without previews still needs
	// the image its agent runs in.
	AgentPublishEnabled = "AGENT_PUBLISH_ENABLED"
	// PreviewPublishEnabled gates the runtime (preview) image's publisher,
	// set last, once the Project previews the repository.
	PreviewPublishEnabled = "PREVIEW_PUBLISH_ENABLED"
)

// Variable is one repository variable the generated publishers read. None
// is secret: AWS role trust, not the variables, is the boundary.
type Variable struct {
	// Name is the variable's name, as the workflows read it.
	Name string
	// Value is its value when the scaffold knows it.
	Value string
	// Shell is a shell expression for the value when GitHub knows it and
	// the scaffold does not (the repository's numeric IDs).
	Shell string
	// Placeholder describes the value when neither does (a role ARN, which
	// the infrastructure that creates the role reports). It is shown in
	// angle brackets inside single quotes, so it never holds a quote.
	Placeholder string
	// About says what the variable is.
	About string
}

// Variables are the repository variables the generated publishers read,
// configuration first and the two gates last, in the order an owner sets
// them. The README tables and the next steps are both built from this
// one list.
func Variables(o Options) []Variable {
	repo := o.Repo.String()
	return []Variable{
		{Name: "PUBLISH_REPOSITORY_ID", Shell: fmt.Sprintf("gh api repos/%s --jq .id", repo),
			About: "this repository's immutable numeric ID, which the guard checks"},
		{Name: "PUBLISH_OWNER_ID", Shell: fmt.Sprintf("gh api repos/%s --jq .owner.id", repo),
			About: "its owner's immutable numeric ID"},
		{Name: "AWS_REGION", Value: o.Region(), About: "the registry's AWS region"},
		{Name: "ECR_REGISTRY", Value: o.Registry, About: "the ECR registry host"},
		{Name: "AGENT_IMAGE_REPOSITORY", Value: o.AgentRepository(),
			About: "the agent image's ECR repository, which `.patchy/agent.yaml` names"},
		{Name: "AGENT_ROLE_ARN", Placeholder: "agent publisher role ARN",
			About: "assumed only by `publish-agent.yml`"},
		{Name: "RUNTIME_IMAGE_REPOSITORY", Value: o.RuntimeRepository(),
			About: "the runtime (preview) image's ECR repository"},
		{Name: "RUNTIME_ROLE_ARN", Placeholder: "runtime publisher role ARN",
			About: "assumed only by `publish-runtime.yml`"},
		{Name: AgentPublishEnabled, Value: "true",
			About: "publishes the agent image; anything else skips it"},
		{Name: PreviewPublishEnabled, Value: "true",
			About: "publishes runtime images; set it last, once previews are configured"},
	}
}

// variablesTable is the Markdown table of Variables the READMEs carry.
func variablesTable(o Options) string {
	rows := [][]string{{"Variable", "Value", "What it is"}}
	for _, v := range Variables(o) {
		value := "the " + v.Placeholder
		switch {
		case v.Value != "":
			value = "`" + v.Value + "`"
		case v.Shell != "":
			value = "`" + v.Shell + "`"
		}
		rows = append(rows, []string{"`" + v.Name + "`", value, v.About})
	}
	return markdownTable(rows)
}

// publishersTable is the Markdown table of the trusted publisher workflows,
// the only workflows that assume an AWS role, and the role each one
// assumes. The role's trust names the workflow path at the default branch
// (job_workflow_ref), so the path is part of the contract.
func publishersTable(o Options) string {
	ref := "@refs/heads/" + o.Branch
	return markdownTable([][]string{
		{"Trusted workflow (`job_workflow_ref`)", "Assumes", "Publishes"},
		{"`" + o.Repo.String() + "/.github/workflows/publish-runtime.yml" + ref + "`", "`RUNTIME_ROLE_ARN`",
			"the runtime image as `sha-<commit>`, and `main-<commit>` for a default-branch commit"},
		{"`" + o.Repo.String() + "/.github/workflows/publish-agent.yml" + ref + "`", "`AGENT_ROLE_ARN`",
			"the agent image as the `toolchain-v<N>` tag `.patchy/agent.yaml` declares"},
	})
}

// markdownTable lays rows out as a Markdown table whose first row is the
// header, padded so the source lines up.
func markdownTable(rows [][]string) string {
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], len([]rune(cell)))
		}
	}
	var b strings.Builder
	line := func(cells []string) {
		b.WriteString("|")
		for i, cell := range cells {
			fmt.Fprintf(&b, " %s%s |", cell, strings.Repeat(" ", widths[i]-len([]rune(cell))))
		}
		b.WriteString("\n")
	}
	line(rows[0])
	rule := make([]string, len(widths))
	for i, w := range widths {
		rule[i] = strings.Repeat("-", w)
	}
	line(rule)
	for _, row := range rows[1:] {
		line(row)
	}
	return strings.TrimSuffix(b.String(), "\n")
}
