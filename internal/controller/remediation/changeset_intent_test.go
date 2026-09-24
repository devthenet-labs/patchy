// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"unicode"

	"github.com/bitwise-media-group/patchy/internal/envelope"
)

// validateChangesetAtMain is the Finding path's validator exactly as main
// had it before ValidateChangeset was exported with its Deny option
// (2784e50, changeset.go's validateChangeset), kept as the oracle the
// exported entry point is compared against. The leaf checks it calls are
// unchanged and shared.
func validateChangesetAtMain(cs *envelope.Changeset, base string, maxEntries int, repositoryImage bool) error {
	switch {
	case base == "":
		return fmt.Errorf("the repository's pinned commit is unknown, so the changeset's base cannot be checked")
	case cs.BaseSHA != base:
		return fmt.Errorf("changeset base %.64q is not the repository's pinned commit %q", cs.BaseSHA, base)
	}
	if n := len(cs.Upserts) + len(cs.Deletes); repositoryImage && n > maxEntries {
		return fmt.Errorf("changeset has %d entries (upserts plus deletes), over the %d-entry limit", n, maxEntries)
	}
	check := func(p string) error {
		if err := checkChangesetPath(p); err != nil {
			return err
		}
		if !repositoryImage {
			return nil
		}
		if strings.ContainsFunc(p, unicode.IsControl) {
			return fmt.Errorf("changeset path %q contains a control character, which a run on a "+
				"repository-declared image may not use", p)
		}
		if ciPath(p) {
			return fmt.Errorf("changeset path %q is a CI definition, which a run on a repository-declared "+
				"image may not change", p)
		}
		return nil
	}
	for _, up := range cs.Upserts {
		if err := check(up.Path); err != nil {
			return err
		}
		if err := checkChangesetUpsert(up); err != nil {
			return err
		}
	}
	for _, p := range cs.Deletes {
		if err := check(p); err != nil {
			return err
		}
	}
	return nil
}

// findingCase is one generated changeset with the rules a Finding
// remediation would hold it to.
type findingCase struct {
	cs              *envelope.Changeset
	base            string
	maxEntries      int
	repositoryImage bool
}

func (c findingCase) String() string {
	return fmt.Sprintf("base %q, max %d, repository image %v, changeset %+v",
		c.base, c.maxEntries, c.repositoryImage, *c.cs)
}

// genDenyPath is genPath biased toward the intent deny directories, in
// every case, as themselves and as lookalikes.
func genDenyPath(r *rand.Rand) string {
	prefixes := []string{".patchy", ".PATCHY", ".devcontainer", ".DevContainer", ".github", ".patchy-docs", ".githubx"}
	p := genPath(r)
	switch r.Intn(3) {
	case 0:
		return prefixes[r.Intn(len(prefixes))]
	case 1:
		return prefixes[r.Intn(len(prefixes))] + "/" + p
	}
	return p
}

func genFindingCase(r *rand.Rand) findingCase {
	pick := func(xs ...string) string { return xs[r.Intn(len(xs))] }
	c := findingCase{
		cs:              &envelope.Changeset{BaseSHA: pick("abc123", "abc123", "abc123", "f00d", "")},
		base:            pick("abc123", "abc123", "abc123", ""),
		maxEntries:      1 + r.Intn(8),
		repositoryImage: r.Intn(2) == 1,
	}
	for range r.Intn(6) {
		c.cs.Upserts = append(c.cs.Upserts, envelope.FileChange{
			Path:       genDenyPath(r),
			Mode:       pick("100644", "100644", "100755", "120000", "040000", ""),
			ContentB64: pick("eA==", "eA==", "", "not base64!", "eA"),
		})
	}
	for range r.Intn(4) {
		c.cs.Deletes = append(c.cs.Deletes, genDenyPath(r))
	}
	return c
}

// TestValidateChangesetFindingVerdictsUnchanged: for every changeset and
// every rule set a Finding remediation can have, the exported entry point
// returns exactly what main's validator returned — the same refusal, word
// for word, or none. The Deny option exists for intents; a Finding's rules
// never carry one, so the Finding flow's verdicts cannot move.
func TestValidateChangesetFindingVerdictsUnchanged(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 5000,
		Rand:     rand.New(rand.NewSource(20260926)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(genFindingCase(r))
		},
	}
	var failure string
	same := func(c findingCase) bool {
		got := ValidateChangeset(c.cs, ChangesetRules{
			Base: c.base, MaxEntries: c.maxEntries, RepositoryImage: c.repositoryImage,
		})
		want := validateChangesetAtMain(c.cs, c.base, c.maxEntries, c.repositoryImage)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			failure = fmt.Sprintf("%v\n got %v\nwant %v", c, got, want)
			return false
		}
		return true
	}
	if err := quick.Check(same, cfg); err != nil {
		t.Errorf("%v\n%s", err, failure)
	}
}

