// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// statusSays runs passes until the status comment contains want, and
// reports the comment it ended with when it never does.
func (e *env) statusSays(name, want string) {
	e.t.Helper()
	for range 10 {
		if strings.Contains(e.statusBody(), want) {
			return
		}
		e.mustIntent(name)
		e.clock.Advance(time.Second)
	}
	e.t.Errorf("status comment = %q, want it to contain %q", e.statusBody(), want)
}

// requestChanges submits an approver's review asking for changes on pull
// request n, and lets its quiet period pass.
func (e *env) requestChanges(n, id int64, body string) {
	e.reviewOn(n, id, body)
	e.clock.Advance(3 * time.Minute)
}

// TestStatusCommentCountsRevisionsAgainstTheLimit is the regression for a
// status comment that never said how much of the revision allowance an
// intent had used: the template had the line, and the controller never
// filled it in. Once the intent has a pull request, the line counts its
// review rounds against the Project's maxRevisions (the schema default when
// unset) as the limit counts them, so a round that failed counts too, and a
// CI-fix round, counted against its own limit, is not a revision.
func TestStatusCommentCountsRevisionsAgainstTheLimit(t *testing.T) {
	tests := []struct {
		name  string
		limit *int32
		want  int32
	}{
		{"schema default", nil, v1alpha1.DefaultMaxRevisions},
		{"project limit", new(int32(5)), 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project := testProject()
			project.Spec.Limits.MaxRevisions = tt.limit
			project.Spec.Checks.Fix = []string{"changelog"}
			e := newEnv(t, project)
			name := e.awaiting()
			if body := e.statusBody(); body == "" || strings.Contains(body, "**Revisions:**") {
				t.Errorf("status comment awaiting approval = %q, want one without a revisions line", body)
			}
			line := func(n int) string { return fmt.Sprintf("**Revisions:** %d of %d\n", n, tt.want) }

			e.gh.label(1, "patchy:approved", approver)
			pr := e.drive(name, v1alpha1.IntentInReview, repoImage).Status.PullRequests[0]
			e.statusSays(name, line(0))

			// Every attempt of the first round fails: the round still spends
			// the allowance, though it revised nothing.
			e.jobs.output = failingBuild
			e.requestChanges(pr.Number, 71, "Mention the endpoint too.")
			e.drive(name, v1alpha1.IntentRevising, repoImage)
			in := e.drive(name, v1alpha1.IntentInReview, repoImage)
			if in.Status.Revisions != 0 {
				t.Fatalf("revisions = %d after a failed round, want 0 completed", in.Status.Revisions)
			}
			e.statusSays(name, line(1))

			e.jobs.output = defaultOutput
			e.requestChanges(pr.Number, 72, "And say so in the README.")
			e.drive(name, v1alpha1.IntentRevising, repoImage)
			in = e.drive(name, v1alpha1.IntentInReview, repoImage)
			e.statusSays(name, line(2))

			head := in.Status.PullRequests[0].HeadSHA
			e.gh.checks[head] = []ghclient.CheckRun{{ID: 73, Name: "changelog", HeadSHA: head,
				Status: "completed", Conclusion: "failure", Output: ghclient.CheckOutput{Title: "no entry for #1"}}}
			e.drive(name, v1alpha1.IntentRevising, repoImage)
			if in = e.drive(name, v1alpha1.IntentInReview, repoImage); in.Status.CheckFixes != 1 {
				t.Fatalf("check fixes = %d, want the CI-fix round completed", in.Status.CheckFixes)
			}
			e.statusSays(name, line(2))
		})
	}
}

// TestEndingCountsRoundsAsTheLimitsDo: the summary and the partial notice
// count revision and CI-fix rounds as the limits, the status comment and the
// dashboard do, a failed round included. Regression: they counted completed
// rounds only, so an intent whose every round failed ended saying
// "Revisions: 0" and no CI-fix round, under a status comment counting them.
func TestEndingCountsRoundsAsTheLimitsDo(t *testing.T) {
	t.Run("summary", func(t *testing.T) {
		project := testProject()
		project.Spec.Checks.Fix = []string{"changelog"}
		e := newEnv(t, project)
		name := e.awaiting()
		e.gh.label(1, "patchy:approved", approver)
		pr := e.drive(name, v1alpha1.IntentInReview, repoImage).Status.PullRequests[0]

		e.jobs.output = failingBuild
		e.requestChanges(pr.Number, 81, "Mention the endpoint too.")
		e.drive(name, v1alpha1.IntentRevising, repoImage)
		e.drive(name, v1alpha1.IntentInReview, repoImage)
		e.gh.checks[pr.HeadSHA] = []ghclient.CheckRun{{ID: 82, Name: "changelog", HeadSHA: pr.HeadSHA,
			Status: "completed", Conclusion: "failure", Output: ghclient.CheckOutput{Title: "no entry for #1"}}}
		e.drive(name, v1alpha1.IntentRevising, repoImage)
		in := e.drive(name, v1alpha1.IntentInReview, repoImage)
		if in.Status.Revisions != 0 || in.Status.CheckFixes != 0 || in.Status.Rounds != 2 {
			t.Fatalf("rounds %d, revisions %d, check fixes %d; want two rounds, neither completed",
				in.Status.Rounds, in.Status.Revisions, in.Status.CheckFixes)
		}

		e.gh.closePR(true)
		e.drive(name, v1alpha1.IntentMerged, repoImage)
		summary := e.gh.withMarker(" summary ")
		if len(summary) != 1 {
			t.Fatalf("summaries = %d, want one", len(summary))
		}
		if body := summary[0].Body; !strings.Contains(body, "**Revisions:** 1\n") ||
			!strings.Contains(body, "**CI-fix rounds:** 1\n") {
			t.Errorf("summary = %q, want one revision and one CI-fix round, counted apart", body)
		}
	})
	t.Run("partial notice", func(t *testing.T) {
		e := newMultiEnv(t)
		name := e.inReviewLinked()
		e.jobs.output = failingBuild
		e.requestChanges(2, 91, "Web: put it in the footer.")
		e.drive(name, v1alpha1.IntentRevising, repoImage)
		if in := e.drive(name, v1alpha1.IntentInReview, repoImage); in.Status.Revisions != 0 {
			t.Fatalf("revisions = %d after a failed round, want 0 completed", in.Status.Revisions)
		}

		e.gh.closePRn(1, true)
		e.gh.closePRn(2, false)
		e.drive(name, v1alpha1.IntentClosed, repoImage)
		notice := e.gh.withMarker(" partial ")
		if len(notice) != 1 {
			t.Fatalf("partial notices = %d, want one", len(notice))
		}
		if body := notice[0].Body; !strings.Contains(body, "**Revisions:** 1\n") {
			t.Errorf("partial notice = %q, want it to count the failed revision round", body)
		}
	})
}
