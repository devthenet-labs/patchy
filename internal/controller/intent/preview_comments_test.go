// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// previewMessage is what the preview controller wrote on the Preview's
// status in every test here: cluster words, which must never reach GitHub.
const previewMessage = "pods \"preview-x\" is forbidden: exceeded quota @octocat #9"

// newOnePreviewEnv is an env over the one-repository Project previewing its
// app, with the preview projection on.
func newOnePreviewEnv(t *testing.T) *env {
	t.Helper()
	p := testProject()
	p.Spec.Preview = &appPreview
	e := newEnv(t, p)
	e.intent.Settings.Previews = true
	return e
}

// inReviewOne drives a new intent to InReview and returns its name and its
// pull request's number.
func (e *env) inReviewOne() (string, int64) {
	e.t.Helper()
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	return name, in.Status.PullRequests[0].Number
}

// previewHostOf is the host preview-controller serves an intent's preview
// at in these tests.
func previewHostOf(name string) string { return name + ".preview.example.com" }

// servePreview plays preview-controller: the preview projection runs
// first, then the Preview's status is set as of its current spec, Ready
// serving every component's revision, with a message of the cluster's.
func (e *env) servePreview(name string, phase v1alpha1.PreviewPhase, url string) {
	e.t.Helper()
	pv := e.previewOf(name)
	if pv == nil {
		e.t.Fatalf("intent %s has no preview", name)
	}
	pv.Status = v1alpha1.PreviewStatus{Phase: phase, ObservedGeneration: pv.Generation, URL: url,
		Message: previewMessage}
	if phase == v1alpha1.PreviewReady {
		for _, c := range pv.Spec.Components {
			pv.Status.Components = append(pv.Status.Components, v1alpha1.PreviewComponentStatus{
				Name: c.Name, Revision: c.Revision, ImageID: "registry.example/" + c.Name + "@sha256:1"})
		}
	}
	// Preview carries no status subresource in the fake client.
	if err := e.c.Update(context.Background(), pv); err != nil {
		e.t.Fatal(err)
	}
}

// previewComments are patchy's preview comments on pull request n.
func (e *env) previewComments(name string, n int64) []ghclient.Comment {
	marker := templates.NoticeMarker(testNS, name, templates.PreviewKey)
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	var out []ghclient.Comment
	for _, c := range e.gh.prComments[n] {
		if markerOf(c.Body) == marker && c.UserLogin == e.gh.bot {
			out = append(out, *c)
		}
	}
	return out
}

// onePreviewComment is the one preview comment on pull request n, failing
// unless there is exactly one.
func (e *env) onePreviewComment(name string, n int64) ghclient.Comment {
	e.t.Helper()
	cs := e.previewComments(name, n)
	if len(cs) != 1 {
		e.t.Fatalf("pull request #%d has %d preview comments, want one: %+v", n, len(cs), cs)
	}
	return cs[0]
}

// pushHead moves pull request n's head to sha, as a push to its branch does.
func (e *env) pushHead(n int64, sha string) {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	e.gh.prs[n].pr.HeadSHA = sha
}

// everyBody is every comment body on the intent issue and every pull
// request.
func (e *env) everyBody() string {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	var b strings.Builder
	for _, is := range e.gh.issues {
		for _, c := range is.comments {
			b.WriteString(c.Body + "\n")
		}
	}
	for _, cs := range e.gh.prComments {
		for _, c := range cs {
			b.WriteString(c.Body + "\n")
		}
	}
	return b.String()
}

// linkTo is how a comment links the live preview at host.
func linkTo(host string) string { return "[`" + host + "`](https://" + host + ")" }

// prWrites counts the preview-comment writes on pull requests: GitHub
// passes through, counting each call by name.
type prWrites struct {
	GitHub
	calls map[string]int
}

func countPRWrites(g GitHub) *prWrites { return &prWrites{GitHub: g, calls: map[string]int{}} }

