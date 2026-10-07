// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// openCandidate is another intent's pull request recorded open in one of
// the Project's repositories, before GitHub is asked about it.
type openCandidate struct {
	intent *v1alpha1.Intent
	// repo is the repository as the Project spells it, and order its place
	// in the Project's list.
	repo  string
	order int
	pr    v1alpha1.IntentPullRequest
}

// otherOpenPullRequests is what the plan run is told of the other intents' open
// pull requests (keyOpenPullRequests): each pull request another Intent of
// this Project that has not ended records open in one of the Project's
// repositories, which are the repositories any plan of it may change, read
// from GitHub as it is now: its title, how many files it changes and the
// first of them. Only the Project's own intents are read: a Project is
// patchy's boundary of who sees what, and its plan is posted to its own
// intent repository. At most templates.OpenPullRequestsMax are read, the
// longest-open intent's first.
//
// A pull request GitHub no longer has, or reports closed, is left out. Any
// other failure to read one, the rate budget under its floor included,
// leaves the whole list out, with a log line, rather than failing the plan
// or telling it of only some: the list helps a plan avoid a conflict, and
// a plan made without it is the plan patchy made before it existed. ""
// for none.
func (p *pass) otherOpenPullRequests(ctx context.Context) string {
	omit := func(why string, err error, attrs ...slog.Attr) string {
		attrs = append([]slog.Attr{slog.String("intent", p.in.Name), slog.String("reason", why)}, attrs...)
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
		}
		p.r.log().LogAttrs(ctx, slog.LevelWarn,
			"the other intents' open pull requests could not be read; the plan is made without them", attrs...)
		return ""
	}
	var list v1alpha1.IntentList
	if err := p.r.List(ctx, &list, client.InNamespace(p.in.Namespace)); err != nil {
		return omit("list the intents", err)
	}
	candidates := p.openCandidates(&list)
	var out []templates.OpenPullRequest
	for _, c := range candidates[:min(len(candidates), templates.OpenPullRequestsMax)] {
		attrs := []slog.Attr{slog.String("repository", c.repo), slog.Int64("pullRequest", c.pr.Number)}
		if ok, err := p.rateOK(ctx, c.repo); err != nil || !ok {
			return omit("the rate budget", err, attrs...)
		}
		pr, err := p.r.GitHub.GetPullRequest(ctx, c.repo, c.pr.Number)
		if ghclient.IsNotFound(err) {
			continue
		}
		if err != nil {
			return omit("read the pull request", err, attrs...)
		}
		if pr.State != prOpen {
			continue
		}
		files, err := p.r.GitHub.ListPullRequestFiles(ctx, c.repo, c.pr.Number, templates.OpenPullRequestMaxFiles)
		if ghclient.IsNotFound(err) {
			continue
		}
		if err != nil {
			return omit("list the pull request's files", err, attrs...)
		}
		url := c.pr.URL
		if url == "" {
			url = fmt.Sprintf("%s/pull/%d", strings.TrimSuffix(c.repo, "/"), c.pr.Number)
		}
		open := templates.OpenPullRequest{Intent: c.intent.Name, Repository: c.repo, Number: c.pr.Number,
			URL: url, Title: pr.Title, ChangedFiles: pr.ChangedFiles}
		for _, f := range files {
			open.Files = append(open.Files, templates.OpenPullRequestFile{Path: f.Path, From: f.PreviousPath})
		}
		out = append(out, open)
	}
	return templates.EncodeOpenPullRequests(out)
}

// openCandidates are the pull requests the other intents of this Project
// that have not ended (nor are being deleted) record open, or not yet
// settled, in one of the Project's repositories: the longest-open intent's
// first, then in the Project's repository order and by number.
func (p *pass) openCandidates(list *v1alpha1.IntentList) []openCandidate {
	var out []openCandidate
	for i := range list.Items {
		in := &list.Items[i]
		if in.Name == p.in.Name || in.Spec.Project != p.in.Spec.Project ||
			!in.DeletionTimestamp.IsZero() || terminal(in.Status.Phase) {
			continue
		}
		for _, pr := range in.Status.PullRequests {
			if (pr.State != "" && pr.State != prOpen) || pr.Number <= 0 {
				continue
			}
			order := slices.IndexFunc(p.proj.Spec.Repositories, func(r v1alpha1.ProjectRepository) bool {
				return sameRepo(r.URL, pr.Repository)
			})
			if order < 0 {
				continue
			}
			out = append(out, openCandidate{intent: in, repo: p.proj.Spec.Repositories[order].URL, order: order,
				pr: pr})
		}
	}
	slices.SortStableFunc(out, func(a, b openCandidate) int {
		return cmp.Or(a.intent.CreationTimestamp.Compare(b.intent.CreationTimestamp.Time),
			strings.Compare(a.intent.Name, b.intent.Name), cmp.Compare(a.order, b.order),
			cmp.Compare(a.pr.Number, b.pr.Number))
	})
	return out
}
