// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

func TestSameRepository(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"https://github.com/acme/web", "https://github.com/acme/web", true},
		{"https://github.com/acme/web", "https://GitHub.com/ACME/Web", true},
		{"https://github.com/acme/web", "https://github.com/acme/web.git", true},
		{"https://github.com/acme/web.GIT", "https://github.com/Acme/Web.Git", true},
		{"https://github.com/acme/Acme.Web_App", "https://github.com/ACME/acme.web_app.git", true},
		{"acme/web", "Acme/Web.git", true},
		{"https://github.com/acme/web", "https://github.com/acme/api", false},
		{"https://github.com/acme/web", "https://ghe.example.com/acme/web", false},
		{"https://github.com/acme/web", "https://github.com/acme/web.gitx", false},
		{"https://github.com/acme/web", "https://github.com/acme/webgit", false},
		// Two spellings of one repository, but not alike: a URL is not its path.
		{"https://github.com/acme/web", "acme/web", false},
		{"", "acme/web", false},
	}
	for _, tt := range tests {
		if got := SameRepository(tt.a, tt.b); got != tt.want {
			t.Errorf("SameRepository(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
		if got := SameRepository(tt.b, tt.a); got != tt.want {
			t.Errorf("SameRepository(%q, %q) = %v, want %v", tt.b, tt.a, got, tt.want)
		}
	}
}

func TestRepositoryPath(t *testing.T) {
	tests := []struct{ url, want string }{
		{"https://github.com/acme/web", "acme/web"},
		{"https://github.com/Acme/Acme.Web_App.git", "Acme/Acme.Web_App.git"},
		{"https://ghe.example.com:8443/acme/web", "acme/web"},
		{"http://github.com/acme/web", ""},
		{"https://github.com/acme", ""},
		{"https://github.com/acme/web/tree/main", ""},
		{"https://github.com/acme/web?x=1", ""},
		{"https://user@github.com/acme/web", ""},
		{"https://github.com/acme/web app", ""},
		{"acme/web", ""},
		{"", ""},
		{"https://github.com/acme/" + strings.Repeat("a", RepositoryURLMaxBytes), ""},
	}
	for _, tt := range tests {
		if got := RepositoryPath(tt.url); got != tt.want {
			t.Errorf("RepositoryPath(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

// repositoryNameAlphabet is what GitHub allows in an owner or a repository
// name, and what the generated repositories are spelled in.
const repositoryNameAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"

func genRepositoryName(r *rand.Rand) string {
	var b strings.Builder
	for range 1 + r.Intn(12) {
		b.WriteByte(repositoryNameAlphabet[r.Intn(len(repositoryNameAlphabet))])
	}
	return b.String()
}

// respell spells a reference the way a model or an operator might: its case
// changed and a ".git" suffix, in any case, added or not.
func respell(r *rand.Rand, s string) string {
	switch r.Intn(3) {
	case 0:
		s = strings.ToUpper(s)
	case 1:
		s = strings.ToLower(s)
	}
	return s + []string{"", ".git", ".GIT", ".Git"}[r.Intn(4)]
}

// TestSameRepositoryProperty: over generated repositories, SameRepository is
// symmetric, agrees with the properties' own statement of when two URLs
// name one repository (sameRepository), holds between a repository and any
// respelling of it, and RepositoryPath carries a URL's repository to its
// "owner/name" path, where SameRepository holds against the path the
// repository is spelled with.
func TestSameRepositoryProperty(t *testing.T) {
	type pair struct{ a, b, path string }
	cfg := quickConfig(20261003, func(args []reflect.Value, r *rand.Rand) {
		host := []string{"github.com", "GitHub.com", "ghe.example.com"}[r.Intn(3)]
		owner, name := genRepositoryName(r), genRepositoryName(r)
		a := "https://" + host + "/" + owner + "/" + name
		b := "https://" + host + "/" + respell(r, owner+"/"+name)
		if r.Intn(3) == 0 {
			// Often another repository altogether.
			b = "https://" + host + "/" + genRepositoryName(r) + "/" + genRepositoryName(r)
		}
		args[0] = reflect.ValueOf(pair{a, b, owner + "/" + name})
	})
	same := 0
	holds := func(p pair) bool {
		got := SameRepository(p.a, p.b)
		if got != SameRepository(p.b, p.a) || got != sameRepository(p.a, p.b) {
			return false
		}
		if got {
			same++
		}
		if !SameRepository(p.a, p.a) || !SameRepository(p.path, RepositoryPath(p.a)) {
			return false
		}
		// The path of either spelling names the repository exactly when the
		// spellings do.
		return SameRepository(RepositoryPath(p.b), p.path) == got
	}
	if err := quick.Check(holds, cfg); err != nil {
		t.Error(err)
	}
	if same < 100 {
		t.Errorf("only %d generated pairs named one repository; the generator stopped reaching the case", same)
	}
}