func (g *prWrites) CreatePullRequestComment(ctx context.Context, repo string, n int64, body string) (
	*ghclient.Comment, error) {
	g.calls["create"]++
	return g.GitHub.CreatePullRequestComment(ctx, repo, n, body)
}

func (g *prWrites) EditPullRequestComment(ctx context.Context, repo string, id int64, body string) error {
	g.calls["edit"]++
	return g.GitHub.EditPullRequestComment(ctx, repo, id, body)
}

// TestPreviewLinkInStatusComment: the status comment says nothing of a
// preview there is none of, says one is being deployed while it is, and
// links it once it is live at the pull request's head; a pass with nothing
// new writes nothing.
func TestPreviewLinkInStatusComment(t *testing.T) {
	e := newOnePreviewEnv(t)
	name, _ := e.inReviewOne()
	e.settleActions(name)
	head := e.get(name).Status.PullRequests[0].HeadSHA
	if body := e.statusBody(); strings.Contains(body, "**Preview:**") {
		t.Errorf("a preview line before there is a Preview:\n%s", body)
	}
	e.previewOf(name) // created; preview-controller has not started it yet
	e.settleActions(name)
	if body := e.statusBody(); !strings.Contains(body, "**Preview:** being deployed at `"+head[:12]+"`") ||
		strings.Contains(body, "https://"+previewHostOf(name)) {
		t.Errorf("status comment while deploying:\n%s", body)
	}
	e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
	e.settleActions(name)
	if body := e.statusBody(); !strings.Contains(body, "**Preview:** "+linkTo(previewHostOf(name))+
		" serves `"+head[:12]+"`.") {
		t.Errorf("status comment once live:\n%s", body)
	}
	edits := e.gh.calls["EditIssueComment"]
	e.settleActions(name)
	if got := e.gh.calls["EditIssueComment"]; got != edits {
		t.Errorf("%d status comment edits with nothing new", got-edits)
	}
	if strings.Contains(e.everyBody(), previewMessage) {
		t.Error("the Preview's status message reached GitHub")
	}
}

// TestPreviewCommentOncePerHead: the pull request's preview comment is
// posted once the preview is first live at its head, never before; a pass
// with nothing new writes nothing; a new head turns the same comment to
// "being deployed" with no link, and back to live at the new head. There is
// only ever the one comment, recorded by id and digest on the pull request.
func TestPreviewCommentOncePerHead(t *testing.T) {
	e := newOnePreviewEnv(t)
	name, n := e.inReviewOne()
	gh := countPRWrites(e.gh)
	e.intent.GitHub = gh
	head := e.get(name).Status.PullRequests[0].HeadSHA
	e.previewOf(name)
	e.servePreview(name, v1alpha1.PreviewDeploying, "")
	e.settleActions(name)
	if cs := e.previewComments(name, n); len(cs) != 0 {
		t.Fatalf("a preview comment before the preview was live: %+v", cs)
	}
	e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
	e.settleActions(name)
	c := e.onePreviewComment(name, n)
	if !strings.Contains(c.Body, "This pull request's head, `"+head[:12]+"`, is live at "+
		linkTo(previewHostOf(name))+".") {
		t.Errorf("preview comment once live:\n%s", c.Body)
	}
	rec := e.get(name).Status.PullRequests[0]
	if rec.PreviewCommentID != c.ID || rec.PreviewDigest != digest([]byte(c.Body)) {
		t.Errorf("record = %d %s, want the comment %d and its digest", rec.PreviewCommentID, rec.PreviewDigest, c.ID)
	}
	if gh.calls["create"] != 1 {
		t.Errorf("%d comments created, want one", gh.calls["create"])
	}
	before := gh.calls["create"] + gh.calls["edit"]
	e.settleActions(name)
	if got := gh.calls["create"] + gh.calls["edit"]; got != before {
		t.Errorf("%d preview comment writes with nothing new", got-before)
	}

	moved := strings.Repeat("9", 40)
	e.pushHead(n, moved)
	e.settleActions(name)
	c = e.onePreviewComment(name, n)
	if !strings.Contains(c.Body, "`"+moved[:12]+"`, is being deployed") || strings.Contains(c.Body, "https://") {
		t.Errorf("preview comment once the head moved:\n%s", c.Body)
	}
	e.previewOf(name) // the new spec: a new generation, status not yet observed
	e.settleActions(name)
	e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
	e.settleActions(name)
	c2 := e.onePreviewComment(name, n)
	if c2.ID != c.ID || !strings.Contains(c2.Body, "`"+moved[:12]+"`, is live at "+linkTo(previewHostOf(name))) {
		t.Errorf("preview comment live at the new head (id %d, first %d):\n%s", c2.ID, c.ID, c2.Body)
	}
	if gh.calls["create"] != 1 || gh.calls["edit"] != 2 {
		t.Errorf("%d created, %d edited; want the one comment edited twice", gh.calls["create"], gh.calls["edit"])
	}
}

