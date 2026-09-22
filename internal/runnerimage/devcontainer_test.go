// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import "testing"

const buildReason = "`.devcontainer/devcontainer.json` builds its image (`%s`); patchy does not build images. " +
	"Publish the image and set `image`, or declare one in `.patchy/agent.yaml`, which takes precedence."

const featuresReason = "`.devcontainer/devcontainer.json` adds features, which patchy would have to build; " +
	"patchy does not build images. Publish the image with the features applied and set `image`, or declare one " +
	"in `.patchy/agent.yaml`, which takes precedence."

func TestJudgeDevcontainer(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantImage  string
		wantReason string
	}{
		{"plain", `{"image": "ghcr.io/org/dev:1"}`, "ghcr.io/org/dev:1", ""},
		{"jsonc with ignored keys", "{\n  // the agent runs here\n  \"image\": \"ghcr.io/org/dev:1\", /* pinned */\n" +
			"  \"customizations\": {\"vscode\": {\"extensions\": [\"golang.go\",],},},\n  \"remoteUser\": \"vscode\",\n}",
			"ghcr.io/org/dev:1", ""},
		{"build wins over image", `{"build": {"dockerfile": "Dockerfile"}, "image": "x"}`, "",
			buildMsg("build")},
		{"dockerFile", `{"dockerFile": "Dockerfile"}`, "", buildMsg("dockerFile")},
		{"dockerComposeFile", `{"dockerComposeFile": "compose.yml", "service": "app"}`, "",
			buildMsg("dockerComposeFile")},
		{"build reported before features", `{"features": {"a": {}}, "build": {}}`, "", buildMsg("build")},
		{"features", `{"image": "x", "features": {"ghcr.io/devcontainers/features/go:1": {}}}`, "", featuresReason},
		{"features as a non-empty array", `{"image": "x", "features": ["a"]}`, "", featuresReason},
		{"features as a string", `{"image": "x", "features": "a"}`, "", featuresReason},
		{"empty features are fine", `{"image": "x", "features": {}}`, "x", ""},
		{"null features are fine", `{"image": "x", "features": null}`, "x", ""},
		{"empty array features are fine", `{"image": "x", "features": []}`, "x", ""},
		{"no image", `{"name": "dev"}`, "", "`.devcontainer/devcontainer.json` has no `image`"},
		{"null document", `null`, "", "`.devcontainer/devcontainer.json` has no `image`"},
		{"image not a string", `{"image": 42}`, "", "`.devcontainer/devcontainer.json` `image` is not a string"},
		{"image null", `{"image": null}`, "", "`.devcontainer/devcontainer.json` `image` is not a string"},
		{"image empty", `{"image": ""}`, "", "`.devcontainer/devcontainer.json` `image` is empty"},
		{"image blank", `{"image": "  "}`, "", "`.devcontainer/devcontainer.json` `image` is empty"},
		{"image with substitution", `{"image": "${localEnv:IMAGE}"}`, "",
			"`.devcontainer/devcontainer.json` `image` uses variable substitution (`${`), which patchy does not perform"},
		{"not an object", `[1]`, "",
			"could not parse `.devcontainer/devcontainer.json`: json: cannot unmarshal array into Go value of type " +
				"map[string]json.RawMessage at offset 1"},
		{"unterminated block comment", `{"image": "x" /* never`, "",
			"could not parse `.devcontainer/devcontainer.json`: unterminated block comment at offset 14"},
		{"unterminated string", `{"image": "x`, "",
			"could not parse `.devcontainer/devcontainer.json`: unterminated string at offset 10"},
		{"truncated", `{"image": "x"`, "",
			"could not parse `.devcontainer/devcontainer.json`: unexpected end of JSON input at offset 13"},
		{"syntax error after a comment keeps the original offset", "{ /* c */ \"image\": \"x\" ]", "",
			"could not parse `.devcontainer/devcontainer.json`: invalid character ']' after object key:value pair " +
				"at offset 24"},
		{"empty file", ``, "",
			"could not parse `.devcontainer/devcontainer.json`: unexpected end of JSON input at offset 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			image, reason := judgeDevcontainer([]byte(c.in))
			if image != c.wantImage {
				t.Errorf("image = %q, want %q", image, c.wantImage)
			}
			if reason != c.wantReason {
				t.Errorf("reason =\n  %q\nwant\n  %q", reason, c.wantReason)
			}
		})
	}
}
