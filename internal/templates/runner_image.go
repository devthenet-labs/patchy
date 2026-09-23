// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
)

// RunnerImageMarker heads the runner-image sticky tracking comment; the
// projection finds the comment to edit in place by it.
const RunnerImageMarker = "<!-- patchy:runner-image -->"

// AgentImageGuideURL is the repository-owner guide to agent images (the
// docs site's integrations/agent-images page), which the runner-image
// comment links to and whose troubleshooting table it points at.
const AgentImageGuideURL = "https://devthenet-labs.github.io/patchy/integrations/agent-images/"

// RunnerImageComment is the data behind the runner-image sticky comment:
// what patchy did with the agent image a repository declared, for an owner
// who has no cluster access. Exactly one of three outcomes holds — the
// declaring file was not applicable, the declaration was rejected, or the
// image was accepted — and an accepted image may add what went wrong when
// a run tried it.
type RunnerImageComment struct {
	// Manifest is the declaring file's repository-relative path.
	Manifest string
	// Declared is the reference exactly as the file names it; empty for a
	// not-applicable devcontainer.json and for a .patchy/agent.yaml that
	// could not be read.
	Declared string
	// Image is the digest-pinned reference the declaration resolved to;
	// empty unless accepted.
	Image string
	// Verified reports that the image's signature was checked.
	Verified bool
	// NotApplicable marks a .devcontainer/devcontainer.json patchy cannot
	// honour, which is no declaration at all: the default image ran.
	NotApplicable bool
	// Rejected is the rejection's reason label (NotAllowlisted, Unsigned,
	// ...); empty unless the declaration was rejected.
	Rejected string
	// Reason is the exact explanation: the rejection's message, or why the
	// devcontainer.json was not applicable.
	Reason string
	// Parked reports that the rejection handed the finding to a human
	// (onReject handoff) instead of falling back to the default image. The
	// gate parks it before any investigation exists, and an approval revives
	// only a finding with an investigation to remediate from, so a parked
	// finding stays with a human.
	Parked bool
	// Used reports that a run launched in the accepted image; a run the
	// sandbox probe refused never did.
	Used bool
	// Unused reports that runs launched and none used the accepted image:
	// each ran the default runner image instead. Neither Used nor Unused
	// means no run has launched yet, so the image is only going to be used.
	Unused bool
	// Incompatible is the latest run whose preflight found the image
	// incompatible (image_incompatible); nil when none did.
	Incompatible *RunnerImageRun
	// SandboxRefused is the latest run the sandbox probe refused because
	// the cluster does not enforce NetworkPolicy; nil when none was.
	SandboxRefused *RunnerImageRun
	// GuideURL is the repository-owner guide; empty means
	// AgentImageGuideURL.
	GuideURL string
}

// RunnerImageRun is one agent run a runner-image comment reports on.
type RunnerImageRun struct {
	// Stage is "investigation" or "remediation".
	Stage string
	// Attempt is the run's attempt number.
	Attempt int32
	// Detail is what the run reported, shown verbatim.
	Detail string
}

// CheckRef is the reference the comment tells the owner to run
// `patchy check image` on: the declared one, or a placeholder when there is
// none to quote.
func (c RunnerImageComment) CheckRef() string {
	if c.Declared != "" {
		return c.Declared
	}
	return "<ref>"
}

// RenderRunnerImageComment renders the runner-image sticky comment, headed
// by RunnerImageMarker.
func RenderRunnerImageComment(c RunnerImageComment) (string, error) {
	if c.GuideURL == "" {
		c.GuideURL = AgentImageGuideURL
	}
	return render("runner_image_comment.md.tmpl", c)
}

// code renders s as one markdown code span that s cannot close early: the
// delimiter is one backtick longer than the longest run of backticks in s,
// and line breaks become spaces, since a code span cannot survive a blank
// line. s is padded with a space on each side where it starts or ends with
// a backtick (which would merge into the delimiter) or both starts and ends
// with a space without being all spaces (CommonMark strips one from each
// side then), so the span always decodes back to s.
func code(s string) string {
	s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(s)
	if s == "" {
		return ""
	}
	delim := strings.Repeat("`", longestBacktickRun(s)+1)
	backtickEdge := strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`")
	spaceEdges := strings.HasPrefix(s, " ") && strings.HasSuffix(s, " ") && strings.Trim(s, " ") != ""
	if backtickEdge || spaceEdges {
		s = " " + s + " "
	}
	return delim + s + delim
}

// fence renders s as a fenced text block that s cannot close early: the
// fence is at least three backticks and longer than any run of backticks
// in s, so no line of s is a closing fence. The block ends with a newline.
func fence(s string) string {
	f := strings.Repeat("`", max(3, longestBacktickRun(s)+1))
	return f + "text\n" + strings.TrimRight(s, "\r\n") + "\n" + f + "\n"
}

// longestBacktickRun is the length of the longest run of backticks in s.
func longestBacktickRun(s string) int {
	longest, run := 0, 0
	for i := range len(s) {
		if s[i] != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	return longest
}