// TestPreviewCommentWithdrawnOnFailureAndExpiry: a preview that fails or
// expires takes its link out of both comments, which say why without a word
// of the preview controller's; a new push deploys it again.
func TestPreviewCommentWithdrawnOnFailureAndExpiry(t *testing.T) {
	for _, tt := range []struct {
		phase v1alpha1.PreviewPhase
		says  string
	}{
		{v1alpha1.PreviewFailed, "could not be deployed; the Preview `%s` records why"},
		{v1alpha1.PreviewExpired, "expired after its time to live without a new deployment"},
	} {
		t.Run(string(tt.phase), func(t *testing.T) {
			e := newOnePreviewEnv(t)
			name, n := e.inReviewOne()
			e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
			e.settleActions(name)
			e.servePreview(name, tt.phase, "")
			e.settleActions(name)
			says := tt.says
			if strings.Contains(says, "%s") {
				says = fmt.Sprintf(says, name)
			}
			c := e.onePreviewComment(name, n)
			if !strings.Contains(c.Body, says) || strings.Contains(c.Body, "https://") {
				t.Errorf("preview comment once %s:\n%s", tt.phase, c.Body)
			}
			if body := e.statusBody(); !strings.Contains(body, "**Preview:** "+says) ||
				strings.Contains(body, "https://"+previewHostOf(name)) {
				t.Errorf("status comment once %s:\n%s", tt.phase, body)
			}
			if strings.Contains(e.everyBody(), previewMessage) {
				t.Error("the Preview's status message reached GitHub")
			}
		})
	}
}

// TestPreviewCommentRemovedWhenIntentEnds: once the intent ends, its pull
// request's preview comment says the preview was removed, and the status
// comment has no preview line. An intent whose preview was never live wrote
// no preview comment, and writes none when it ends.
func TestPreviewCommentRemovedWhenIntentEnds(t *testing.T) {
	for _, wasLive := range []bool{true, false} {
		t.Run(fmt.Sprintf("live %v", wasLive), func(t *testing.T) {
			e := newOnePreviewEnv(t)
			name, n := e.inReviewOne()
			gh := countPRWrites(e.gh)
			e.intent.GitHub = gh
			if wasLive {
				e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
			} else {
				e.servePreview(name, v1alpha1.PreviewDeploying, "")
			}
			e.settleActions(name)
			e.gh.closePR(true)
			e.drive(name, v1alpha1.IntentMerged, repoImage)
			e.settleActions(name)
			if body := e.statusBody(); strings.Contains(body, "**Preview:**") {
				t.Errorf("an ended intent's status comment has a preview line:\n%s", body)
			}
			if !wasLive {
				if cs := e.previewComments(name, n); len(cs) != 0 || gh.calls["create"]+gh.calls["edit"] != 0 {
					t.Errorf("a never-live preview wrote %d comments (%v)", len(cs), gh.calls)
				}
				return
			}
			c := e.onePreviewComment(name, n)
			if !strings.Contains(c.Body, "The intent has ended, and its preview was removed.") ||
				strings.Contains(c.Body, "https://") {
				t.Errorf("preview comment once the intent ended:\n%s", c.Body)
			}
			before := gh.calls["edit"]
			e.settleActions(name)
			if gh.calls["edit"] != before {
				t.Errorf("%d more edits of an ended intent's preview comment", gh.calls["edit"]-before)
			}
		})
	}
}

