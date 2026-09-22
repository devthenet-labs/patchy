// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"bytes"
	"fmt"
	"testing"
)

// buildMsg is the build-based devcontainer reason for one key.
func buildMsg(key string) string { return fmt.Sprintf(buildReason, key) }

func present(data string) File {
	return File{Present: true, Size: int64(len(data)), Data: []byte(data)}
}

func TestDeclare(t *testing.T) {
	buildDC := `{"build": {"dockerfile": "Dockerfile"}}`
	cases := []struct {
		name    string
		files   Files
		want    Declaration
		wantErr string
	}{
		{"neither file", Files{}, Declaration{Outcome: OutcomeNone}, ""},
		{"valid yaml beside a build-based devcontainer yields the yaml image",
			Files{AgentYAML: present("image: ghcr.io/org/app:1\n"), Devcontainer: present(buildDC)},
			Declaration{Outcome: OutcomeDeclared, Manifest: ".patchy/agent.yaml", Image: "ghcr.io/org/app:1"}, ""},
		{"invalid yaml never falls through to a valid devcontainer",
			Files{AgentYAML: present("build: {}\n"), Devcontainer: present(`{"image": "ghcr.io/org/dev:1"}`)},
			Declaration{},
			"`.patchy/agent.yaml` has a `build` key; patchy does not build images. Publish the image and set `image`"},
		{"oversize yaml is a rejection",
			Files{AgentYAML: File{Present: true, Size: 70000}, Devcontainer: present(`{"image": "x"}`)},
			Declaration{}, "`.patchy/agent.yaml` is 70000 bytes; the limit is 65536 bytes"},
		{"devcontainer image alone is declared with its manifest",
			Files{Devcontainer: present(`{"image": "ghcr.io/org/dev:1"}`)},
			Declaration{Outcome: OutcomeDeclared, Manifest: ".devcontainer/devcontainer.json",
				Image: "ghcr.io/org/dev:1"}, ""},
		{"build-based devcontainer alone is not applicable, not rejected",
			Files{Devcontainer: present(buildDC)},
			Declaration{Outcome: OutcomeNotApplicable, Manifest: ".devcontainer/devcontainer.json",
				Reason: buildMsg("build")}, ""},
		{"unparseable devcontainer is not applicable",
			Files{Devcontainer: present(`{"image": `)},
			Declaration{Outcome: OutcomeNotApplicable, Manifest: ".devcontainer/devcontainer.json",
				Reason: "could not parse `.devcontainer/devcontainer.json`: unexpected end of JSON input at offset 10"},
			""},
		{"oversize devcontainer is not applicable",
			Files{Devcontainer: File{Present: true, Size: 70000}},
			Declaration{Outcome: OutcomeNotApplicable, Manifest: ".devcontainer/devcontainer.json",
				Reason: "`.devcontainer/devcontainer.json` is 70000 bytes; the limit is 65536 bytes"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Declare(c.files)
			if c.wantErr != "" {
				if err == nil || err.Error() != c.wantErr {
					t.Fatalf("Declare error = %v, want %q", err, c.wantErr)
				}
				if !IsRejection(err) {
					t.Errorf("error is not a Rejection: %T", err)
				}
			} else if err != nil {
				t.Fatalf("Declare: %v", err)
			}
			if got != c.want {
				t.Errorf("Declare =\n  %+v\nwant\n  %+v", got, c.want)
			}
		})
	}
}

func TestDeclareFromArchive(t *testing.T) {
	tarball := archive(t,
		entry{name: "org-repo-sha/.devcontainer/devcontainer.json", data: `{"build": {}}`},
		entry{name: "org-repo-sha/.patchy/agent.yaml", data: "image: ghcr.io/org/app:1.26\n"})
	files, err := ReadFiles(bytes.NewReader(tarball))
	if err != nil {
		t.Fatal(err)
	}
	d, err := Declare(files)
	if err != nil {
		t.Fatal(err)
	}
	want := Declaration{Outcome: OutcomeDeclared, Manifest: AgentYAMLPath, Image: "ghcr.io/org/app:1.26"}
	if d != want {
		t.Errorf("Declare = %+v, want %+v", d, want)
	}
}

func TestOutcomeString(t *testing.T) {
	cases := map[Outcome]string{
		OutcomeNone: "none", OutcomeDeclared: "declared", OutcomeNotApplicable: "not-applicable", Outcome(9): "outcome(9)",
	}
	for o, want := range cases {
		if got := o.String(); got != want {
			t.Errorf("Outcome(%d).String() = %q, want %q", int(o), got, want)
		}
	}
}

func TestRejection(t *testing.T) {
	err := reject("fixed %s", "message")
	if err.Error() != "fixed message" {
		t.Errorf("Error() = %q", err.Error())
	}
	if !IsRejection(err) || !IsRejection(fmt.Errorf("wrapped: %w", err)) {
		t.Error("IsRejection must see through wrapping")
	}
	if IsRejection(fmt.Errorf("plain")) || IsRejection(nil) {
		t.Error("IsRejection must be false for plain errors and nil")
	}
}
