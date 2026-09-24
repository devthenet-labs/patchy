// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/go-github/v90/github"
)

// walkPageCap bounds one walk (100 entries per page, so 5000 entries). A
// listing that runs past it is an error, not a truncated result: the issue
// events and comments feed authority decisions, and GitHub lists them oldest
// first, so a truncated walk would drop exactly the newest entries.
const walkPageCap = 50

// pageWalk is one paginated GET listing through the go-github client, for
// the endpoints whose go-github types drop fields patchy needs
// (performed_via_github_app, node_id) or that must be conditional.
type pageWalk struct {
	// path is relative to the API root: "repos/o/r/issues/3/events".
	path string
	// query holds the listing's filters; per_page and page are set per
	// request.
	query url.Values
	// etag, when set, makes the first page conditional (If-None-Match).
	etag string
}

// walk fetches w page by page, decoding each page into a fresh []T and
// handing it to visit. It returns the first page's ETag, or notModified when
// that page answered 304 to w.etag — then visit never runs. Only the first
// page is conditional: the rest are fetched only when the first changed.
func walk[T any](
	ctx context.Context, c *Client, w pageWalk, visit func([]T),
) (etag string, notModified bool, err error) {
	query := url.Values{}
	for k, vs := range w.query {
		query[k] = append([]string(nil), vs...)
	}
	query.Set("per_page", strconv.Itoa(listPageSize))
	page := 1
	for range walkPageCap {
		query.Set("page", strconv.Itoa(page))
		var opts []github.RequestOption
		if page == 1 && w.etag != "" {
			opts = append(opts, func(r *http.Request) { r.Header.Set("If-None-Match", w.etag) })
		}
		req, err := c.gh.NewRequest(ctx, http.MethodGet, w.path+"?"+query.Encode(), nil, opts...)
		if err != nil {
			return "", false, err
		}
		var items []T
		resp, err := c.gh.Do(req, &items)
		// go-github reports a 304 as an ErrorResponse, so it is recognised
		// by status before the error is.
		if page == 1 && w.etag != "" && resp != nil && resp.StatusCode == http.StatusNotModified {
			if tag := resp.Header.Get("ETag"); tag != "" {
				return tag, true, nil
			}
			return w.etag, true, nil
		}
		if err != nil {
			return "", false, err
		}
		if page == 1 {
			etag = resp.Header.Get("ETag")
		}
		visit(items)
		if resp.NextPage == 0 {
			return etag, false, nil
		}
		page = resp.NextPage
	}
	return "", false, fmt.Errorf("listing %s runs past %d pages", w.path, walkPageCap)
}

// repoPath is the API path of repo, its owner and name path-escaped.
func repoPath(repo Repo) string {
	return "repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name)
}

// wireUser is an account as the REST API renders it.
type wireUser struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

// actor maps the account onto an Actor; a missing account is the zero one.
func (u *wireUser) actor() Actor {
	if u == nil {
		return Actor{}
	}
	return Actor{Login: u.Login, ID: u.ID, Type: u.Type}
}

// wireApp is the performed_via_github_app object: the App that did it.
type wireApp struct {
	Slug string `json:"slug"`
}

// slug returns the App's slug, "" when GitHub reported none.
func (a *wireApp) slug() string {
	if a == nil {
		return ""
	}
	return a.Slug
}

// wireTime is the RFC 3339 form GitHub timestamps use.
func wireTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }
