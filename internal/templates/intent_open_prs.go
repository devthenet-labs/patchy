// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// OpenPullRequest is another intent's pull request, which a plan prompt
// lists so the planner can steer clear of the files it changes: an intent
// of the same Project, not ended, with the pull request open in one of the
// Project's repositories. The intent controller reads it from GitHub when
// it creates the plan run and carries it to the pod as JSON
// (PATCHY_OPEN_PULL_REQUESTS, EncodeOpenPullRequests), like
// PreviousAttempt: the runner has no GitHub access.
//
// Every string in it is UNTRUSTED. The title is whatever anyone who can
// edit the pull request wrote, the paths whatever anyone who can push to
// its branch named, and the rest is copied from them. The prompt quotes
// them bounded and stripped (BoundOpenPullRequests), in one fence no line
// of them can close, under a statement that they are data, not
// instructions.
type OpenPullRequest struct {
	// Intent is the other intent's name.
	Intent string `json:"intent"`
	// Repository is the repository the pull request is in, as the Project
	// (and so the request's repository list) spells it.
	Repository string `json:"repository"`
	// Number and URL are the pull request's; Title is its title on GitHub.
	Number int64  `json:"number"`
	URL    string `json:"url,omitempty"`
	Title  string `json:"title,omitempty"`
	// Files are the first files it changes, in GitHub's order;
	// ChangedFiles is how many it changes in all, which may be more.
	Files        []OpenPullRequestFile `json:"files,omitempty"`
	ChangedFiles int                   `json:"changedFiles,omitempty"`
}

// OpenPullRequestFile is one file an open pull request changes, and the
// path it was renamed from, if any, which the pull request changes too.
type OpenPullRequestFile struct {
	Path string `json:"path"`
	From string `json:"from,omitempty"`
}

// Bounds on the open pull requests a plan prompt lists. A file whose path
// (or the path it was renamed from) is over OpenPullRequestPathMaxBytes, or
// holds a character a listing cannot show as it is, is not listed: it is
// counted with the files the listing leaves out rather than shown altered.
// The listed paths share one budget across the list, which keeps the JSON
// a Job carries under OpenPullRequestsMaxBytes whatever they hold.
const (
	OpenPullRequestsMax          = 5
	OpenPullRequestMaxFiles      = 50
	OpenPullRequestPathMaxBytes  = 256
	OpenPullRequestTitleMaxBytes = 256
	OpenPullRequestsPathBudget   = 16 << 10
	// OpenPullRequestsMaxBytes bounds EncodeOpenPullRequests' JSON: well
	// inside the 128 KiB Linux allows one environment string.
	OpenPullRequestsMaxBytes = 64 << 10

	openPullRequestNameMaxBytes = 253
	openPullRequestURLMaxBytes  = 512
	// openPullRequestMaxCount bounds ChangedFiles, which GitHub reports
	// and the listing prints.
	openPullRequestMaxCount = 1 << 20
)

// BoundOpenPullRequests returns prs as a prompt may list them: the first
// OpenPullRequestsMax with a pull request number; the intent, repository,
// URL and title of each on one line, free of control and format
// characters, valid UTF-8, and cut to their bounds on a rune boundary; and
// its first OpenPullRequestMaxFiles listable files within the list's path
// budget, the rest counted in ChangedFiles. It is idempotent, and nil for
// none.
func BoundOpenPullRequests(prs []OpenPullRequest) []OpenPullRequest {
	var out []OpenPullRequest
	budget := OpenPullRequestsPathBudget
	for _, pr := range prs {
		if len(out) == OpenPullRequestsMax {
			break
		}
		if pr.Number <= 0 {
			continue
		}
		b := OpenPullRequest{
			Intent:     oneLineText(pr.Intent, openPullRequestNameMaxBytes),
			Repository: oneLineText(pr.Repository, openPullRequestURLMaxBytes),
			Number:     pr.Number,
			URL:        oneLineText(pr.URL, openPullRequestURLMaxBytes),
			Title:      oneLineText(pr.Title, OpenPullRequestTitleMaxBytes),
		}
		for _, f := range pr.Files {
			size := len(f.Path) + len(f.From)
			if len(b.Files) == OpenPullRequestMaxFiles || size > budget || !listablePath(f.Path) ||
				f.From != "" && !listablePath(f.From) {
				continue
			}
			budget -= size
			b.Files = append(b.Files, f)
		}
		b.ChangedFiles = min(max(pr.ChangedFiles, len(pr.Files), len(b.Files)), openPullRequestMaxCount)
		out = append(out, b)
	}
	return out
}

// oneLineText is untrusted text on one line: plainText, its whitespace runs
// (line breaks and tabs among them) one space, cut to limit bytes.
func oneLineText(s string, limit int) string {
	return strings.TrimSpace(cutRunes(strings.Join(strings.Fields(plainText(s)), " "), limit))
}

// listablePath reports a path a listing can show exactly as it is: not
// empty, within OpenPullRequestPathMaxBytes, valid UTF-8, and with no
// control or format character and no white space but the plain space, so
// it stays one line and every character of it shows.
func listablePath(p string) bool {
	if p == "" || len(p) > OpenPullRequestPathMaxBytes || !utf8.ValidString(p) {
		return false
	}
	return !strings.ContainsFunc(p, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r != ' ' && unicode.IsSpace(r)
	})
}

// EncodeOpenPullRequests is the JSON a plan Job hands agent-runner as
// PATCHY_OPEN_PULL_REQUESTS: prs bounded (BoundOpenPullRequests), with
// HTML left unescaped so only a quote or a backslash grows in the
// encoding, which keeps it within OpenPullRequestsMaxBytes. "" for none.
func EncodeOpenPullRequests(prs []OpenPullRequest) string {
	prs = BoundOpenPullRequests(prs)
	if len(prs) == 0 {
		return ""
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(prs); err != nil {
		// Strings, numbers and slices of them always encode: never.
		return ""
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// DecodeOpenPullRequests reads EncodeOpenPullRequests' JSON back, bounded
// again whatever it holds; "" is none.
func DecodeOpenPullRequests(s string) ([]OpenPullRequest, error) {
	if s == "" {
		return nil, nil
	}
	var prs []OpenPullRequest
	if err := json.Unmarshal([]byte(s), &prs); err != nil {
		return nil, fmt.Errorf("open pull requests: %w", err)
	}
	return BoundOpenPullRequests(prs), nil
}

// openPullRequestsListing is the plan prompt's listing of bounded prs, one
// paragraph each, which the prompt quotes in one fence.
func openPullRequestsListing(prs []OpenPullRequest) string {
	paras := make([]string, 0, len(prs))
	for _, pr := range prs {
		var b strings.Builder
		fmt.Fprintf(&b, "Pull request #%d in %s, from intent %s\n", pr.Number, pr.Repository, pr.Intent)
		if pr.URL != "" {
			fmt.Fprintf(&b, "URL: %s\n", pr.URL)
		}
		if pr.Title != "" {
			fmt.Fprintf(&b, "Title: %s\n", pr.Title)
		}
		fmt.Fprintf(&b, "Files it changes (%d):\n", pr.ChangedFiles)
		for _, f := range pr.Files {
			if f.From != "" {
				fmt.Fprintf(&b, "  %s (renamed from %s)\n", f.Path, f.From)
				continue
			}
			fmt.Fprintf(&b, "  %s\n", f.Path)
		}
		if more := pr.ChangedFiles - len(pr.Files); more > 0 {
			fmt.Fprintf(&b, "  and %d more not listed\n", more)
		}
		paras = append(paras, b.String())
	}
	return strings.Join(paras, "\n")
}
