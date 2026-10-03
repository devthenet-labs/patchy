// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// pushWatch is the fake GitHub as the run reconciler sees it, recording any
// intent branch fast-forwarded while the pull request from it is not open.
type pushWatch struct {
	*fakeGitHub
	violations []string
}

func (w *pushWatch) FastForwardRef(ctx context.Context, repoURL, branch, sha string) error {
	w.mu.Lock()
	for n, pr := range w.prs {
		if pr.in(repoURL) && pr.head == branch && pr.pr.State != "open" {
			w.violations = append(w.violations, fmt.Sprintf("%s moved to %s while pull request %d is %s",
				repoSlug(repoURL), sha, n, pr.pr.State))
		}
	}
	w.mu.Unlock()
	return w.fakeGitHub.FastForwardRef(ctx, repoURL, branch, sha)
}

// reviewsProperty is one seeded history's world: the env, its watch, the
// reviews submitted (by pull request) and which revise runs fail.
type reviewsProperty struct {
	t     *testing.T
	seed  int64
	rng   *rand.Rand
	e     *env
	watch *pushWatch
	// reviews are the ids of the reviews submitted on each pull request.
	reviews map[int64][]int64
	fails   map[string]bool
	nextID  int64
	// drainedOpen: the drain found the intent in review, and checked the
	// reviews on the pull requests still open.
	drainedOpen bool
}

// TestMultiPRRoundsInvariantsSeeded is a seeded property over random review
// histories of a two-repository intent in review: reviews arriving on either
// pull request, either one merging or closing at any time, rounds that fail,
// random interleavings of the reconcilers, failed status writes (so a round
// is leased and its Revising write lost) and restarts. At every step: at most
// one round is in flight; each round's runs are in one repository; no branch
// is fast-forwarded while its pull request is not open. Then, with the
// failures stopped: no round is left waiting on another (the intent is not
// stuck), every review on a pull request still open has been consumed by a
// round in its own repository, and once every pull request has settled the
// intent ends, Merged exactly when every one merged, its round notices all
// delivered.
func TestMultiPRRoundsInvariantsSeeded(t *testing.T) {
	var seen coverage
	for seed := range int64(12) {
		w := newReviewsProperty(t, seed)
		name := w.e.inReviewLinked()
		for step := range 700 {
			w.step(name)
			w.check(name, step)
			if terminal(w.e.get(name).Status.Phase) {
				break
			}
		}
		w.drain(name)
		seen.add(w, name)
	}
	t.Logf("coverage: %+v", seen)
	// The histories reach what the property is about: rounds in both
	// repositories, a round whose pull request ended under it, failed status
	// writes, reviews checked on pull requests still open, and both endings.
	if seen.rounds[appRepoURL] == 0 || seen.rounds[webRepoURL] == 0 || seen.endedUnder == 0 ||
		seen.lostWrites == 0 || seen.drainedOpen == 0 || seen.merged == 0 || seen.closed == 0 {
		t.Errorf("the seeded histories miss a case: %+v", seen)
	}
}

// coverage tallies what the seeded histories reached.
type coverage struct {
	rounds                              map[string]int
	endedUnder, lostWrites, drainedOpen int
	merged, closed, failures            int
}

func (c *coverage) add(w *reviewsProperty, name string) {
	if c.rounds == nil {
		c.rounds = map[string]int{}
	}
	for _, r := range w.e.runsOf(name, v1alpha1.IntentStageRevise) {
		if r.Spec.Attempt == 1 {
			c.rounds[r.Spec.Repository.URL]++
		}
		if strings.Contains(r.Status.Detail, "was merged or closed before the round") {
			c.endedUnder++
		}
	}
	c.lostWrites += w.e.failed
	if w.drainedOpen {
		c.drainedOpen++
	}
	switch w.e.get(name).Status.Phase {
	case v1alpha1.IntentMerged:
		c.merged++
	case v1alpha1.IntentClosed:
		c.closed++
	}
	for _, f := range w.fails {
		if f {
			c.failures++
		}
	}
}