// TestEndedPreviewEditRetried: the last edit of an ended intent's preview
// comment failing for a reason that may pass holds nothing and comes back a
// poll interval later; one GitHub refuses, or a repository patchy can no
// longer reach, is not asked again.
func TestEndedPreviewEditRetried(t *testing.T) {
	e := newOnePreviewEnv(t)
	name, n := e.inReviewOne()
	e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
	e.settleActions(name)
	e.gh.closePR(true)
	e.gh.failNext("EditPullRequestComment", errTransient)
	e.passUntil(name, func() bool { return e.get(name).Status.Phase == v1alpha1.IntentMerged })
	res, err := e.intent.Reconcile(context.Background(), req(name))
	if err != nil || res.RequeueAfter != e.intent.Settings.PRPollInterval {
		t.Fatalf("ended pass with the edit failing = %+v, %v; want a requeue after the poll interval", res, err)
	}
	e.mustIntent(name)
	if c := e.onePreviewComment(name, n); !strings.Contains(c.Body, "preview was removed") {
		t.Errorf("preview comment after the retry:\n%s", c.Body)
	}

	// Unreachable: the ended pass neither fails nor comes back for it.
	e2 := newOnePreviewEnv(t)
	name2, _ := e2.inReviewOne()
	e2.servePreview(name2, v1alpha1.PreviewReady, "https://"+previewHostOf(name2))
	e2.settleActions(name2)
	e2.gh.closePR(true)
	e2.passUntil(name2, func() bool { return e2.get(name2).Status.Phase == v1alpha1.IntentMerged })
	e2.setRepoErrs(map[string]error{"EditPullRequestComment acme/app": ghError(http.StatusForbidden,
		"Resource not accessible by integration")})
	for range 3 {
		res, err := e2.intent.Reconcile(context.Background(), req(name2))
		if err != nil || res.RequeueAfter != 0 {
			t.Fatalf("ended pass with the edit refused = %+v, %v; want done", res, err)
		}
	}
	if got := e2.gh.calls["EditPullRequestComment"]; got != 1 {
		t.Errorf("refused edit asked %d times, want once", got)
	}
}

// lostStatusWrite fails the first Intent status write that records a
// preview comment.
func (e *env) lostStatusWrite() {
	e.failStatusIf = func(in *v1alpha1.Intent) bool {
		for _, pr := range in.Status.PullRequests {
			if pr.PreviewCommentID != 0 {
				return true
			}
		}
		return false
	}
}

// TestPreviewCommentExactlyOnceAcrossLostWrites: a lost response to the
// post, or a lost status write after it, never posts a second comment: the
// next pass edits the one it remembers or, after a restart, finds it by the
// bot's marker. A human's copy of the marker is not taken for it.
func TestPreviewCommentExactlyOnceAcrossLostWrites(t *testing.T) {
	for _, tt := range []struct {
		name     string
		lose     func(*env)
		restart  bool
		moveHead bool
	}{
		{"lost response", func(e *env) { e.intent.GitHub = &lostPRCommentResponse{GitHub: e.gh} }, false, false},
		{"lost status write", (*env).lostStatusWrite, false, true},
		{"lost status write and a restart", (*env).lostStatusWrite, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newOnePreviewEnv(t)
			name, n := e.inReviewOne()
			e.gh.mu.Lock()
			e.gh.prComments[n] = append(e.gh.prComments[n], &ghclient.Comment{ID: 1,
				Body:      templates.NoticeMarker(testNS, name, templates.PreviewKey) + "\nnot patchy's",
				UserLogin: approver, CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()})
			e.gh.mu.Unlock()
			e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
			tt.lose(e)
			_ = e.reconcileIntent(name)
			if tt.restart {
				e.restart()
				e.intent.Settings.Previews = true
			}
			e.intent.GitHub = e.gh
			if tt.moveHead {
				// The preview moves on before the next pass looks (the
				// poll records the new head first, and the preview is no
				// longer live): the comment posted is still the one edited,
				// and recorded, never left behind or a second posted.
				e.pushHead(n, strings.Repeat("8", 40))
				e.clock.Advance(time.Minute)
			}
			e.settleActions(name)
			c := e.onePreviewComment(name, n)
			if rec := e.get(name).Status.PullRequests[0]; rec.PreviewCommentID != c.ID {
				t.Errorf("recorded comment %d, want %d", rec.PreviewCommentID, c.ID)
			}
		})
	}
}

