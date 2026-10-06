// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// newIntentFor is newIntent for issue n: the Intent discovery would create
// for it, requested by requester, with the fake issue open and labelled.
func (e *env) newIntentFor(n int64, requester string) string {
	e.t.Helper()
	e.gh.openIssue(n, fmt.Sprintf("Request %d", n), "Please change something.", requester)
	ev := e.gh.issues[n].events[len(e.gh.issues[n].events)-1]
	name := v1alpha1.IntentName("target", n)
	in := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: v1alpha1.IntentSpec{
			Project: "target",
			Issue: v1alpha1.IntentIssue{Repository: intentRepoURL, Number: n,
				URL: fmt.Sprintf("%s/issues/%d", intentRepoURL, n)},
			RequestedBy: v1alpha1.IntentRequest{Login: requester, At: metav1.NewTime(ev.CreatedAt), EventID: ev.ID},
		},
	}
	if err := e.c.Create(context.Background(), in); err != nil {
		e.t.Fatal(err)
	}
	e.clock.Advance(time.Second)
	return name
}

// seedIntent stores an Intent of project in phase, recording prs, as
// another intent's reconciler would have left it; each is created a second
// after the last, so they are in creation order.
func (e *env) seedIntent(name, project string, phase v1alpha1.IntentPhase, prs ...v1alpha1.IntentPullRequest) {
	e.t.Helper()
	ctx := context.Background()
	in := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: v1alpha1.IntentSpec{
			Project: project,
			Issue:   v1alpha1.IntentIssue{Repository: intentRepoURL, Number: 900, URL: intentRepoURL + "/issues/900"},
		},
	}
	if err := e.c.Create(ctx, in); err != nil {
		e.t.Fatal(err)
	}
	in.Status.Phase, in.Status.PullRequests = phase, prs
	if err := e.c.Status().Update(ctx, in); err != nil {
		e.t.Fatal(err)
	}
	e.clock.Advance(time.Second)
}

// seedPR puts pull request n in repo ("owner/name") on the fake GitHub, in
// state, titled title, changing changed files of which files are listed.
func (e *env) seedPR(n int64, repo, state, title string, changed int, files ...ghclient.PullRequestFile) {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	e.gh.prs[n] = &fakePR{repo: repo, url: fmt.Sprintf("https://github.com/%s/pull/%d", repo, n),
		pr: ghclient.PullRequest{Number: int(n), State: state, Title: title, ChangedFiles: changed}, files: files}
}

// openPR is the record an intent keeps of its pull request n, open in repoURL.
func openPR(repoURL string, n int64) v1alpha1.IntentPullRequest {
	return v1alpha1.IntentPullRequest{Repository: repoURL, Number: n, URL: fmt.Sprintf("%s/pull/%d", repoURL, n),
		State: prOpen}
}

// openPRsPass is a pass of the intent reconciler over own, as the plan run's
// input is created in, logging to the returned buffer.
func (e *env) openPRsPass(own string, proj *v1alpha1.Project) (*pass, *bytes.Buffer) {
	var logs bytes.Buffer
	e.intent.Log = slog.New(slog.NewTextHandler(&logs, nil))
	return &pass{r: e.intent, set: testSettings(), in: e.get(own), proj: proj}, &logs
}

