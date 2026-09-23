// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	"github.com/bitwise-media-group/patchy/internal/envelope"
)

func changesetOf(paths ...string) *envelope.Changeset {
	cs := &envelope.Changeset{BaseSHA: "abc123", CommitMessage: "fix"}
	for _, p := range paths {
		cs.Upserts = append(cs.Upserts, envelope.FileChange{Path: p, Mode: "100644", ContentB64: "eA=="})
	}
	return cs
}

// TestValidateChangeset names each refusal, and shows the repository-image
// rules touching CI definitions only.
func TestValidateChangeset(t *testing.T) {
	tests := []struct {
		name     string
		cs       *envelope.Changeset
		repoImg  bool
		wantErr  string
		wantPass bool
	}{
		{"ordinary fix", changesetOf("cmd/main.go", "go.sum", "docs/SECURITY.md"), true, "", true},
		{"dotfiles are fine", changesetOf(".gitignore", ".github/dependabot.yml", ".githooks/pre-commit"), true, "", true},
		{"at the cap", changesetOf("a", "b", "c"), false, "", true},
		{"deletes count toward the cap", &envelope.Changeset{BaseSHA: "abc123",
			Upserts: changesetOf("a", "b").Upserts, Deletes: []string{"c", "d"}}, false, "4 entries", false},
		{"empty path", changesetOf(""), false, "empty path", false},
		{"absolute", changesetOf("/etc/passwd"), false, "is absolute", false},
		{"parent component", changesetOf("src/../../x"), false, `".." component`, false},
		{"dot component", changesetOf("src/./x"), false, `"." component`, false},
		{"double slash", changesetOf("src//x"), false, "empty component", false},
		{"trailing slash", changesetOf("src/"), false, "empty component", false},
		{"inside .git", changesetOf(".git/hooks/pre-commit"), false, "inside .git", false},
		{"inside a nested .GIT", changesetOf("vendor/x/.GIT/config"), false, "inside .git", false},
		{"control character", changesetOf("a\nb"), false, "control character", false},
		{"NUL", changesetOf("a\x00b"), false, "control character", false},
		{"invalid UTF-8", changesetOf("a\xffb"), false, "not valid UTF-8", false},
		{"overlong", changesetOf(strings.Repeat("a", maxChangesetPathBytes+1)), false, "over the 4096-byte limit", false},
		{"workflow on a default image", changesetOf(".github/workflows/ci.yml"), false, "", true},
		{"workflow on a repository image", changesetOf(".github/workflows/ci.yml"), true, "is a CI definition", false},
		{"case-folded workflow on a repository image", changesetOf(".GitHub/Workflows/ci.yml"), true,
			"is a CI definition", false},
		{"action on a repository image", changesetOf(".github/actions/x/action.yml"), true, "is a CI definition", false},
		{".github replaced on a repository image", changesetOf(".github"), true, "is a CI definition", false},
		{"workflows dir replaced on a repository image", changesetOf(".github/workflows"), true, "is a CI definition", false},
		{"lookalike directory is fine", changesetOf(".github/workflows-docs/x.md"), true, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateChangeset(tt.cs, changesetRules{Base: "abc123", MaxEntries: 3, RepositoryImage: tt.repoImg})
			if tt.wantPass {
				if err != nil {
					t.Errorf("validateChangeset = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("validateChangeset = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestValidateChangesetBase: the base must be the pinned commit, on any
// run, and an unknown pin refuses rather than waves the changeset through.
func TestValidateChangesetBase(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		pinned  string
		wantErr string
	}{
		{"the pinned commit", "abc123", "abc123", ""},
		{"another commit", "f00d", "abc123", `changeset base "f00d" is not the repository's pinned commit "abc123"`},
		{"no base at all", "", "abc123", `changeset base "" is not the repository's pinned commit`},
		{"the pin unknown", "abc123", "", "pinned commit is unknown"},
		{"the pin unknown and no base", "", "", "pinned commit is unknown"},
		{"an overlong base is bounded in the message", strings.Repeat("f", 5000), "abc123",
			strings.Repeat("f", 64) + `" is not`},
	}
	for _, tt := range tests {
		for image, repoImg := range map[string]bool{"default": false, "repository": true} {
			t.Run(tt.name+"/"+image, func(t *testing.T) {
				cs := changesetOf("a.go")
				cs.BaseSHA = tt.base
				err := validateChangeset(cs, changesetRules{Base: tt.pinned, MaxEntries: 3, RepositoryImage: repoImg})
				if tt.wantErr == "" {
					if err != nil {
						t.Errorf("validateChangeset = %v, want nil", err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("validateChangeset = %v, want an error containing %q", err, tt.wantErr)
				}
			})
		}
	}
}

// TestValidateChangesetUpserts: an upsert must carry a mode a blob can
// have and content that decodes, on any run.
func TestValidateChangesetUpserts(t *testing.T) {
	tests := []struct {
		name    string
		fc      envelope.FileChange
		wantErr string
	}{
		{"regular", envelope.FileChange{Path: "a", Mode: "100644", ContentB64: "eA=="}, ""},
		{"executable", envelope.FileChange{Path: "a", Mode: "100755", ContentB64: "IyEvYmluL3No"}, ""},
		{"symlink", envelope.FileChange{Path: "a", Mode: "120000", ContentB64: "Yg=="}, ""},
		{"empty file", envelope.FileChange{Path: "a", Mode: "100644", ContentB64: ""}, ""},
		{"unknown mode", envelope.FileChange{Path: "a", Mode: "999999", ContentB64: "eA=="}, `has mode "999999"`},
		{"tree mode", envelope.FileChange{Path: "a", Mode: "040000", ContentB64: "eA=="}, `has mode "040000"`},
		{"gitlink mode", envelope.FileChange{Path: "a", Mode: "160000", ContentB64: "eA=="}, `has mode "160000"`},
		{"no mode", envelope.FileChange{Path: "a", Mode: "", ContentB64: "eA=="}, `has mode ""`},
		{"not base64", envelope.FileChange{Path: "a", Mode: "100644", ContentB64: "not base64!"},
			`"a" content is not base64`},
		{"unpadded", envelope.FileChange{Path: "a", Mode: "100644", ContentB64: "eA"}, `"a" content is not base64`},
		{"URL alphabet", envelope.FileChange{Path: "a", Mode: "100644", ContentB64: "-_-_"},
			`"a" content is not base64`},
	}
	for _, tt := range tests {
		for image, repoImg := range map[string]bool{"default": false, "repository": true} {
			t.Run(tt.name+"/"+image, func(t *testing.T) {
				cs := &envelope.Changeset{BaseSHA: "abc123", Upserts: []envelope.FileChange{tt.fc}}
				err := validateChangeset(cs, changesetRules{Base: "abc123", MaxEntries: 3, RepositoryImage: repoImg})
				if tt.wantErr == "" {
					if err != nil {
						t.Errorf("validateChangeset = %v, want nil", err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("validateChangeset = %v, want an error containing %q", err, tt.wantErr)
				}
			})
		}
	}
}

// genPath builds a slash-joined path from segments drawn over an alphabet
// that includes the separator's troublemakers ('.', upper case, space), so
// the generator reaches "..", ".git", ".GitHub" and empty components.
func genPath(r *rand.Rand) string {
	alphabet := []byte("abgGhHiItuwWks.-_ ")
	segs := make([]string, 1+r.Intn(5))
	for i := range segs {
		b := make([]byte, r.Intn(6))
		for j := range b {
			b[j] = alphabet[r.Intn(len(alphabet))]
		}
		segs[i] = string(b)
	}
	// Bias toward the interesting prefixes.
	switch r.Intn(4) {
	case 0:
		segs = append([]string{".github", "workflows"}, segs...)
	case 1:
		segs = append([]string{".github", "actions"}, segs...)
	}
	return strings.Join(segs, "/")
}

func pathConfig(seed int64) *quick.Config {
	return &quick.Config{
		MaxCount: 3000,
		Rand:     rand.New(rand.NewSource(seed)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(genPath(r))
		},
	}
}

// TestValidateChangesetProperties states the validator's invariants over
// generated paths: an accepted path never escapes the tree or touches .git;
// the repository-image rules only ever refuse more, and what they refuse
// beyond the default is exactly the CI definitions.
func TestValidateChangesetProperties(t *testing.T) {
	accepted := func(p string, repoImg bool) bool {
		return validateChangeset(changesetOf(p), changesetRules{
			Base: "abc123", MaxEntries: DefaultChangesetMaxEntries, RepositoryImage: repoImg,
		}) == nil
	}
	contained := func(p string) bool {
		if !accepted(p, false) {
			return true
		}
		if strings.HasPrefix(p, "/") {
			return false
		}
		for seg := range strings.SplitSeq(p, "/") {
			if seg == "" || seg == "." || seg == ".." || strings.EqualFold(seg, ".git") {
				return false
			}
		}
		return true
	}
	if err := quick.Check(contained, pathConfig(20260923)); err != nil {
		t.Errorf("an accepted path escapes the tree: %v", err)
	}
	stricter := func(p string) bool {
		repo, dflt := accepted(p, true), accepted(p, false)
		if !dflt {
			return !repo // the repository rules may only refuse more
		}
		return repo == !ciPath(p)
	}
	if err := quick.Check(stricter, pathConfig(20260924)); err != nil {
		t.Errorf("repository-image rules differ from the default by more than CI paths: %v", err)
	}
}

// TestValidateChangesetEntryCapProperty: the cap alone decides a changeset
// of acceptable paths — any count up to it passes, any count past it fails.
func TestValidateChangesetEntryCapProperty(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 500,
		Rand:     rand.New(rand.NewSource(20260925)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(r.Intn(40))
			args[1] = reflect.ValueOf(r.Intn(40))
			args[2] = reflect.ValueOf(1 + r.Intn(60))
		},
	}
	capped := func(upserts, deletes, maxEntries int) bool {
		cs := &envelope.Changeset{BaseSHA: "abc123"}
		for i := range upserts {
			cs.Upserts = append(cs.Upserts, envelope.FileChange{Path: "src/f" + strings.Repeat("x", i%7), Mode: "100644"})
		}
		for range deletes {
			cs.Deletes = append(cs.Deletes, "old/file")
		}
		err := validateChangeset(cs, changesetRules{Base: "abc123", MaxEntries: maxEntries, RepositoryImage: true})
		return (err == nil) == (upserts+deletes <= maxEntries)
	}
	if err := quick.Check(capped, cfg); err != nil {
		t.Error(err)
	}
}