// TestPreviewCommentRespectsRateFloor: under the pull request repository's
// rate floor nothing is posted; back over it, the comment is.
func TestPreviewCommentRespectsRateFloor(t *testing.T) {
	e := newOnePreviewEnv(t)
	name, n := e.inReviewOne()
	e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
	e.gh.mu.Lock()
	e.gh.appRemaining = new(int)
	e.gh.mu.Unlock()
	e.settleActions(name)
	if cs := e.previewComments(name, n); len(cs) != 0 {
		t.Fatalf("posted under the rate floor: %+v", cs)
	}
	e.gh.mu.Lock()
	e.gh.appRemaining = nil
	e.gh.mu.Unlock()
	e.settleActions(name)
	e.onePreviewComment(name, n)
}

// TestPreviewCommentRefusedNotRetriedEveryPass: a locked conversation
// refuses the comment; it is tried once for what it would say, not at every
// pass, and again once that changes.
func TestPreviewCommentRefusedNotRetriedEveryPass(t *testing.T) {
	e := newOnePreviewEnv(t)
	name, n := e.inReviewOne()
	gh := countPRWrites(e.gh)
	e.intent.GitHub = gh
	e.gh.mu.Lock()
	e.gh.lockedPRs[n] = true
	e.gh.mu.Unlock()
	e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
	for range 5 {
		e.mustIntent(name)
		e.clock.Advance(time.Minute)
	}
	if gh.calls["create"] != 1 {
		t.Errorf("refused comment tried %d times, want once", gh.calls["create"])
	}
	e.gh.mu.Lock()
	e.gh.lockedPRs[n] = false
	e.gh.mu.Unlock()
	e.pushHead(n, strings.Repeat("7", 40))
	e.settleActions(name)
	e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
	e.settleActions(name)
	if c := e.onePreviewComment(name, n); !strings.Contains(c.Body, "`777777777777`, is live") {
		t.Errorf("preview comment once unlocked:\n%s", c.Body)
	}
}

// TestPreviewCommentDeletedByHumanIsReposted: a deleted comment is not
// posted again to say the preview is redeploying, only once it is live
// again, as a new comment, recorded.
func TestPreviewCommentDeletedByHumanIsReposted(t *testing.T) {
	e := newOnePreviewEnv(t)
	name, n := e.inReviewOne()
	e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
	e.settleActions(name)
	first := e.onePreviewComment(name, n)
	e.gh.deleteComment(first.ID)
	e.servePreview(name, v1alpha1.PreviewDeploying, "")
	e.settleActions(name)
	if cs := e.previewComments(name, n); len(cs) != 0 {
		t.Fatalf("a deleted comment was posted again while deploying: %+v", cs)
	}
	if rec := e.get(name).Status.PullRequests[0]; rec.PreviewCommentID != 0 || rec.PreviewDigest != "" {
		t.Errorf("record of a deleted comment = %d %q, want none", rec.PreviewCommentID, rec.PreviewDigest)
	}
	e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
	e.settleActions(name)
	again := e.onePreviewComment(name, n)
	if again.ID == first.ID || e.get(name).Status.PullRequests[0].PreviewCommentID != again.ID {
		t.Errorf("reposted comment %d (first %d), recorded %d", again.ID, first.ID,
			e.get(name).Status.PullRequests[0].PreviewCommentID)
	}
}

