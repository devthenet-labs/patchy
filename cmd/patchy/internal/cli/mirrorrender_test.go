// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/printer"
	"github.com/bitwise-media-group/patchy/internal/mirror"
)

func TestRenderMirrorEvent(t *testing.T) {
	cases := []struct {
		event mirror.Event
		want  string
	}{
		{mirror.Event{Kind: "info", Entry: "demo", Message: "pulled chart"}, "patchy: [demo] pulled chart\n"},
		{mirror.Event{Kind: "warn", Entry: "demo", Message: "unsigned"}, "patchy: warning: [demo] unsigned\n"},
		{mirror.Event{Kind: "warn", Message: "no scanner"}, "patchy: warning: no scanner\n"},
		{mirror.Event{Kind: "info", Message: "done"}, "patchy: done\n"},
	}
	for _, tc := range cases {
		out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
		renderMirrorEvent(&Options{Out: out, ErrOut: errOut}, tc.event)
		if errOut.String() != tc.want {
			t.Errorf("event %+v rendered %q, want %q", tc.event, errOut.String(), tc.want)
		}
		if out.Len() != 0 {
			t.Errorf("narration reached stdout: %q", out.String())
		}
	}
}

var syncResults = []mirror.SyncResult{
	{Name: "demo", Kind: "Chart", Records: []mirror.SyncRecord{
		{Registry: "primary", Ref: "registry.example.com/org/platform/charts/demo:1.0.0", Action: "pushed", Signed: true},
		{Registry: "primary", Ref: "registry.example.com/org/platform/app@sha256:aa", Action: "skipped"},
	}},
	{Name: "broken", Kind: "Chart", Err: "upstream mutated"},
}

func TestRenderSyncResultsTable(t *testing.T) {
	out := &bytes.Buffer{}
	if err := renderSyncResults(&Options{Out: out}, syncResults, printer.FormatTable); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want a header and three rows:\n%s", len(lines), out.String())
	}
	for i, want := range [][]string{
		{"ENTRY", "REGISTRY", "REF", "ACTION", "SIGNED"},
		{"demo", "primary", "charts/demo:1.0.0", "pushed", "signed"},
		{"demo", "primary", "app@sha256:aa", "skipped"},
		{"broken", "failed: upstream mutated"},
	} {
		for _, w := range want {
			if !strings.Contains(lines[i], w) {
				t.Errorf("line %d %q missing %q", i, lines[i], w)
			}
		}
	}
	if strings.Contains(lines[2], "signed") {
		t.Errorf("an unsigned record claims a signature: %q", lines[2])
	}
}

func TestRenderSyncResultsJSON(t *testing.T) {
	out := &bytes.Buffer{}
	if err := renderSyncResults(&Options{Out: out}, syncResults, printer.FormatJSON); err != nil {
		t.Fatal(err)
	}
	var got []mirror.SyncResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if len(got) != 2 || got[0].Records[0].Action != "pushed" || got[1].Err != "upstream mutated" {
		t.Errorf("round trip = %+v", got)
	}
}

// TestMirrorCompletions drives shell completion through cobra's own
// __complete entry point: entry names come from the store, registry names
// from mirror.yaml.
func TestMirrorCompletions(t *testing.T) {
	t.Chdir(t.TempDir())
	writeMirrorStore(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"__complete", "mirror", "sync", ""}, "demo"},
		{[]string{"__complete", "mirror", "upgrade", ""}, "demo"},
		{[]string{"__complete", "mirror", "sync", "--registry", ""}, "primary"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args[1:], " "), func(t *testing.T) {
			out, err := execDev(t, tc.args...)
			if err != nil {
				t.Fatalf("complete: %v", err)
			}
			first := strings.SplitN(out, "\n", 2)[0]
			if first != tc.want {
				t.Errorf("first completion = %q, want %q\n%s", first, tc.want, out)
			}
			// :4 is ShellCompDirectiveNoFileComp.
			if !strings.Contains(out, ":4\n") {
				t.Errorf("completion offers files:\n%s", out)
			}
		})
	}
}

func TestMirrorCompletionsWithoutStore(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{
		{"__complete", "mirror", "sync", ""},
		{"__complete", "mirror", "sync", "--registry", ""},
	} {
		out, err := execDev(t, args...)
		if err != nil {
			t.Fatalf("complete: %v", err)
		}
		if first := strings.SplitN(out, "\n", 2)[0]; first != ":4" {
			t.Errorf("%v offered %q outside a store", args, first)
		}
	}
}

func TestMirrorCompletionsBrokenStore(t *testing.T) {
	t.Chdir(t.TempDir())
	writeMirrorStore(t)
	// A manifest the strict parser refuses breaks discovery; a broken
	// mirror.yaml breaks the registry list. Neither may crash completion.
	if err := os.WriteFile(filepath.Join("charts", "demo", "manifest.yaml"), []byte("kind: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := execDev(t, "__complete", "mirror", "sync", "")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if first := strings.SplitN(out, "\n", 2)[0]; first != ":4" {
		t.Errorf("broken manifest completed %q", first)
	}
	if err := os.WriteFile("mirror.yaml", []byte("registries: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = execDev(t, "__complete", "mirror", "sync", "--registry", "")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if first := strings.SplitN(out, "\n", 2)[0]; first != ":4" {
		t.Errorf("broken mirror.yaml completed %q", first)
	}
}

func TestSelectMirrorEntries(t *testing.T) {
	t.Chdir(t.TempDir())
	writeMirrorStore(t)
	opts := &Options{Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	eng, err := mirrorEngine(opts, &mirrorFlags{}, root)
	if err != nil {
		t.Fatalf("mirrorEngine: %v", err)
	}
	cases := []struct {
		name    string
		names   []string
		all     bool
		group   string
		want    string
		wantErr string
	}{
		{"all", nil, true, "", "demo", ""},
		{"by name", []string{"demo"}, false, "", "demo", ""},
		// An entry outside any lockstep group is its own group.
		{"group by name", nil, false, "demo", "demo", ""},
		{"empty group", nil, false, "nope", "", `no entries in group "nope"`},
		{"unknown name", []string{"nope"}, false, "", "", "nope"},
		{"all with group", nil, true, "demo", "", "--all cannot be combined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectMirrorEntries(eng, tc.names, tc.all, tc.group)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("select: %v", err)
			}
			if len(got) != 1 || got[0].Name != tc.want {
				t.Errorf("entries = %+v, want [%s]", got, tc.want)
			}
		})
	}
}

func TestSelectMirrorEntriesEmptyStore(t *testing.T) {
	t.Chdir(t.TempDir())
	writeMirrorStore(t)
	if err := os.RemoveAll("charts"); err != nil {
		t.Fatal(err)
	}
	_, err := execDev(t, "mirror", "sync", "--all")
	if err == nil || !strings.Contains(err.Error(), "the store has no entries") {
		t.Fatalf("err = %v", err)
	}
}

func TestMirrorEngineBrokenConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	writeMirrorStore(t)
	if err := os.WriteFile("mirror.yaml", []byte("registries: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := execDev(t, "mirror", "sync", "--all"); err == nil {
		t.Fatal("sync ran against an unparseable mirror.yaml")
	}
}
