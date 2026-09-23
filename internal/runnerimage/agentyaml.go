// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// parseAgentYAML reads the declared image out of a .patchy/agent.yaml. The
// schema is one key, image, and it fails closed: a build key has its own
// message because it is the obvious thing to try, and any other key is
// refused so a newer schema is never half-honoured. The file is exactly one
// YAML document.
func parseAgentYAML(data []byte) (string, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return "", reject("`%s` is empty; it must set `image`", AgentYAMLPath)
		}
		return "", reject("could not parse `%s`: %s", AgentYAMLPath, oneLine(err.Error()))
	}
	if doc == nil {
		return "", reject("`%s` is empty; it must set `image`", AgentYAMLPath)
	}
	// A second document is refused rather than ignored: only the first would
	// be honoured, so a `build:` (or a second `image:`) there would be
	// silently dropped. Any further decode outcome but EOF, a parse error in
	// the second document included, means the file is not one mapping.
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", reject("`%s` has more than one YAML document; it must be a single mapping", AgentYAMLPath)
	}
	if _, ok := doc["build"]; ok {
		return "", reject("`%s` has a `build` key; patchy does not build images. Publish the image and set `image`",
			AgentYAMLPath)
	}
	var unknown []string
	for k := range doc {
		if k != "image" {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return "", reject("`%s` has unknown key `%s`; `image` is the only key", AgentYAMLPath,
			strings.Join(unknown, "`, `"))
	}
	raw, ok := doc["image"]
	if !ok {
		return "", reject("`%s` must set `image`", AgentYAMLPath)
	}
	if raw == nil {
		return "", reject("`%s` `image` is empty", AgentYAMLPath)
	}
	image, ok := raw.(string)
	if !ok {
		return "", reject("`%s` `image` is not a string", AgentYAMLPath)
	}
	if strings.TrimSpace(image) == "" {
		return "", reject("`%s` `image` is empty", AgentYAMLPath)
	}
	return image, nil
}

// oneLine collapses a multi-line parser error onto one line so it reads
// cleanly in a condition message.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