// TestIntentChangesetRules names what an intent run may never change, and
// shows everything else held to the repository-image rules.
func TestIntentChangesetRules(t *testing.T) {
	tests := []struct {
		name    string
		cs      *envelope.Changeset
		wantErr string
	}{
		{"ordinary change", changesetOf("cmd/main.go", "internal/server/version.go"), ""},
		{"lookalike directories are fine", changesetOf(".patchy-docs/x", ".githubx/y", "docs/.github/z",
			".devcontainers/w"), ""},
		{"a workflow", changesetOf(".github/workflows/ci.yml"), "is under .github"},
		{"anything under .github", changesetOf(".github/dependabot.yml"), "is under .github"},
		{".github replaced", changesetOf(".github"), "is under .github"},
		{"the agent image declaration", changesetOf(".patchy/agent.yaml"), "is under .patchy"},
		{"the agent image recipe", changesetOf(".patchy/Dockerfile"), "is under .patchy"},
		{".patchy replaced", changesetOf(".patchy"), "is under .patchy"},
		{"the devcontainer declaration", changesetOf(".devcontainer/devcontainer.json"), "is under .devcontainer"},
		{"case-folded", changesetOf(".DevContainer/Dockerfile"), "is under .devcontainer"},
		{"a delete counts", &envelope.Changeset{BaseSHA: "abc123", Deletes: []string{".patchy/agent.yaml"}},
			"is under .patchy"},
		{"a control character", changesetOf("a\tb"), "control character"},
		{"the base", &envelope.Changeset{BaseSHA: "f00d"}, "is not the repository's pinned commit"},
		{"path shape", changesetOf("../x"), `".." component`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateChangeset(tt.cs, IntentChangesetRules("abc123", 0))
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("ValidateChangeset = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ValidateChangeset = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}

	rules := IntentChangesetRules("abc123", 0)
	if rules.MaxEntries != DefaultChangesetMaxEntries || !rules.RepositoryImage || rules.Base != "abc123" {
		t.Errorf("IntentChangesetRules = %+v, want the default cap and the repository-image rules", rules)
	}
	if got := IntentChangesetRules("abc123", 7).MaxEntries; got != 7 {
		t.Errorf("IntentChangesetRules cap = %d, want 7", got)
	}
	// The rules own their deny list: a caller's edit reaches no other.
	rules.Deny[0] = "x"
	if IntentChangesetRules("abc123", 0).Deny[0] != ".github" {
		t.Error("IntentChangesetRules shares its deny list")
	}
}

// TestDenyAppliesWhicheverImage: the deny list refuses on a default-image
// run too.
func TestDenyAppliesWhicheverImage(t *testing.T) {
	rules := IntentChangesetRules("abc123", 0)
	rules.RepositoryImage = false
	if err := ValidateChangeset(changesetOf(".patchy/agent.yaml"), rules); err == nil ||
		!strings.Contains(err.Error(), "is under .patchy") {
		t.Errorf("ValidateChangeset = %v, want the deny list applied without the repository-image rules", err)
	}
}

// TestIntentDenyProperty: over generated paths, an intent's rules refuse
// exactly what the Finding repository-image rules refuse plus the paths in
// or replacing .github, .patchy or .devcontainer, in any case.
func TestIntentDenyProperty(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 5000,
		Rand:     rand.New(rand.NewSource(20260927)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(genDenyPath(r))
		},
	}
	denied := func(p string) bool {
		first, _, _ := strings.Cut(strings.ToLower(p), "/")
		return first == ".github" || first == ".patchy" || first == ".devcontainer"
	}
	exact := func(p string) bool {
		intent := ValidateChangeset(changesetOf(p), IntentChangesetRules("abc123", 0)) == nil
		finding := ValidateChangeset(changesetOf(p), ChangesetRules{
			Base: "abc123", MaxEntries: DefaultChangesetMaxEntries, RepositoryImage: true,
		}) == nil
		return intent == (finding && !denied(p))
	}
	if err := quick.Check(exact, cfg); err != nil {
		t.Error(err)
	}
}
