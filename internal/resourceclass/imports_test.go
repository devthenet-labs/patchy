// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package resourceclass

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestPureImports keeps the menu neutral ground between its readers: the
// job controllers' flags, intent-controller and, later, the CLI. It may
// import the standard library and apimachinery's resource quantity, and
// nothing else, so no reader links controller code through it.
func TestPureImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	const allowed = "k8s.io/apimachinery/pkg/api/resource"
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
				t.Errorf("%s imports %s: resourceclass imports only the standard library and %s", name, path, allowed)
			}
		}
	}
}
