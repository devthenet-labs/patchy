// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intentperm

import (
	"slices"
	"strings"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// Permission names, as GitHub spells them in an App manifest's
// default_permissions and in an installation token request.
const (
	Issues       = "issues"
	Contents     = "contents"
	PullRequests = "pull_requests"
	Checks       = "checks"
	Statuses     = "statuses"
	Actions      = "actions"
)

// Access levels. Write includes read.
const (
	Read  = "read"
	Write = "write"
)

// Grant is one permission at one access level.
type Grant struct {
	Permission string
	Access     string
}

// String renders the grant the way GitHub's API spells it, e.g.
// "pull_requests: write".
func (g Grant) String() string { return g.Permission + ": " + g.Access }

// Role is what a repository is to a Project.
type Role string

const (
	// RoleIntent is the intent repository, whose issues are the intents.
	RoleIntent Role = "intent"
	// RoleApp is an application repository intents build in.
	RoleApp Role = "app"
)

// Requirement is the grants one repository of a Project needs.
type Requirement struct {
	Role Role
	// Key is the repository's key within the Project
	// (spec.repositories[].name); empty for the intent repository.
	Key string
	// URL is the repository's https URL as the Project spells it.
	URL    string
	Grants []Grant
}

// Intent is what intents need on the intent repository.
func Intent() []Grant {
	return []Grant{{Issues, Write}}
}

// App is what intents need on an application repository: contents write
// (push the intent branch), pull requests write (open the pull request, read
// its reviews and conversation), and the RepositoryReads. checkFix adds the
// reads a check-fix round makes: the check runs and their annotations, the
// commit statuses, and the Actions jobs and log tails behind a failed run.
func App(checkFix bool) []Grant {
	out := []Grant{{Contents, Write}, {PullRequests, Write}}
	out = append(out, RepositoryReads()...)
	if checkFix {
		out = append(out, CheckFix()...)
	}
	return out
}

// RepositoryReads is the grant intent-controller reads a repository's own
// facts with, on every repository it polls: a reviewer's or commenter's
// collaborator permission (who may approve) and the installation's rate
// budget. Both need only the metadata read every token carries, but a token
// must request some permission, and intent-controller requests issues read.
// Intent covers it on the intent repository (write includes read); an
// application repository lists it on its own.
func RepositoryReads() []Grant {
	return []Grant{{Issues, Read}}
}

// CheckFix are the reads a check-fix round adds on an application
// repository (spec.checks.fix non-empty).
func CheckFix() []Grant {
	return []Grant{{Checks, Read}, {Statuses, Read}, {Actions, Read}}
}

// For is the table for one Project: the intent repository first, then every
// entry of spec.repositories in order. A repository listed in two roles (an
// intent repository that is also an application repository) appears once
// per role, so each role's grants stay visible on their own.
func For(spec *v1alpha1.ProjectSpec) []Requirement {
	out := make([]Requirement, 0, 1+len(spec.Repositories))
	out = append(out, Requirement{Role: RoleIntent, URL: spec.IntentRepository, Grants: Intent()})
	checkFix := len(spec.Checks.Fix) > 0
	for _, r := range spec.Repositories {
		out = append(out, Requirement{Role: RoleApp, Key: r.Name, URL: r.URL, Grants: App(checkFix)})
	}
	return out
}

// Merge combines grant sets into one, a grant per permission at the highest
// access any set asks for, sorted by permission name: what an App must hold
// to serve them all.
func Merge(sets ...[]Grant) []Grant {
	access := map[string]string{}
	for _, set := range sets {
		for _, g := range set {
			if cur, ok := access[g.Permission]; !ok || cur == Read && g.Access == Write {
				access[g.Permission] = g.Access
			}
		}
	}
	out := make([]Grant, 0, len(access))
	for p, a := range access {
		out = append(out, Grant{p, a})
	}
	slices.SortFunc(out, func(a, b Grant) int { return strings.Compare(a.Permission, b.Permission) })
	return out
}
