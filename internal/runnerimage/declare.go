// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import "fmt"

// Outcome is what the precedence rule concluded about the pinned tree.
type Outcome int

const (
	// OutcomeNone means neither declaration file exists: the default image
	// runs and nothing is recorded.
	OutcomeNone Outcome = iota
	// OutcomeDeclared means Declaration.Image, declared in
	// Declaration.Manifest, is to be resolved.
	OutcomeDeclared
	// OutcomeNotApplicable means a .devcontainer/devcontainer.json exists but
	// was not written for patchy (it builds its image, adds features, names no
	// usable image, or cannot be parsed): the default image runs and
	// Declaration.Reason is informational, surfaced to the repository owner
	// and never subject to onReject.
	OutcomeNotApplicable
)

// String names the outcome for logs and tests.
func (o Outcome) String() string {
	switch o {
	case OutcomeNone:
		return "none"
	case OutcomeDeclared:
		return "declared"
	case OutcomeNotApplicable:
		return "not-applicable"
	}
	return fmt.Sprintf("outcome(%d)", int(o))
}

// Declaration is the result of applying precedence to the two files.
type Declaration struct {
	Outcome Outcome
	// Manifest is the repository-relative path of the file that decided the
	// outcome (AgentYAMLPath or DevcontainerPath), empty for OutcomeNone.
	Manifest string
	// Image is the declared reference, set only for OutcomeDeclared.
	Image string
	// Reason says why a devcontainer.json was not applicable, set only for
	// OutcomeNotApplicable.
	Reason string
}

// Declare applies precedence to the declaration files of one pinned tree.
//
// .patchy/agent.yaml wins whenever it exists: a valid one is declared and
// devcontainer.json is not consulted, an invalid one (or one over
// MaxDeclarationBytes) is returned as a *Rejection error and never falls
// through, because a broken explicit declaration must not silently become a
// different image. Only when .patchy/agent.yaml is absent is
// devcontainer.json judged, and a file patchy cannot honour is
// OutcomeNotApplicable rather than an error: a rejection is something the
// repository asked patchy for and got wrong; a not-applicable devcontainer
// was never addressed to patchy at all. The two therefore never share a
// representation, and a caller that checks err and then switches on Outcome
// cannot park a finding on a build-based devcontainer.
func Declare(files Files) (Declaration, error) {
	if files.AgentYAML.Present {
		if files.AgentYAML.Data == nil {
			return Declaration{}, reject("`%s` is %d bytes; the limit is %d bytes",
				AgentYAMLPath, files.AgentYAML.Size, MaxDeclarationBytes)
		}
		image, err := parseAgentYAML(files.AgentYAML.Data)
		if err != nil {
			return Declaration{}, err
		}
		return Declaration{Outcome: OutcomeDeclared, Manifest: AgentYAMLPath, Image: image}, nil
	}
	if files.Devcontainer.Present {
		if files.Devcontainer.Data == nil {
			return Declaration{
				Outcome:  OutcomeNotApplicable,
				Manifest: DevcontainerPath,
				Reason: fmt.Sprintf("`%s` is %d bytes; the limit is %d bytes",
					DevcontainerPath, files.Devcontainer.Size, MaxDeclarationBytes),
			}, nil
		}
		image, reason := judgeDevcontainer(files.Devcontainer.Data)
		if image == "" {
			return Declaration{Outcome: OutcomeNotApplicable, Manifest: DevcontainerPath, Reason: reason}, nil
		}
		return Declaration{Outcome: OutcomeDeclared, Manifest: DevcontainerPath, Image: image}, nil
	}
	return Declaration{Outcome: OutcomeNone}, nil
}