// decodeOpen decodes the list a plan run is handed, failing on an error.
func decodeOpen(t *testing.T, s string) []templates.OpenPullRequest {
	t.Helper()
	prs, err := templates.DecodeOpenPullRequests(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return prs
}

// TestOtherOpenPullRequests: a plan is told of the pull requests the other
// intents of its own Project record open in one of the Project's
// repositories, each read from GitHub as it is now, the longest-open
// intent's first. Its own, an ended intent's, another Project's, one merged
// or in a repository the Project does not list are never asked about; one
// GitHub reports closed, or no longer has, is left out.
func TestOtherOpenPullRequests(t *testing.T) {
	proj := testMultiProject()
	e := newEnv(t, proj)
	own := e.newIntent(approver)
	in := e.get(own)
	in.Status.PullRequests = []v1alpha1.IntentPullRequest{openPR(appRepoURL, 1)}
	if err := e.c.Status().Update(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	const legacyRepoURL = "https://github.com/acme/legacy"
	e.seedIntent("target-2", "target", v1alpha1.IntentInReview, openPR(appRepoURL, 2))
	e.seedIntent("target-3", "target", v1alpha1.IntentMerged, openPR(appRepoURL, 3))
	e.seedIntent("other-4", "other", v1alpha1.IntentInReview, openPR(appRepoURL, 4))
	merged := openPR(appRepoURL, 6)
	merged.State = "merged"
	// Recorded in another spelling than the Project's.
	e.seedIntent("target-5", "target", v1alpha1.IntentRevising, v1alpha1.IntentPullRequest{
		Repository: "https://github.com/ACME/acme.web_app/", Number: 5,
		URL: "https://github.com/acme/Acme.Web_App/pull/5", State: prOpen}, merged)
	e.seedIntent("target-7", "target", v1alpha1.IntentInReview, openPR(legacyRepoURL, 7))
	e.seedIntent("target-8", "target", v1alpha1.IntentBlocked, openPR(appRepoURL, 8), openPR(appRepoURL, 9))

	e.seedPR(1, "acme/app", "open", "own", 1, ghclient.PullRequestFile{Path: "own.go"})
	e.seedPR(2, "acme/app", "open", "target: Add a health endpoint", 3,
		ghclient.PullRequestFile{Path: "src/server.ts"}, ghclient.PullRequestFile{Path: "src/health.ts"},
		ghclient.PullRequestFile{Path: "test/health.test.ts", PreviousPath: "test/ping.test.ts"})
	e.seedPR(3, "acme/app", "open", "ended", 1, ghclient.PullRequestFile{Path: "ended.go"})
	e.seedPR(4, "acme/app", "open", "another project's", 1, ghclient.PullRequestFile{Path: "other.go"})
	e.seedPR(5, "acme/Acme.Web_App", "open", "target: Restyle the header", 1,
		ghclient.PullRequestFile{Path: "web/header.css"})
	e.seedPR(6, "acme/app", "open", "merged", 1, ghclient.PullRequestFile{Path: "merged.go"})
	e.seedPR(7, "acme/legacy", "open", "departed", 1, ghclient.PullRequestFile{Path: "legacy.go"})
	e.seedPR(8, "acme/app", "closed", "closed since", 1, ghclient.PullRequestFile{Path: "closed.go"})

	p, logs := e.openPRsPass(own, proj)
	got := decodeOpen(t, p.otherOpenPullRequests(context.Background()))
	want := []templates.OpenPullRequest{
		{Intent: "target-2", Repository: appRepoURL, Number: 2, URL: appRepoURL + "/pull/2",
			Title: "target: Add a health endpoint", ChangedFiles: 3, Files: []templates.OpenPullRequestFile{
				{Path: "src/server.ts"}, {Path: "src/health.ts"},
				{Path: "test/health.test.ts", From: "test/ping.test.ts"},
			}},
		{Intent: "target-5", Repository: webRepoURL, Number: 5, URL: webRepoURL + "/pull/5",
			Title: "target: Restyle the header", ChangedFiles: 1,
			Files: []templates.OpenPullRequestFile{{Path: "web/header.css"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("open pull requests =\n%+v\nwant\n%+v", got, want)
	}
	// Asked about: 2, 5, 8 (closed) and 9 (gone); listed: 2 and 5.
	if g, l := e.gh.calls["GetPullRequest"], e.gh.calls["ListPullRequestFiles"]; g != 4 || l != 2 {
		t.Errorf("GitHub asked %d pull requests and listed %d files, want 4 and 2", g, l)
	}
	if logs.Len() != 0 {
		t.Errorf("a closed or missing pull request was logged as a failure:\n%s", logs.String())
	}
}

// TestOtherOpenPullRequestsBounds: at most templates.OpenPullRequestsMax pull
// requests are read, the longest-open intents' first, and each lists at most
// templates.OpenPullRequestMaxFiles files, the rest counted.
func TestOtherOpenPullRequestsBounds(t *testing.T) {
	e := newEnv(t, testProject())
	own := e.newIntent(approver)
	files := make([]ghclient.PullRequestFile, 70)
	for i := range files {
		files[i].Path = fmt.Sprintf("src/f%02d.go", i)
	}
	for n := int64(2); n <= 8; n++ {
		e.seedIntent(v1alpha1.IntentName("target", n), "target", v1alpha1.IntentInReview, openPR(appRepoURL, n))
		e.seedPR(n, "acme/app", "open", fmt.Sprintf("change %d", n), 120, files...)
	}
	p, _ := e.openPRsPass(own, testProject())
	got := decodeOpen(t, p.otherOpenPullRequests(context.Background()))
	if len(got) != templates.OpenPullRequestsMax {
		t.Fatalf("%d pull requests listed, want %d", len(got), templates.OpenPullRequestsMax)
	}
	for i, pr := range got {
		if want := int64(i + 2); pr.Number != want || pr.Intent != v1alpha1.IntentName("target", want) {
			t.Errorf("listing %d is %s#%d, want the longest-open intents' first (#%d)", i, pr.Intent, pr.Number, want)
		}
		if len(pr.Files) != templates.OpenPullRequestMaxFiles || pr.ChangedFiles != 120 {
			t.Errorf("#%d lists %d of %d files, want %d of 120", pr.Number, len(pr.Files), pr.ChangedFiles,
				templates.OpenPullRequestMaxFiles)
		}
	}
	if g, l := e.gh.calls["GetPullRequest"], e.gh.calls["ListPullRequestFiles"]; g != 5 || l != 5 {
		t.Errorf("GitHub asked %d pull requests and listed %d files, want 5 of each", g, l)
	}
}

// TestOtherOpenPullRequestsFailure: any failure to read a pull request other
// than its absence, the rate budget under its floor included, leaves the
// whole list out, with a log line naming why, rather than failing the plan
// or telling it of only some.
func TestOtherOpenPullRequestsFailure(t *testing.T) {
	for _, tt := range []struct {
		name, reason string
		fail         func(e *env)
	}{
		{"pull request", "read the pull request", func(e *env) {
			e.gh.repoErrs["GetPullRequest acme/app"] = ghError(http.StatusBadGateway, "Bad Gateway")
		}},
		{"files", "list the pull request's files", func(e *env) {
			e.gh.repoErrs["ListPullRequestFiles acme/app"] = ghError(http.StatusForbidden,
				"Resource not accessible by integration")
		}},
		{"rate budget unread", "the rate budget", func(e *env) {
			e.gh.repoErrs["RateRemaining acme/app"] = errTransient
		}},
		{"rate budget under the floor", "the rate budget", func(e *env) { e.gh.remaining = 10 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			own := e.newIntent(approver)
			e.seedIntent("target-2", "target", v1alpha1.IntentInReview, openPR(appRepoURL, 2))
			e.seedIntent("target-3", "target", v1alpha1.IntentInReview, openPR(appRepoURL, 3))
			e.seedPR(2, "acme/app", "open", "two", 1, ghclient.PullRequestFile{Path: "a.go"})
			e.seedPR(3, "acme/app", "open", "three", 1, ghclient.PullRequestFile{Path: "b.go"})
			tt.fail(e)
			p, logs := e.openPRsPass(own, testProject())
			if got := p.otherOpenPullRequests(context.Background()); got != "" {
				t.Errorf("open pull requests = %s, want none after a failure", got)
			}
			if !strings.Contains(logs.String(), "the plan is made without them") ||
				!strings.Contains(logs.String(), `reason="`+tt.reason+`"`) {
				t.Errorf("log = %q, want the omission and its reason %q", logs.String(), tt.reason)
			}
		})
	}
}

// TestPlanIsToldOfOtherIntentsOpenPullRequests drives two intents of one
// Project: the first plans with nothing to be told of and opens its pull
// request; the second's plan run records that pull request in its input,
// beside the request and outside its digest, and its plan Job is handed it.
func TestPlanIsToldOfOtherIntentsOpenPullRequests(t *testing.T) {
	e := newEnv(t, testProject())
	first := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	pr := e.drive(first, v1alpha1.IntentInReview, repoImage).Status.PullRequests[0]
	firstPlan := e.planRunOf(first)
	if spec := e.specOf(firstPlan); spec.OpenPullRequests != "" {
		t.Errorf("the first plan was handed open pull requests: %s", spec.OpenPullRequests)
	}
	if cm := e.runInputOf(firstPlan); cm.Data[keyOpenPullRequests] != "" {
		t.Errorf("the first plan's input lists open pull requests: %s", cm.Data[keyOpenPullRequests])
	}
	e.gh.mu.Lock()
	fp := e.gh.prs[pr.Number]
	fp.pr.Title, fp.pr.ChangedFiles = "target: Add a version endpoint", 2
	fp.files = []ghclient.PullRequestFile{{Path: "version.go"}, {Path: "main.go"}}
	e.gh.mu.Unlock()
	// Neither an ended intent's pull request nor another Project's is told.
	e.seedIntent("target-8", "target", v1alpha1.IntentClosed, openPR(appRepoURL, 80))
	e.seedIntent("other-9", "other", v1alpha1.IntentInReview, openPR(appRepoURL, 81))
	e.seedPR(80, "acme/app", "open", "ended", 1, ghclient.PullRequestFile{Path: "version.go"})
	e.seedPR(81, "acme/app", "open", "elsewhere", 1, ghclient.PullRequestFile{Path: "version.go"})

	second := e.newIntentFor(2, approver)
	e.drive(second, v1alpha1.IntentAwaitingApproval, repoImage)
	run := e.planRunOf(second)
	spec := e.specOf(run)
	want := []templates.OpenPullRequest{{Intent: first, Repository: appRepoURL, Number: pr.Number, URL: pr.URL,
		Title: "target: Add a version endpoint", ChangedFiles: 2,
		Files: []templates.OpenPullRequestFile{{Path: "version.go"}, {Path: "main.go"}}}}
	if got := decodeOpen(t, spec.OpenPullRequests); !reflect.DeepEqual(got, want) {
		t.Errorf("the second plan was handed %+v, want %+v", got, want)
	}
	cm := e.runInputOf(run)
	if cm.Data[keyOpenPullRequests] != spec.OpenPullRequests {
		t.Errorf("the run's input lists %q, the Job was handed %q", cm.Data[keyOpenPullRequests],
			spec.OpenPullRequests)
	}
	if got := digest([]byte(cm.Data[keyIssue])); got != run.Spec.Inputs.InputDigest ||
		spec.IssueMarkdown != cm.Data[keyIssue] {
		t.Errorf("the request is not the snapshot's alone: digest %s, want %s", got, run.Spec.Inputs.InputDigest)
	}
}

// TestPlanLaunchedWithoutOpenPullRequestsOnFailure: a plan whose list of
// open pull requests cannot be read is planned without one, as it was
// before the list existed, and posted for approval as usual.
func TestPlanLaunchedWithoutOpenPullRequestsOnFailure(t *testing.T) {
	e := newEnv(t, testProject())
	e.seedIntent("target-2", "target", v1alpha1.IntentInReview, openPR(appRepoURL, 2))
	e.seedPR(2, "acme/app", "open", "two", 1, ghclient.PullRequestFile{Path: "a.go"})
	e.gh.repoErrs["ListPullRequestFiles acme/app"] = ghError(http.StatusInternalServerError, "Server Error")
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	run := e.planRunOf(name)
	if spec := e.specOf(run); spec.OpenPullRequests != "" {
		t.Errorf("the plan was handed %s after a failed read", spec.OpenPullRequests)
	}
	if _, ok := e.runInputOf(run).Data[keyOpenPullRequests]; ok {
		t.Error("the plan run's input records a list after a failed read")
	}
	if e.gh.calls["ListPullRequestFiles"] != 1 {
		t.Errorf("files listed %d times, want once: the list is read as the run's input is created",
			e.gh.calls["ListPullRequestFiles"])
	}
}

// TestPlanOpenPullRequestsBoundedAtLaunch: what a plan Job is handed is its
// input's list bounded again, whatever the ConfigMap holds; a list that does
// not decode is left out, and the plan launches without it.
func TestPlanOpenPullRequestsBoundedAtLaunch(t *testing.T) {
	e := newEnv(t, testProject())
	run := &v1alpha1.IntentRun{ObjectMeta: metav1.ObjectMeta{Name: "target-1-plan-r1-a1", Namespace: testNS}}
	many := make([]templates.OpenPullRequest, 9)
	for i := range many {
		many[i] = templates.OpenPullRequest{Intent: "target-2", Repository: appRepoURL, Number: int64(i + 1),
			Title: "line\nbreak"}
	}
	raw, err := json.Marshal(many)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, data string
		want       int
	}{{"bounded again", string(raw), templates.OpenPullRequestsMax}, {"does not decode", "{", 0}, {"none", "", 0}} {
		t.Run(tt.name, func(t *testing.T) {
			got := e.runs.planOpenPullRequests(run, &corev1.ConfigMap{Data: map[string]string{keyOpenPullRequests: tt.data}})
			prs := decodeOpen(t, got)
			if len(prs) != tt.want || tt.want > 0 && prs[0].Title != "line break" {
				t.Errorf("handed %s, want %d bounded", got, tt.want)
			}
		})
	}
}

// planRunOf is the intent's one plan run.
func (e *env) planRunOf(name string) v1alpha1.IntentRun {
	e.t.Helper()
	var plans []v1alpha1.IntentRun
	for _, run := range e.intentRuns(name) {
		if run.Spec.Stage == v1alpha1.IntentStagePlan {
			plans = append(plans, run)
		}
	}
	if len(plans) != 1 {
		e.t.Fatalf("intent %s has %d plan runs, want 1", name, len(plans))
	}
	return plans[0]
}

// runInputOf is the run's input ConfigMap.
func (e *env) runInputOf(run v1alpha1.IntentRun) *corev1.ConfigMap {
	e.t.Helper()
	var cm corev1.ConfigMap
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: run.Spec.Inputs.ConfigMap},
		&cm); err != nil {
		e.t.Fatal(err)
	}
	return &cm
}
