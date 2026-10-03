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

// roundNotices are patchy's round notices on pull request n, by round.
func (e *env) roundNotices(name string, n int64) map[int32]string {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	out := map[int32]string{}
	for _, c := range e.gh.prComments[n] {
		var round int32
		if _, err := fmt.Sscanf(markerOf(c.Body), "<!-- patchy:intent-pr-round:"+name+":%d -->", &round); err == nil {
			out[round] = c.Body
		}
	}
	return out
}

// statusBody is the body of patchy's status comment on the intent issue.
func (e *env) statusBody() string {
	st := e.gh.withMarker("patchy:intent ")
	if len(st) != 1 {
		return ""
	}
	return st[0].Body
}

// TestCIFixRoundIsLabelledAndCountedApart is the second live regression of
// the 2026-10-03 CI-fix demo: a round a failed check started said "Revision
// round" on its pull request, the issue's status said patchy was revising
// "from review feedback", and the summary left it out. A CI-fix round now says
// so, naming the check, everywhere; a review round still reads as before; and
// the summary counts the two apart.
func TestCIFixRoundIsLabelledAndCountedApart(t *testing.T) {
	project := testProject()
	project.Spec.Checks.Fix = []string{"changelog"}
	e := newEnv(t, project)
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	pr := in.Status.PullRequests[0]
	e.gh.checks[pr.HeadSHA] = []ghclient.CheckRun{{ID: 61, Name: "changelog", HeadSHA: pr.HeadSHA,
		Status: "completed", Conclusion: "failure", Output: ghclient.CheckOutput{Title: "no entry for #1"}}}
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.passUntil(name, func() bool { return strings.Contains(e.statusBody(), "fixing a pull request's failed checks") })

	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	fix := e.runsOf(name, v1alpha1.IntentStageRevise)[0]
	want := fmt.Sprintf("CI-fix round for `changelog` pushed commit `%s`. Review is requested again.",
		fix.Status.PushedCommit)
	if got := e.roundNotices(name, pr.Number)[1]; !strings.Contains(got, want) || strings.Contains(got, "Revision") {
		t.Errorf("CI-fix round notice = %q, want %q", got, want)
	}
	e.gh.checks[in.Status.PullRequests[0].HeadSHA] = []ghclient.CheckRun{{ID: 62, Name: "changelog",
		HeadSHA: in.Status.PullRequests[0].HeadSHA, Status: "completed", Conclusion: "success"}}

	e.gh.reviews[pr.Number] = []ghclient.Review{{ID: 63, NodeID: "review-63", Author: actorOf(approver),
		State: "CHANGES_REQUESTED", Body: "Mention the endpoint too.", SubmittedAt: e.clock.Now()}}
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	review := e.runsOf(name, v1alpha1.IntentStageRevise)[1]
	if got := e.roundNotices(name, pr.Number)[2]; !strings.Contains(got,
		"Revision round pushed commit `"+review.Status.PushedCommit+"`.") {
		t.Errorf("review round notice = %q, want it unchanged", got)
	}
	if in.Status.CheckFixes != 1 || in.Status.Revisions != 1 {
		t.Fatalf("counters = check fixes %d, revisions %d; want 1 and 1", in.Status.CheckFixes, in.Status.Revisions)
	}

	e.gh.closePR(true)
	e.drive(name, v1alpha1.IntentMerged, repoImage)
	summary := e.gh.withMarker(" summary ")
	if len(summary) != 1 || !strings.Contains(summary[0].Body, "**Revisions:** 1") ||
		!strings.Contains(summary[0].Body, "**CI-fix rounds:** 1") {
		t.Errorf("summary = %+v, want one revision and one CI-fix round counted apart", summary)
	}
}
