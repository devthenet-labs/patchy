// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The agents-namespace Role deliberately copies the agent-jobs grant. Keep
// every operator-facing description explicit about its Secret scope.
func TestAgentSecretGrantIsDocumented(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for _, path := range []string{
		"AGENTS.md",
		"DESIGN.md",
		"deploy/README.md",
		"docs/deployment/kustomize.md",
		"docs/configuration/intent-controller.md",
		"docs/design/intent-driven-development.md",
		"charts/patchy/templates/NOTES.txt",
		"charts/patchy/templates/intent-controller.yaml",
		"deploy/kustomize/components/intent-controller/rbac.yaml",
	} {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, path))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "any Secret in the agents namespace") {
				t.Error("the agents-namespace unrestricted Secret grant is not stated")
			}
		})
	}
}
