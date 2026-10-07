// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package keydir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	masterA = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ01"
	masterB = "ZYXWVUTSRQPONMLKJIHGFEDCBA9876543210zyxwvutsrqponmlkjihgfedcba10"
)

func writeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, v := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func base() map[string]string {
	return map[string]string{FileMaster: masterA + "\n", FileGeneration: "3\n", FileSigningKey: "PEM\n"}
}

func TestLoad(t *testing.T) {
	k, err := Load(writeDir(t, base()))
	if err != nil {
		t.Fatal(err)
	}
	if k.Ring.Generation() != 3 || string(k.SigningKey) != "PEM" || k.PreviousSigningKey != nil {
		t.Fatalf("keys %+v", k)
	}
	rotated := base()
	rotated[FilePreviousMaster], rotated[FilePreviousGeneration], rotated[FilePreviousSigningKey] = masterB, "2", "OLD"
	k, err = Load(writeDir(t, rotated))
	if err != nil {
		t.Fatal(err)
	}
	// A slot secret of the previous generation still authenticates.
	prev, err := Load(writeDir(t, map[string]string{FileMaster: masterB, FileGeneration: "2", FileSigningKey: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if !k.Ring.VerifyClientSecret(1, prev.Ring.ClientSecret(1)) || string(k.PreviousSigningKey) != "OLD" {
		t.Fatal("the previous generation is not loaded")
	}
}

func TestLoadRefuses(t *testing.T) {
	tests := map[string]func(map[string]string){
		"no master":           func(m map[string]string) { delete(m, FileMaster) },
		"empty master":        func(m map[string]string) { m[FileMaster] = "\n" },
		"short master":        func(m map[string]string) { m[FileMaster] = "short" },
		"no generation":       func(m map[string]string) { delete(m, FileGeneration) },
		"signed generation":   func(m map[string]string) { m[FileGeneration] = "+3" },
		"leading zero":        func(m map[string]string) { m[FileGeneration] = "03" },
		"negative generation": func(m map[string]string) { m[FileGeneration] = "-1" },
		"huge generation":     func(m map[string]string) { m[FileGeneration] = "1000000" },
		"no signing key":      func(m map[string]string) { delete(m, FileSigningKey) },
		"half a previous":     func(m map[string]string) { m[FilePreviousMaster] = masterB },
		"other half":          func(m map[string]string) { m[FilePreviousGeneration] = "2" },
		"same generation": func(m map[string]string) {
			m[FilePreviousMaster], m[FilePreviousGeneration] = masterB, "3"
		},
		"same master": func(m map[string]string) {
			m[FilePreviousMaster], m[FilePreviousGeneration] = masterA, "2"
		},
		"oversized": func(m map[string]string) { m[FileSigningKey] = strings.Repeat("x", maxFile+1) },
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			m := base()
			edit(m)
			if _, err := Load(writeDir(t, m)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