// TestHostilePreviewURLNeverPosted: whatever URL the Preview's status
// holds, nothing but a bare https host under the intent's label is linked:
// no preview comment is posted, the status comment says the address is not
// one patchy links, and the URL appears nowhere on GitHub.
func TestHostilePreviewURLNeverPosted(t *testing.T) {
	for _, url := range []string{
		"https://evil.example", "https://x.y/@a", "https://%s.preview.example.com/login",
		"https://%s.preview.example.com?next=https://evil.example", "https://u@%s.preview.example.com",
		"https://%s.preview.example.com:8443", "http://%s.preview.example.com", "https://%s",
	} {
		t.Run(url, func(t *testing.T) {
			e := newOnePreviewEnv(t)
			name, n := e.inReviewOne()
			if strings.Contains(url, "%s") {
				url = fmt.Sprintf(url, name)
			}
			e.servePreview(name, v1alpha1.PreviewReady, url)
			e.settleActions(name)
			if cs := e.previewComments(name, n); len(cs) != 0 {
				t.Errorf("a preview comment for %s: %+v", url, cs)
			}
			if body := e.statusBody(); !strings.Contains(body, "its address is not one patchy links") {
				t.Errorf("status comment for %s:\n%s", url, body)
			}
			if strings.Contains(e.everyBody(), url) {
				t.Errorf("%s reached GitHub", url)
			}
		})
	}
}

// previewGets counts the Previews read through it.
type previewGets struct {
	client.Reader
	gets int
	err  error
}

func (r *previewGets) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	if _, ok := obj.(*v1alpha1.Preview); ok {
		r.gets++
		if r.err != nil {
			return r.err
		}
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

// TestNoPreviewReadsWithoutProjectionOrProjectPreview: with the preview
// projection off, or a Project that previews nothing, no Preview is ever
// read, nothing is posted on the pull request, and the status comment is
// byte-identical either way.
func TestNoPreviewReadsWithoutProjectionOrProjectPreview(t *testing.T) {
	bodies := map[string]string{}
	for _, tt := range []struct {
		name               string
		previews, projects bool
		wantReads          bool
	}{
		{"projection off", false, true, false},
		{"no project preview", true, false, false},
		{"both", true, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := testProject()
			if tt.projects {
				p.Spec.Preview = &appPreview
			}
			e := newEnv(t, p)
			e.intent.Settings.Previews = tt.previews
			reads := &previewGets{Reader: e.c}
			e.intent.APIReader = reads
			name, n := e.inReviewOne()
			e.settleActions(name)
			if (reads.gets > 0) != tt.wantReads {
				t.Errorf("%d Preview reads, want any: %v", reads.gets, tt.wantReads)
			}
			if cs := e.previewComments(name, n); len(cs) != 0 {
				t.Errorf("preview comments: %+v", cs)
			}
			bodies[tt.name] = e.statusBody()
		})
	}
	if bodies["projection off"] != bodies["no project preview"] {
		t.Errorf("status comments differ:\n%s\n---\n%s", bodies["projection off"], bodies["no project preview"])
	}
}

// TestPreviewReadErrorChangesNothing: a failed read of the Preview shows no
// link (it fails closed) and leaves the pull request's comment as it is.
func TestPreviewReadErrorChangesNothing(t *testing.T) {
	e := newOnePreviewEnv(t)
	name, n := e.inReviewOne()
	e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
	e.settleActions(name)
	live := e.onePreviewComment(name, n)
	reads := &previewGets{Reader: e.c, err: kerrors.NewInternalError(errors.New("etcd unavailable"))}
	e.intent.APIReader = reads
	e.servePreview(name, v1alpha1.PreviewFailed, "")
	e.settleActions(name)
	if reads.gets == 0 {
		t.Fatal("the Preview was not read")
	}
	if c := e.onePreviewComment(name, n); c.Body != live.Body {
		t.Errorf("a failed read changed the preview comment:\n%s", c.Body)
	}
	if body := e.statusBody(); strings.Contains(body, "https://"+previewHostOf(name)) ||
		strings.Contains(body, "**Preview:**") {
		t.Errorf("status comment after a failed read:\n%s", body)
	}
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentInReview {
		t.Errorf("phase %s after a failed read, want InReview", in.Status.Phase)
	}
}

