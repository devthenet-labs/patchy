// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// buildKeys are the devcontainer.json keys that mean the image is built
// rather than pulled, in the order they are reported.
var buildKeys = []string{"build", "dockerFile", "dockerComposeFile"}

// judgeDevcontainer reads the top-level string image out of a
// .devcontainer/devcontainer.json. A file patchy cannot honour (unparseable,
// build-based, adding features, or naming no usable image) yields an empty
// image and the reason; the caller records that as not applicable, never as
// a rejection, because the file was written for editor tooling and every
// other key is ignored rather than judged.
func judgeDevcontainer(data []byte) (image, reason string) {
	stripped, err := stripJSONC(data)
	if err != nil {
		return "", fmt.Sprintf("could not parse `%s`: %v", DevcontainerPath, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(stripped, &doc); err != nil {
		return "", fmt.Sprintf("could not parse `%s`: %s", DevcontainerPath, jsonError(err))
	}
	for _, key := range buildKeys {
		if _, ok := doc[key]; ok {
			return "", fmt.Sprintf("`%s` builds its image (`%s`); patchy does not build images. "+
				"Publish the image and set `image`, or declare one in `%s`, which takes precedence.",
				DevcontainerPath, key, AgentYAMLPath)
		}
	}
	if raw, ok := doc["features"]; ok && !emptyJSON(raw) {
		return "", fmt.Sprintf("`%s` adds features, which patchy would have to build; patchy does not build "+
			"images. Publish the image with the features applied and set `image`, or declare one in `%s`, "+
			"which takes precedence.", DevcontainerPath, AgentYAMLPath)
	}
	raw, ok := doc["image"]
	if !ok {
		return "", fmt.Sprintf("`%s` has no `image`", DevcontainerPath)
	}
	if err := json.Unmarshal(raw, &image); err != nil || string(bytes.TrimSpace(raw)) == "null" {
		return "", fmt.Sprintf("`%s` `image` is not a string", DevcontainerPath)
	}
	if strings.TrimSpace(image) == "" {
		return "", fmt.Sprintf("`%s` `image` is empty", DevcontainerPath)
	}
	if strings.Contains(image, "${") {
		return "", fmt.Sprintf("`%s` `image` uses variable substitution (`${`), which patchy does not perform",
			DevcontainerPath)
	}
	return image, ""
}

// emptyJSON reports whether a raw value carries nothing: null, {} or [].
func emptyJSON(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	switch v := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(v) == 0
	case []any:
		return len(v) == 0
	}
	return false
}

// jsonError renders an encoding/json error with its byte offset, which the
// pre-processor preserved, so the message points into the original file.
func jsonError(err error) string {
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return fmt.Sprintf("%s at offset %d", syn.Error(), syn.Offset)
	}
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) {
		return fmt.Sprintf("%s at offset %d", ute.Error(), ute.Offset)
	}
	return err.Error()
}
