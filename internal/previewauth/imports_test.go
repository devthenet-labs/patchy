// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestPureCoreImports keeps the core free of I/O: the standard library,
// internal/sealed and the api/v1alpha1 value types, and nothing else. Any
// Kubernetes client, HTTP server, OIDC or signing library belongs in an
// adapter.
func TestPureCoreImports(t *testing.T) {
	allowed := map[string]bool{
		"github.com/bitwise-media-group/patchy/internal/sealed": true,
		"github.com/bitwise-media-group/patchy/api/v1alpha1":    true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
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
			if strings.Contains(first, ".") && !allowed[path] {
				t.Errorf("%s imports %s: the pure core imports only the standard library, internal/sealed and api/v1alpha1",
					name, path)
			}
			if path == "net/http/httptest" || strings.HasPrefix(path, "os/exec") {
				t.Errorf("%s imports %s", name, path)
			}
		}
	}
}