func newReviewsProperty(t *testing.T, seed int64) *reviewsProperty {
	p := testMultiProject()
	p.Spec.Limits.MaxRevisions = new(int32(50))
	e := newEnv(t, p)
	w := &reviewsProperty{t: t, seed: seed, rng: rand.New(rand.NewSource(0x5e33 + seed)), e: e,
		reviews: map[int64][]int64{}, fails: map[string]bool{}, nextID: 5000}
	w.wire()
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "build" && w.fails[spec.Finding] {
			return jobs.RunOutput{Events: []envelope.Event{{V: envelope.Version, Type: envelope.TypeRemediation,
				Remediation: &envelope.Remediation{Stage: envelope.Stage{Outcome: envelope.OutcomeRuntimeError,
					Detail: "the CLI crashed"}}}}}
		}
		return multiOutput(spec)
	}
	return w
}

// wire sets the flag and the watch on the reconcilers, again after a
// restart replaced them.
func (w *reviewsProperty) wire() {
	w.e.multiRepo(true)
	w.e.runs.MaxConcurrent = 1 + w.rng.Intn(2)
	if w.watch == nil {
		w.watch = &pushWatch{fakeGitHub: w.e.gh}
	}
	w.e.runs.GitHub = w.watch
}

// openPRs are the numbers of the pull requests open on GitHub.
func (w *reviewsProperty) openPRs() []int64 {
	w.e.gh.mu.Lock()
	defer w.e.gh.mu.Unlock()
	var out []int64
	for n, pr := range w.e.gh.prs {
		if pr.pr.State == "open" {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// step takes one random action.
func (w *reviewsProperty) step(name string) {
	e, rng, ctx := w.e, w.rng, context.Background()
	for _, r := range e.runsOf(name, v1alpha1.IntentStageRevise) {
		if _, ok := w.fails[r.Name]; !ok {
			w.fails[r.Name] = rng.Intn(4) == 0
		}
	}
	e.failEvery = 0
	if rng.Intn(5) == 0 {
		e.failEvery = 2 + rng.Intn(3)
	}
	defer func() { e.failEvery = 0 }()
	switch n := rng.Intn(14); {
	case n < 3:
		_ = e.reconcileIntent(name)
	case n < 4:
		e.readyRepositories(repoImage)
	case n < 5:
		_, _ = e.runs.Reconcile(ctx, req(runSchedulerRequest))
	case n < 8:
		if runs := e.intentRuns(name); len(runs) > 0 {
			_, _ = e.runs.Reconcile(ctx, req(runs[rng.Intn(len(runs))].Name))
		}
	case n < 10:
		e.clock.Advance(time.Duration(1+rng.Intn(90)) * time.Second)
	case n < 12:
		if open := w.openPRs(); len(open) > 0 && len(w.reviews[1])+len(w.reviews[2]) < 8 {
			pr := open[rng.Intn(len(open))]
			w.nextID++
			e.reviewOn(pr, w.nextID, fmt.Sprintf("Change %d, please.", w.nextID))
			w.reviews[pr] = append(w.reviews[pr], w.nextID)
		}
	case n < 13:
		if rng.Intn(3) == 0 {
			e.restart()
			w.wire()
		}
	default:
		// Odd seeds keep their pull requests open until the drain, which
		// then checks every review on them was consumed.
		if open := w.openPRs(); len(open) > 0 && w.seed%2 == 0 && rng.Intn(12) == 0 {
			e.gh.closePRn(open[rng.Intn(len(open))], rng.Intn(2) == 0)
		}
	}
}

// check holds the invariants of every step.
func (w *reviewsProperty) check(name string, step int) {
	w.t.Helper()
	inFlight := map[int32]bool{}
	repoOf := map[int32]string{}
	for _, r := range w.e.runsOf(name, v1alpha1.IntentStageRevise) {
		if repo, ok := repoOf[r.Spec.Round]; ok && !sameRepo(repo, r.Spec.Repository.URL) {
			w.t.Fatalf("seed %d step %d: round %d has runs in %s and %s", w.seed, step, r.Spec.Round, repo,
				r.Spec.Repository.URL)
		}
		repoOf[r.Spec.Round] = r.Spec.Repository.URL
		if r.Status.Phase != v1alpha1.RunComplete && r.Status.Phase != v1alpha1.RunFailed {
			inFlight[r.Spec.Round] = true
		}
	}
	if len(inFlight) > 1 {
		w.t.Fatalf("seed %d step %d: rounds %v in flight at once", w.seed, step, inFlight)
	}
	if len(w.watch.violations) > 0 {
		w.t.Fatalf("seed %d step %d: %v", w.seed, step, w.watch.violations)
	}
}

// drain stops the failures and drives the intent on its own: first with
// whatever pull requests are open left open, then with every one settled.
func (w *reviewsProperty) drain(name string) {
	w.t.Helper()
	e := w.e
	e.restart()
	w.wire()
	w.cycles(name, 80)
	in := e.get(name)
	if !terminal(in.Status.Phase) {
		if in.Status.Phase != v1alpha1.IntentInReview {
			w.t.Fatalf("seed %d: the intent is stuck in %s: %+v, rounds %d", w.seed, in.Status.Phase,
				in.Status.Conditions, in.Status.Rounds)
		}
		w.consumed(name, in)
		w.drainedOpen = true
		for _, n := range w.openPRs() {
			e.gh.closePRn(n, w.rng.Intn(3) > 0)
		}
		w.cycles(name, 40)
	}
	w.ended(name)
}

// cycles runs n rounds of every reconciler, a minute apart.
func (w *reviewsProperty) cycles(name string, n int) {
	for range n {
		w.e.mustIntent(name)
		w.e.readyRepositories(repoImage)
		w.e.runRuns()
		w.e.clock.Advance(time.Minute)
	}
}

// consumed checks every review on a pull request still open was taken by a
// round in the pull request's own repository.
func (w *reviewsProperty) consumed(name string, in *v1alpha1.Intent) {
	w.t.Helper()
	for _, n := range w.openPRs() {
		repo := in.Status.PullRequests[n-1].Repository
		var taken []int64
		for _, r := range w.e.revisesIn(name, repo) {
			taken = append(taken, r.Spec.Inputs.ReviewIDs...)
		}
		for _, id := range w.reviews[n] {
			if !slices.Contains(taken, id) {
				w.t.Fatalf("seed %d: review %d on pull request %d (%s) was never consumed; rounds took %v",
					w.seed, id, n, repoSlug(repo), taken)
			}
		}
	}
}

// ended checks the ending once every pull request has settled.
func (w *reviewsProperty) ended(name string) {
	w.t.Helper()
	in := w.e.get(name)
	allMerged := true
	for _, pr := range in.Status.PullRequests {
		allMerged = allMerged && pr.State == prMerged
	}
	switch {
	case !terminal(in.Status.Phase):
		w.t.Fatalf("seed %d: every pull request settled and the intent is %s", w.seed, in.Status.Phase)
	case allMerged && in.Status.Phase != v1alpha1.IntentMerged,
		!allMerged && in.Status.Phase != v1alpha1.IntentClosed:
		w.t.Fatalf("seed %d: pull requests %+v ended %s", w.seed, in.Status.PullRequests, in.Status.Phase)
	case in.Status.RoundNoticesThrough != in.Status.Rounds:
		w.t.Fatalf("seed %d: %d of %d round notices delivered", w.seed, in.Status.RoundNoticesThrough,
			in.Status.Rounds)
	}
	if !allMerged && len(w.e.gh.withMarker(" partial ")) != 1 {
		w.t.Fatalf("seed %d: partial notices = %d, want one", w.seed, len(w.e.gh.withMarker(" partial ")))
	}
}
