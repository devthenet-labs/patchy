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
// request n, read once its quiet period has passed.
func (e *env) requestChanges(n, id int64, body string) {
	e.gh.reviews[n] = append(e.gh.reviews[n], ghclient.Review{ID: id, NodeID: fmt.Sprintf("review-%d", id),
		Author: actorOf(approver), State: "CHANGES_REQUESTED", Body: body, SubmittedAt: e.clock.Now()})
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
	five := int32(5)
	tests := []struct {
		name  string
		limit *int32
		want  int32
	}{
		{"schema default", nil, v1alpha1.DefaultMaxRevisions},
		{"project limit", &five, 5},
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
