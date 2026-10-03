// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"maps"
	"reflect"
	"strings"
	"testing"

	kerrors "k8s.io/apimachinery/pkg/api/errors"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// Runtime contracts of the previewed repositories: the API under /api, the
// web page at the root.
var (
	appPreview = v1alpha1.ProjectPreview{ImageRepository: "registry.example/patchy/previews/app", Port: 8080,
		ReadinessPath: "/healthz"}
	webPreview = v1alpha1.ProjectPreview{ImageRepository: "registry.example/patchy/previews/web", Port: 8081,
		ReadinessPath: "/healthz"}
)

// previewedProject is the multi-repository Project with app previewed at
// /api and, when web is, web at the root.
func previewedProject(web bool) *v1alpha1.Project {
	p := testMultiProject()
	p.Spec.Repositories[0].Preview = &v1alpha1.ProjectRepositoryPreview{ProjectPreview: appPreview, Path: "/api"}
	if web {
		p.Spec.Repositories[1].Preview = &v1alpha1.ProjectRepositoryPreview{ProjectPreview: webPreview}
	}
	return p
}

// newPreviewEnv is newMultiEnv over p with the preview projection on.
func newPreviewEnv(t *testing.T, p *v1alpha1.Project) *env {
	t.Helper()
	e := newEnv(t, p)
	e.multiRepo(true)
	e.intent.Settings.Previews = true
	e.jobs.output = multiOutput
	return e
}

// previewOf runs the preview projection once and returns the Intent's
// Preview, nil when there is none.
func (e *env) previewOf(name string) *v1alpha1.Preview {
	e.t.Helper()
	r := &PreviewSourceReconciler{Client: e.c, APIReader: e.c, Scheme: kube.Scheme()}
	if _, err := r.Reconcile(context.Background(), req(name)); err != nil {
		e.t.Fatal(err)
	}
	var p v1alpha1.Preview
	err := e.c.Get(context.Background(), req(name).NamespacedName, &p)
	if kerrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return &p
}

// setWebHead points web's default branch at sha ("" removes it).
func (e *env) setWebHead(sha string) {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	if sha == "" {
		delete(e.gh.heads, fakeRef(webRepoURL, "main"))
		return
	}
	e.gh.heads[fakeRef(webRepoURL, "main")] = sha
}

// TestPreviewOfTwoChangedRepositories: both previewed repositories changed,
// so the preview has two components, in Project order, each at its pull
// request's head under its own path, and nothing is read for a base.
func TestPreviewOfTwoChangedRepositories(t *testing.T) {
	e := newPreviewEnv(t, previewedProject(true))
	name := e.inReviewMulti()
	reads := e.gh.calls["HeadSHA"]
	e.settleActions(name)
	in := e.get(name)
	if len(in.Status.PreviewBases) != 0 || e.gh.calls["HeadSHA"] != reads {
		t.Fatalf("preview bases %+v, %d head reads; want none", in.Status.PreviewBases, e.gh.calls["HeadSHA"]-reads)
	}
	p := e.previewOf(name)
	want := []v1alpha1.PreviewComponent{
		{Name: "app", ImageRepository: appPreview.ImageRepository, Revision: in.Status.PullRequests[0].HeadSHA,
			Port: 8080, ReadinessPath: "/healthz", Path: "/api"},
		{Name: "web", ImageRepository: webPreview.ImageRepository, Revision: in.Status.PullRequests[1].HeadSHA,
			Port: 8081, ReadinessPath: "/healthz"},
	}
	if p == nil || !reflect.DeepEqual(p.Spec.Components, want) {
		t.Fatalf("preview = %+v, want components %+v", p, want)
	}
}

// TestPreviewBaseOfAnUnchangedRepository: the plan changes app alone, and
// web is previewed too. The first review pass records web's default-branch
// head as its preview base, once; the preview runs app's pull request beside
// web at that commit. When main moves on, the base and the preview stay
// where review began (regression: never rewritten). With the projection off
// nothing is read and there is no preview, since web has no revision.
func TestPreviewBaseOfAnUnchangedRepository(t *testing.T) {
	const base, moved = "abcabcabcabcabcabcabcabcabcabcabcabcabca", "defdefdefdefdefdefdefdefdefdefdefdefdefd"
	for _, previews := range []bool{true, false} {
		e := newPreviewEnv(t, previewedProject(true))
		e.intent.Settings.Previews = previews
		e.jobs.output = defaultOutput // the plan names app alone
		e.setWebHead(base)
		name := e.inReviewMulti()
		in := e.get(name)
		if len(in.Status.PullRequests) != 1 {
			t.Fatalf("pull requests = %+v, want app's alone", in.Status.PullRequests)
		}
		e.settleActions(name)
		in = e.get(name)
		if !previews {
			if len(in.Status.PreviewBases) != 0 || e.previewOf(name) != nil {
				t.Errorf("projection off: bases %+v, or a preview", in.Status.PreviewBases)
			}
			continue
		}
		wantBases := []v1alpha1.IntentPreviewBase{{Repository: webRepoURL, SHA: base}}
		if !reflect.DeepEqual(in.Status.PreviewBases, wantBases) {
			t.Fatalf("preview bases = %+v, want %+v", in.Status.PreviewBases, wantBases)
		}
		p := e.previewOf(name)
		if p == nil || len(p.Spec.Components) != 2 || p.Spec.Components[0].Revision != in.Status.PullRequests[0].HeadSHA ||
			p.Spec.Components[1].Name != "web" || p.Spec.Components[1].Revision != base {
			t.Fatalf("preview = %+v, want app's head beside web at %s", p, base)
		}

		e.setWebHead(moved)
		reads := e.gh.calls["HeadSHA"]
		e.settleActions(name)
		if got := e.get(name).Status.PreviewBases; !reflect.DeepEqual(got, wantBases) ||
			e.gh.calls["HeadSHA"] != reads {
			t.Errorf("after main moved: bases %+v, %d more head reads; want unchanged, none", got,
				e.gh.calls["HeadSHA"]-reads)
		}
		if p := e.previewOf(name); p.Spec.Components[1].Revision != base {
			t.Errorf("the preview moved with main to %s", p.Spec.Components[1].Revision)
		}
	}
}

