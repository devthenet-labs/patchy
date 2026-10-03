// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// webAsks is GitHub counting the calls made on the web repository: the
// departed read's rate check and pull request read, each of which asks for a
// token.
type webAsks struct {
	GitHub
	n int
}

func (w *webAsks) RateRemaining(ctx context.Context, repoURL string) (int, error) {
	if sameRepo(repoURL, webRepoURL) {
		w.n++
	}
	return w.GitHub.RateRemaining(ctx, repoURL)
}

func (w *webAsks) GetPullRequest(ctx context.Context, repoURL string, number int64) (*ghclient.PullRequest,
	error) {
	if sameRepo(repoURL, webRepoURL) {
		w.n++
	}
	return w.GitHub.GetPullRequest(ctx, repoURL, number)
}

// flooredWeb is GitHub with the web repository's installation under every
// rate floor.
type flooredWeb struct{ GitHub }

func (f flooredWeb) RateRemaining(ctx context.Context, repoURL string) (int, error) {
	if sameRepo(repoURL, webRepoURL) {
		return 0, nil
	}
	return f.GitHub.RateRemaining(ctx, repoURL)
}

// TestUnreachableDepartedRepositoryIsAskedOnlyNowAndThen is the round-4
// regression of the read of a pull request whose repository left the
// Project, which is outside what the Project's Ready proved. Once out of
// reach, it was asked again on every pull request poll, each time asking for
// a token for a repository no Project vouches for. Now one found out of reach
// is not asked again for departedRetry; then it is asked once more.
func TestUnreachableDepartedRepositoryIsAskedOnlyNowAndThen(t *testing.T) {
	web := strings.ToLower(webSlug)
	tests := []struct {
		name string
		errs map[string]error
	}{
		{name: "no Forge covers it", errs: map[string]error{
			"* " + web: fmt.Errorf("resolve %s: %w", webRepoURL, forge.ErrNoMatch)}},
		{name: "the installation refuses it", errs: map[string]error{
			"RateRemaining " + web: ghError(http.StatusUnprocessableEntity, "not accessible to the installation")}},
		{name: "GitHub will not show it", errs: map[string]error{
			"GetPullRequest " + web: ghError(http.StatusNotFound, "Not Found")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newMultiEnv(t)
			name := e.inReviewLinked()
			e.dropRepository(webRepoURL)
			e.setRepoErrs(tt.errs)
			asks := &webAsks{GitHub: e.gh}
			e.intent.GitHub = asks

			e.clock.Advance(time.Minute)
			e.mustIntent(name)
			first := asks.n
			if first == 0 {
				t.Fatal("web's pull request was never asked for")
			}
			for range 5 {
				e.clock.Advance(time.Minute)
				e.mustIntent(name)
			}
			if asks.n != first {
				t.Errorf("web asked %d times over 6 polls, want %d: once, until departedRetry has passed", asks.n,
					first)
			}
			e.clock.Advance(departedRetry)
			e.mustIntent(name)
			if asks.n != 2*first {
				t.Errorf("web asked %d times once departedRetry passed, want %d", asks.n, 2*first)
			}
			if in := e.get(name); in.Status.Phase != v1alpha1.IntentInReview || in.Status.PullRequests[1].State != prOpen {
				t.Errorf("phase %s, pull requests %+v; want the intent in review on web's last read state",
					in.Status.Phase, in.Status.PullRequests)
			}
		})
	}
}

// TestStaleDepartedRecordNeverEndsTheIntent is the round-4 regression of a
// departed repository read on a failure that may pass. Both pull requests
// merged, web after web left the Project. The pass that read both merged
// posted the summary and closed the issue as completed, and its Merged write
// was lost. On the next passes web could not be read for a while (its rate
// floor, a server error), and its record, last read open or closed, was taken
// as it stood: closed, it ended the all-merged intent Closed, with a notice
// that web's pull request did not merge and the issue closed as not planned;
// open, the issue patchy itself had closed was taken for a human's, and the
// intent ended Closed. Now the ending waits for web to be read, and the intent
// is Merged once it is.
func TestStaleDepartedRecordNeverEndsTheIntent(t *testing.T) {
	tests := []struct {
		name string
		// webClosed: web's pull request was last read closed unmerged, and
		// reopened since; otherwise it was last read open.
		webClosed bool
		// floor: web's installation is under its rate floor; otherwise its
		// pull request read fails with a server error.
		floor bool
	}{
		{name: "last read closed, server error", webClosed: true},
		{name: "last read closed, rate floor", webClosed: true, floor: true},
		{name: "last read open, server error"},
		{name: "last read open, rate floor", floor: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newMultiEnv(t)
			name := e.inReviewLinked()
			if tt.webClosed {
				e.gh.closePRn(2, false)
				e.driveUntil(name, func(in *v1alpha1.Intent) bool { return in.Status.PullRequests[1].State == prClosed })
				e.gh.mu.Lock()
				e.gh.prs[2].pr.State = prOpen // reopened
				e.gh.mu.Unlock()
			}
			e.dropRepository(webRepoURL)
			e.gh.closePRn(2, true)
			e.gh.closePRn(1, true)
			e.failStatusIf = func(in *v1alpha1.Intent) bool { return in.Status.Phase == v1alpha1.IntentMerged }
			for range 5 {
				if e.failed > 0 {
					break
				}
				e.clock.Advance(time.Minute)
				_ = e.reconcileIntent(name)
			}
			if e.failed == 0 || e.gh.state() != "closed" {
				t.Fatalf("Merged write lost %v, issue %s; want the issue closed and the Merged write lost",
					e.failed > 0, e.gh.state())
			}

			if tt.floor {
				e.intent.GitHub = flooredWeb{GitHub: e.gh}
			} else {
				e.setRepoErrs(map[string]error{
					"GetPullRequest " + strings.ToLower(webSlug): ghError(http.StatusBadGateway, "Bad Gateway")})
			}
			for range 5 {
				e.clock.Advance(time.Minute)
				if err := e.reconcileIntent(name); err != nil {
					t.Fatalf("pass while web cannot be read: %v", err)
				}
			}
			if in := e.get(name); in.Status.Phase != v1alpha1.IntentInReview ||
				len(e.gh.withMarker(templates.PartialKey)) != 0 || slices.Contains(e.gh.closes[1], ghclient.CloseNotPlanned) {
				t.Fatalf("phase %s, %d partial notices, issue closes %v while web cannot be read; want the ending "+
					"waiting for it", in.Status.Phase, len(e.gh.withMarker(templates.PartialKey)), e.gh.closes[1])
			}

			e.intent.GitHub = e.gh
			e.gh.mu.Lock()
			clear(e.gh.repoErrs)
			e.gh.mu.Unlock()
			for range 5 {
				if e.get(name).Status.Phase == v1alpha1.IntentMerged {
					break
				}
				e.clock.Advance(time.Minute)
				e.mustIntent(name)
			}
			if in := e.get(name); in.Status.Phase != v1alpha1.IntentMerged ||
				in.Status.PullRequests[0].State != prMerged || in.Status.PullRequests[1].State != prMerged {
				t.Fatalf("phase %s, pull requests %+v once web is read; want both merged and the intent Merged",
					in.Status.Phase, in.Status.PullRequests)
			}
			if got := len(e.gh.withMarker(templates.SummaryKey)); got != 1 ||
				slices.Contains(e.gh.closes[1], ghclient.CloseNotPlanned) {
				t.Errorf("%d summaries, issue closes %v; want one summary, the issue closed as completed only", got,
					e.gh.closes[1])
			}
		})
	}
}
