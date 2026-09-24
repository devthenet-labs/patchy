// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package changeset

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestPureImports keeps the validator neutral ground between the
// controllers that share it: it may import the standard library and the
// changeset's own contract (internal/envelope), and nothing from a
// controller engine, Kubernetes or a forge — so remediation and
// intent-controller never couple through it.
func TestPureImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	const allowed = "github.com/bitwise-media-group/patchy/internal/envelope"
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(path, "/")
			if path != allowed && strings.Contains(first, ".") {
				t.Errorf("%s imports %s: the changeset package imports only the standard library and %s",
					name, path, allowed)
			}
		}
	}
}
