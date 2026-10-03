// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"regexp"
	"slices"
	"strings"
)

// The preview derivation. One pure function decides what an Intent's Preview
// is: intent-controller's preview-source reconciler writes Preview spec from
// it, and preview-controller re-derives it before rendering anything, so the
// writer and the check cannot drift. Everything it reads is operator Project
// configuration or Intent status the intent reconciler recorded from GitHub;
// no issue or agent text reaches a Preview.

// previewRevisionPattern is the only revision a preview runs: a full 40-hex
// commit SHA, whose image is the immutable sha-<SHA> tag.
var previewRevisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// previewPathPattern is the path grammar of repositories[].preview.path and
// components[].path, repeated from their schema markers so a Project the API
// server never validated (a fake client's) fails closed too.
var previewPathPattern = regexp.MustCompile(`^/([a-z0-9-]+(/[a-z0-9-]+)*)?$`)

// SameRepositoryURL reports whether two repository URLs name one repository,
// compared the way forges compare them: case-insensitively, with surrounding
// space, a trailing slash and any .git suffix dropped. The Project schema's
// rule that every repository is a different one compares the same way.
func SameRepositoryURL(a, b string) bool {
	return normalizeRepositoryURL(a) == normalizeRepositoryURL(b)
}

func normalizeRepositoryURL(u string) string {
	u = strings.ToLower(strings.TrimRight(strings.TrimSpace(u), "/"))
	return strings.TrimSuffix(u, ".git")
}

// RepositoryPreview is one previewed Project repository: its key and URL, its
// runtime contract, and the path its component is served under.
type RepositoryPreview struct {
	// Name is the repository's key, which names its component.
	Name string
	// URL is the repository's URL, which its pull request is matched by.
	URL string
	// Preview is the runtime contract.
	Preview ProjectPreview
	// Path is the effective path: "/" when the Project omitted it.
	Path string
}

// EffectivePreviews returns a Project's previewed repositories in Project
// order: each repositories[].preview, or, for the spec.preview shorthand,
// repositories[0] at "/". A Project the schema would refuse — both forms, the
// shorthand beside more or fewer than one repository, more than
// MaxPreviewComponents previewed repositories, a malformed or repeated path —
// previews nothing, so a Project the API server never validated fails closed.
func EffectivePreviews(p *Project) []RepositoryPreview {
	repos := p.Spec.Repositories
	perRepo := slices.ContainsFunc(repos, func(r ProjectRepository) bool { return r.Preview != nil })
	if p.Spec.Preview != nil {
		if perRepo || len(repos) != 1 {
			return nil
		}
		return []RepositoryPreview{{Name: repos[0].Name, URL: repos[0].URL, Preview: *p.Spec.Preview, Path: "/"}}
	}
	var out []RepositoryPreview
	for _, r := range repos {
		if r.Preview == nil {
			continue
		}
		path := r.Preview.Path
		if path == "" {
			path = "/"
		}
		if !previewPathPattern.MatchString(path) ||
			slices.ContainsFunc(out, func(o RepositoryPreview) bool { return o.Path == path }) {
			return nil
		}
		out = append(out, RepositoryPreview{Name: r.Name, URL: r.URL, Preview: r.Preview.ProjectPreview, Path: path})
	}
	if len(out) > MaxPreviewComponents {
		return nil
	}
	return out
}

// IntentWantsPreview reports whether an Intent is in a state its Preview
// exists in: InReview or Revising, or Blocked from one of them, with at least
// one of its pull requests open. Any other phase — a terminal one, or a block
// before review began — has no Preview.
func IntentWantsPreview(in *Intent) bool {
	switch in.Status.Phase {
	case IntentInReview, IntentRevising:
	case IntentBlocked:
		if from := IntentBlockedFrom(in); from != IntentInReview && from != IntentRevising {
			return false
		}
	default:
		return false
	}
	return slices.ContainsFunc(in.Status.PullRequests, func(pr IntentPullRequest) bool { return pr.State == "open" })
}

// DesiredPreviewComponents derives an Intent's Preview components from its
// Project, and reports false when the Intent has no Preview. There is one
// component per previewed repository (EffectivePreviews), in Project order,
// named by the repository's key, with the repository's runtime contract and
// path. Its revision is the recorded head of the Intent's pull request in
// that repository, whatever the pull request's state; or, when the Intent
// has no pull request there, the default-branch head recorded in
// status.previewBases. A pull request whose recorded head is not a 40-hex SHA
// gives no revision: its repository is never shown at another commit
// instead. There is no Preview unless IntentWantsPreview holds, every
// component has a revision, and at least one revision is a pull request's.
//
// The root path is left out of a component (PreviewComponent.Path omitted),
// the form every single-repository Preview has always been written in, so a
// one-repository Project derives exactly the component it always did.
func DesiredPreviewComponents(p *Project, in *Intent) ([]PreviewComponent, bool) {
	if !IntentWantsPreview(in) {
		return nil, false
	}
	previews := EffectivePreviews(p)
	if len(previews) == 0 {
		return nil, false
	}
	out := make([]PreviewComponent, 0, len(previews))
	fromPR := false
	for _, rp := range previews {
		revision, pr := previewRevision(in, rp.URL)
		if !previewRevisionPattern.MatchString(revision) {
			return nil, false
		}
		fromPR = fromPR || pr
		path := rp.Path
		if path == "/" {
			path = ""
		}
		out = append(out, PreviewComponent{
			Name: rp.Name, ImageRepository: rp.Preview.ImageRepository, Revision: revision,
			Port: rp.Preview.Port, ReadinessPath: rp.Preview.ReadinessPath, Path: path,
		})
	}
	if !fromPR {
		return nil, false
	}
	return out, true
}

// previewRevision is the revision a previewed repository runs and whether it
// is a pull request's head: the Intent's pull request in the repository, else
// its recorded preview base, else none.
func previewRevision(in *Intent, url string) (string, bool) {
	for _, pr := range in.Status.PullRequests {
		if SameRepositoryURL(pr.Repository, url) {
			return pr.HeadSHA, true
		}
	}
	for _, base := range in.Status.PreviewBases {
		if SameRepositoryURL(base.Repository, url) {
			return base.SHA, false
		}
	}
	return "", false
}