// TestPreviewCommentsOnPreviewedPullRequestsOnly: a multi-repository
// intent's preview comment goes on the pull request of each previewed
// repository, under its path, and never on a library's. Held Blocked from
// review, the comment still follows the preview. A repository that left the
// Project is written nothing, even once the intent ends.
func TestPreviewCommentsOnPreviewedPullRequestsOnly(t *testing.T) {
	e := newPreviewEnv(t, previewedProject(false)) // app at /api; web a library
	name := e.inReviewMulti()
	in := e.get(name)
	app, web := recordedPullRequest(in, appRepoURL), recordedPullRequest(in, webRepoURL)
	e.settleActions(name)
	e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
	e.settleActions(name)
	c := e.onePreviewComment(name, app.Number)
	if !strings.Contains(c.Body, "is live at "+linkTo(previewHostOf(name))) {
		t.Errorf("app preview comment:\n%s", c.Body)
	}
	if cs := e.previewComments(name, web.Number); len(cs) != 0 {
		t.Errorf("the library's pull request has preview comments: %+v", cs)
	}

	// Held Blocked from review (multi-repository intents turned off): the
	// comment still follows the preview.
	e.multiRepo(false)
	e.passUntil(name, func() bool { return e.get(name).Status.Phase == v1alpha1.IntentBlocked })
	e.servePreview(name, v1alpha1.PreviewFailed, "")
	e.settleActions(name)
	if c := e.onePreviewComment(name, app.Number); !strings.Contains(c.Body, "could not be deployed") {
		t.Errorf("app preview comment while blocked:\n%s", c.Body)
	}
	e.multiRepo(true)
	e.passUntil(name, func() bool { return e.get(name).Status.Phase == v1alpha1.IntentInReview })

	// The app repository leaves the Project: its comment is not touched
	// again, ended intent included.
	edits := e.gh.calls["EditPullRequestComment"]
	e.dropRepository(appRepoURL)
	if pv := e.previewOf(name); pv != nil {
		t.Fatalf("a preview with nothing left to preview: %+v", pv.Spec)
	}
	e.settleActions(name)
	e.gh.closePRn(app.Number, true)
	e.gh.closePRn(web.Number, true)
	e.settleActions(name)
	if got := e.gh.calls["EditPullRequestComment"]; got != edits {
		t.Errorf("%d edits on a repository that left the project", got-edits)
	}
}

// TestUnreachablePreviewRepositoryHoldsNothing: a comment patchy can no
// longer post (no Forge covers the repository for the write) fails no pass
// and is not tried again at every pass.
func TestUnreachablePreviewRepositoryHoldsNothing(t *testing.T) {
	e := newOnePreviewEnv(t)
	name, n := e.inReviewOne()
	gh := countPRWrites(e.gh)
	e.intent.GitHub = gh
	e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
	e.setRepoErrs(map[string]error{"CreatePullRequestComment acme/app": fmt.Errorf("resolve: %w", forge.ErrNoMatch)})
	for range 4 {
		if err := e.reconcileIntent(name); err != nil {
			t.Fatalf("a pass failed on the unreachable repository: %v", err)
		}
		e.clock.Advance(time.Minute)
	}
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentInReview || gh.calls["create"] != 1 {
		t.Errorf("phase %s after %d tries, want InReview after one", in.Status.Phase, gh.calls["create"])
	}
	if cs := e.previewComments(name, n); len(cs) != 0 {
		t.Errorf("preview comments: %+v", cs)
	}
}
