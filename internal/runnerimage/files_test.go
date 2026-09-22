// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

type entry struct {
	name     string
	data     string
	typeflag byte
}

// archive builds a gzip-compressed tar the way GitHub's archive endpoint
// does: every entry under one prefix directory.
func archive(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		flag := e.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: flag}
		switch flag {
		case tar.TypeReg:
			hdr.Size = int64(len(e.data))
		case tar.TypeSymlink:
			hdr.Linkname = e.data
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if flag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sameFile(a, b File) bool {
	return a.Present == b.Present && a.Size == b.Size && (a.Data == nil) == (b.Data == nil) && bytes.Equal(a.Data, b.Data)
}

func TestReadFiles(t *testing.T) {
	yaml := "image: ghcr.io/org/app:1\n"
	dc := `{"image": "ghcr.io/org/dev:1"}`
	big := strings.Repeat("#", MaxDeclarationBytes+1)
	exact := strings.Repeat("#", MaxDeclarationBytes)
	cases := []struct {
		name    string
		entries []entry
		want    Files
	}{
		{"both files under the archive prefix",
			[]entry{{name: "org-repo-abc123/README.md", data: "hi"},
				{name: "org-repo-abc123/.patchy/agent.yaml", data: yaml},
				{name: "org-repo-abc123/.devcontainer/devcontainer.json", data: dc}},
			Files{AgentYAML: File{Present: true, Size: int64(len(yaml)), Data: []byte(yaml)},
				Devcontainer: File{Present: true, Size: int64(len(dc)), Data: []byte(dc)}}},
		{"yaml only",
			[]entry{{name: "p/.patchy/agent.yaml", data: yaml}},
			Files{AgentYAML: File{Present: true, Size: int64(len(yaml)), Data: []byte(yaml)}}},
		{"devcontainer only",
			[]entry{{name: "p/.devcontainer/devcontainer.json", data: dc}},
			Files{Devcontainer: File{Present: true, Size: int64(len(dc)), Data: []byte(dc)}}},
		{"neither", []entry{{name: "p/main.go", data: "package main"}}, Files{}},
		{"a ./ prefix is tolerated",
			[]entry{{name: "./p/.patchy/agent.yaml", data: yaml}},
			Files{AgentYAML: File{Present: true, Size: int64(len(yaml)), Data: []byte(yaml)}}},
		{"an entry without the prefix component is ignored",
			[]entry{{name: ".patchy/agent.yaml", data: yaml}}, Files{}},
		{"deeper paths are ignored",
			[]entry{{name: "p/sub/.patchy/agent.yaml", data: yaml},
				{name: "p/.devcontainer/go/devcontainer.json", data: dc},
				{name: "p/.devcontainer.json", data: dc}},
			Files{}},
		{"a symlink at the path is ignored",
			[]entry{{name: "p/.patchy/agent.yaml", data: "../elsewhere", typeflag: tar.TypeSymlink}}, Files{}},
		{"a directory entry is ignored",
			[]entry{{name: "p/.patchy/agent.yaml", typeflag: tar.TypeDir}}, Files{}},
		{"an oversize file is present but unread",
			[]entry{{name: "p/.patchy/agent.yaml", data: big},
				{name: "p/.devcontainer/devcontainer.json", data: dc}},
			Files{AgentYAML: File{Present: true, Size: MaxDeclarationBytes + 1},
				Devcontainer: File{Present: true, Size: int64(len(dc)), Data: []byte(dc)}}},
		{"exactly the cap is read",
			[]entry{{name: "p/.devcontainer/devcontainer.json", data: exact}},
			Files{Devcontainer: File{Present: true, Size: MaxDeclarationBytes, Data: []byte(exact)}}},
		{"the first occurrence wins",
			[]entry{{name: "p/.patchy/agent.yaml", data: yaml}, {name: "p/.patchy/agent.yaml", data: "image: other\n"}},
			Files{AgentYAML: File{Present: true, Size: int64(len(yaml)), Data: []byte(yaml)}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ReadFiles(bytes.NewReader(archive(t, c.entries...)))
			if err != nil {
				t.Fatalf("ReadFiles: %v", err)
			}
			if !sameFile(got.AgentYAML, c.want.AgentYAML) {
				t.Errorf("AgentYAML = %+v, want %+v", got.AgentYAML, c.want.AgentYAML)
			}
			if !sameFile(got.Devcontainer, c.want.Devcontainer) {
				t.Errorf("Devcontainer = %+v, want %+v", got.Devcontainer, c.want.Devcontainer)
			}
		})
	}
}

func TestReadFilesStopsAfterBoth(t *testing.T) {
	// A truncated entry after the two files would be an error if the walk
	// read on; it must not, so the cost is bounded by their position.
	good := archive(t,
		entry{name: "p/.patchy/agent.yaml", data: "image: x\n"},
		entry{name: "p/.devcontainer/devcontainer.json", data: "{}"})
	var raw bytes.Buffer
	gz, err := gzip.NewReader(bytes.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ReadFrom(gz); err != nil {
		t.Fatal(err)
	}
	// Drop the tar end-of-archive blocks and append a header claiming a
	// large body that never arrives.
	tarBytes := bytes.TrimRight(raw.Bytes(), "\x00")
	pad := 512 - len(tarBytes)%512
	if pad < 512 {
		tarBytes = append(tarBytes, make([]byte, pad)...)
	}
	var trailer bytes.Buffer
	tw := tar.NewWriter(&trailer)
	if err := tw.WriteHeader(&tar.Header{Name: "p/big.bin", Mode: 0o644, Size: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	tw.Flush() //nolint:errcheck // the header is what matters; the body is deliberately absent
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	w.Write(tarBytes)        //nolint:errcheck // in-memory
	w.Write(trailer.Bytes()) //nolint:errcheck // in-memory
	w.Close()                //nolint:errcheck // in-memory
	files, err := ReadFiles(&out)
	if err != nil {
		t.Fatalf("ReadFiles read past both declaration files: %v", err)
	}
	if !files.AgentYAML.Present || !files.Devcontainer.Present {
		t.Errorf("files = %+v, want both present", files)
	}
}

func TestReadFilesErrors(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"not gzip", []byte("plain text"), "runnerimage: open archive: "},
		{"truncated tar", archive(t, entry{name: "p/.patchy/agent.yaml", data: "image: x\n"})[:40],
			"runnerimage: "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ReadFiles(bytes.NewReader(c.in))
			if err == nil || !strings.HasPrefix(err.Error(), c.want) {
				t.Errorf("err = %v, want prefix %q", err, c.want)
			}
			if IsRejection(err) {
				t.Errorf("a corrupt artifact must not be a Rejection: %v", err)
			}
		})
	}
}

func TestArchivePath(t *testing.T) {
	cases := map[string]string{
		"p/.patchy/agent.yaml":      ".patchy/agent.yaml",
		"./p/.patchy/agent.yaml":    ".patchy/agent.yaml",
		".patchy/agent.yaml":        "agent.yaml",
		"/p/.patchy/agent.yaml":     "",
		"noslash":                   "",
		"p/":                        "",
		"p/.devcontainer/x/dc.json": ".devcontainer/x/dc.json",
	}
	for in, want := range cases {
		if got := archivePath(in); got != want {
			t.Errorf("archivePath(%q) = %q, want %q", in, got, want)
		}
	}
}
