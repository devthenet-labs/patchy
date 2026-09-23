// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import "testing"

func TestParseAgentYAML(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantImage string
		wantErr   string
	}{
		{"plain", "image: ghcr.io/org/app:1.26\n", "ghcr.io/org/app:1.26", ""},
		{"quoted with comments", "# the agent's toolchain\nimage: \"ghcr.io/org/app@sha256:abc\" # pinned\n",
			"ghcr.io/org/app@sha256:abc", ""},
		{"empty file", "", "", "`.patchy/agent.yaml` is empty; it must set `image`"},
		{"only a comment", "# nothing here\n", "", "`.patchy/agent.yaml` is empty; it must set `image`"},
		{"empty document", "---\n", "", "`.patchy/agent.yaml` is empty; it must set `image`"},
		{"empty mapping", "{}\n", "", "`.patchy/agent.yaml` must set `image`"},
		{"build key", "image: x\nbuild:\n  dockerfile: Dockerfile\n", "",
			"`.patchy/agent.yaml` has a `build` key; patchy does not build images. Publish the image and set `image`"},
		{"unknown keys, sorted", "image: x\nfoo: 1\nbar: 2\n", "",
			"`.patchy/agent.yaml` has unknown key `bar`, `foo`; `image` is the only key"},
		{"unknown key without image", "name: x\n", "",
			"`.patchy/agent.yaml` has unknown key `name`; `image` is the only key"},
		{"null image", "image:\n", "", "`.patchy/agent.yaml` `image` is empty"},
		{"empty image", "image: ''\n", "", "`.patchy/agent.yaml` `image` is empty"},
		{"blank image", "image: '   '\n", "", "`.patchy/agent.yaml` `image` is empty"},
		{"sequence image", "image: [a]\n", "", "`.patchy/agent.yaml` `image` is not a string"},
		{"integer image", "image: 42\n", "", "`.patchy/agent.yaml` `image` is not a string"},
		{"mapping image", "image:\n  name: x\n", "", "`.patchy/agent.yaml` `image` is not a string"},
		{"sequence document", "- a\n", "",
			"could not parse `.patchy/agent.yaml`: yaml: unmarshal errors: " +
				"line 1: cannot unmarshal !!seq into map[string]interface {}"},
		{"scalar document", "just a string\n", "",
			"could not parse `.patchy/agent.yaml`: yaml: unmarshal errors: " +
				"line 1: cannot unmarshal !!str `just a ...` into map[string]interface {}"},
		{"malformed", "image: [\n", "",
			"could not parse `.patchy/agent.yaml`: yaml: line 1: did not find expected node content"},
		{"second document carrying build", "image: ghcr.io/org/app:1\n---\nbuild:\n  dockerfile: Dockerfile\n", "",
			"`.patchy/agent.yaml` has more than one YAML document; it must be a single mapping"},
		{"second document repeating image", "image: a\n---\nimage: b\n", "",
			"`.patchy/agent.yaml` has more than one YAML document; it must be a single mapping"},
		{"empty second document", "image: a\n---\n", "",
			"`.patchy/agent.yaml` has more than one YAML document; it must be a single mapping"},
		{"leading document marker is one document", "---\nimage: ghcr.io/org/app:1\n", "ghcr.io/org/app:1", ""},
		{"duplicate key", "image: x\nimage: y\n", "",
			"could not parse `.patchy/agent.yaml`: yaml: unmarshal errors: " +
				"line 2: mapping key \"image\" already defined at line 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseAgentYAML([]byte(c.in))
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("parseAgentYAML(%q) error: %v", c.in, err)
				}
				if got != c.wantImage {
					t.Errorf("parseAgentYAML(%q) = %q, want %q", c.in, got, c.wantImage)
				}
				return
			}
			if err == nil {
				t.Fatalf("parseAgentYAML(%q) = %q, want error %q", c.in, got, c.wantErr)
			}
			if !IsRejection(err) {
				t.Errorf("error is not a Rejection: %T", err)
			}
			if err.Error() != c.wantErr {
				t.Errorf("parseAgentYAML(%q) error =\n  %q\nwant\n  %q", c.in, err.Error(), c.wantErr)
			}
		})
	}
}