// TestMissingPreviewBaseGivesNoPreview: web's default-branch head cannot be
// read (or is not a 40-hex commit): no base, so no preview, and review goes
// on regardless. Once it reads, the base is recorded and the preview
// appears.
func TestMissingPreviewBaseGivesNoPreview(t *testing.T) {
	e := newPreviewEnv(t, previewedProject(true))
	e.jobs.output = defaultOutput
	name := e.inReviewMulti()
	e.setWebHead("")
	e.gh.mu.Lock()
	delete(e.gh.heads, "main") // no repository's default branch reads now
	e.gh.mu.Unlock()
	e.settleActions(name)
	in := e.get(name)
	if len(in.Status.PreviewBases) != 0 || in.Status.Phase != v1alpha1.IntentInReview || e.previewOf(name) != nil {
		t.Fatalf("unreadable head: phase %s, bases %+v; want in review, no base, no preview", in.Status.Phase,
			in.Status.PreviewBases)
	}
	e.setWebHead("not-a-sha")
	e.settleActions(name)
	if got := e.get(name).Status.PreviewBases; len(got) != 0 || e.previewOf(name) != nil {
		t.Fatalf("a head that is not a commit became the base %+v", got)
	}
	const base = "1234512345123451234512345123451234512345"
	e.setWebHead(base)
	e.settleActions(name)
	if got := e.get(name).Status.PreviewBases; len(got) != 1 || got[0].SHA != base {
		t.Fatalf("bases = %+v, want %s once it reads", got, base)
	}
	if p := e.previewOf(name); p == nil || p.Spec.Components[1].Revision != base {
		t.Errorf("preview = %+v, want web at %s", p, base)
	}
}

// TestLibraryRepositoryIsNotPreviewed: web is a repository with no runtime
// (a library, no preview config): the preview is app's component alone, and
// web's head is never read for it.
func TestLibraryRepositoryIsNotPreviewed(t *testing.T) {
	e := newPreviewEnv(t, previewedProject(false))
	e.jobs.output = defaultOutput // web unchanged, and not previewed either
	name := e.inReviewMulti()
	reads := e.gh.calls["HeadSHA"]
	e.settleActions(name)
	if in := e.get(name); len(in.Status.PreviewBases) != 0 || e.gh.calls["HeadSHA"] != reads {
		t.Fatalf("bases %+v, %d head reads for a library", in.Status.PreviewBases, e.gh.calls["HeadSHA"]-reads)
	}
	p := e.previewOf(name)
	if p == nil || len(p.Spec.Components) != 1 || p.Spec.Components[0].Name != "app" ||
		p.Spec.Components[0].Path != "/api" {
		t.Fatalf("preview = %+v, want app's component alone", p)
	}
}

// TestOneRepositoryPreviewUnchangedByTheProjectionFlag: a one-repository
// Project in the spec.preview shorthand reads nothing more with the
// projection on: the same GitHub calls, no bases, and the one component at
// the root it always had.
func TestOneRepositoryPreviewUnchangedByTheProjectionFlag(t *testing.T) {
	calls := map[bool]map[string]int{}
	for _, previews := range []bool{false, true} {
		p := testProject()
		p.Spec.Preview = &appPreview
		e := newEnv(t, p)
		e.intent.Settings.Previews = previews
		name := e.awaiting()
		e.gh.label(1, "patchy:approved", approver)
		e.drive(name, v1alpha1.IntentInReview, repoImage)
		e.settleActions(name)
		in := e.get(name)
		calls[previews] = maps.Clone(e.gh.calls)
		if len(in.Status.PreviewBases) != 0 {
			t.Errorf("previews %v: bases %+v", previews, in.Status.PreviewBases)
		}
		pv := e.previewOf(name)
		want := []v1alpha1.PreviewComponent{{Name: "app", ImageRepository: appPreview.ImageRepository,
			Revision: in.Status.PullRequests[0].HeadSHA, Port: 8080, ReadinessPath: "/healthz"}}
		if pv == nil || !reflect.DeepEqual(pv.Spec.Components, want) || strings.Contains(pv.Name, "-app") {
			t.Errorf("previews %v: preview = %+v, want %+v", previews, pv, want)
		}
	}
	if !reflect.DeepEqual(calls[false], calls[true]) {
		t.Errorf("GitHub calls differ with the projection on:\noff %v\non  %v", calls[false], calls[true])
	}
}
