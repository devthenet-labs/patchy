// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	previewImageWeb = "registry.example/patchy/previews/acme-web"
	previewImageAPI = "registry.example/patchy/previews/acme-api"
)

var (
	headA = strings.Repeat("a", 40)
	headB = strings.Repeat("b", 40)
	baseC = strings.Repeat("c", 40)
)

func webPreview() ProjectPreview {
	return ProjectPreview{ImageRepository: previewImageWeb, Port: 8080, ReadinessPath: "/healthz"}
}

func apiPreview() ProjectPreview {
	return ProjectPreview{ImageRepository: previewImageAPI, Port: 9090, ReadinessPath: "/api/healthz"}
}

// twoAppProject previews a freely named web repository at / and an API at
// /api, beside a library that is never previewed.
func twoAppProject() *Project {
	return &Project{Spec: ProjectSpec{Repositories: []ProjectRepository{
		{Name: "web", URL: "https://github.com/acme/Acme.Web_App",
			Preview: &ProjectRepositoryPreview{ProjectPreview: webPreview(), Path: "/"}},
		{Name: "lib", URL: "https://github.com/acme/shared-lib"},
		{Name: "api", URL: "https://github.com/acme/api",
			Preview: &ProjectRepositoryPreview{ProjectPreview: apiPreview(), Path: "/api"}},
	}}}
}

// reviewing is an Intent in review with the given pull requests.
func reviewing(prs ...IntentPullRequest) *Intent {
	return &Intent{Status: IntentStatus{Phase: IntentInReview, PullRequests: prs}}
}

func openPR(repo, head string) IntentPullRequest {
	return IntentPullRequest{Repository: repo, Number: 1, State: "open", HeadSHA: head}
}

// blockedFrom is an Intent Blocked after `from`, as SetIntentPhase records it.
func blockedFrom(from IntentPhase, prs ...IntentPullRequest) *Intent {
	in := &Intent{}
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, ph := range []IntentPhase{IntentPending, IntentPlanning, IntentAwaitingApproval, IntentBuilding} {
		in.Status.PhaseTimes = append(in.Status.PhaseTimes, IntentPhaseTime{Phase: ph, At: metav1.NewTime(now)})
		if ph == from {
			break
		}
	}
	if from == IntentInReview || from == IntentRevising {
		in.Status.PhaseTimes = append(in.Status.PhaseTimes, IntentPhaseTime{Phase: from, At: metav1.NewTime(now)})
	}
	in.Status.PhaseTimes = append(in.Status.PhaseTimes, IntentPhaseTime{Phase: IntentBlocked, At: metav1.NewTime(now)})
	in.Status.Phase = IntentBlocked
	in.Status.PullRequests = prs
	return in
}

func TestDesiredPreviewComponents(t *testing.T) {
	legacy := &Project{Spec: ProjectSpec{
		Repositories: []ProjectRepository{{Name: "preview-demo", URL: "https://github.com/acme/preview-demo"}},
		Preview:      &ProjectPreview{ImageRepository: previewImageWeb, Port: 8080, ReadinessPath: "/healthz"},
	}}
	webPR := openPR("https://github.com/acme/Acme.Web_App", headA)
	apiPR := openPR("https://github.com/acme/api", headB)
	webComponent := PreviewComponent{Name: "web", ImageRepository: previewImageWeb, Revision: headA, Port: 8080,
		ReadinessPath: "/healthz"}
	apiComponent := PreviewComponent{Name: "api", ImageRepository: previewImageAPI, Revision: headB, Port: 9090,
		ReadinessPath: "/api/healthz", Path: "/api"}
	tests := []struct {
		name    string
		project *Project
		intent  *Intent
		want    []PreviewComponent
	}{
		{
			// Exactly the component the slice-2 writer has always written,
			// with no path: a live single-repository Preview never moves.
			name: "the legacy shorthand", project: legacy,
			intent: reviewing(openPR("https://github.com/acme/preview-demo", headA)),
			want: []PreviewComponent{{Name: "preview-demo", ImageRepository: previewImageWeb, Revision: headA,
				Port: 8080, ReadinessPath: "/healthz"}},
		},
		{
			name: "the legacy shorthand matching a recased .git url", project: legacy,
			intent: reviewing(openPR("https://GitHub.com/Acme/preview-demo.git", headA)),
			want: []PreviewComponent{{Name: "preview-demo", ImageRepository: previewImageWeb, Revision: headA,
				Port: 8080, ReadinessPath: "/healthz"}},
		},
		{
			name:    "two previewed repositories with two pull requests, in project order",
			project: twoAppProject(), intent: reviewing(apiPR, webPR),
			want: []PreviewComponent{webComponent, apiComponent},
		},
		{
			name: "an unchanged repository runs its recorded base", project: twoAppProject(),
			intent: func() *Intent {
				in := reviewing(apiPR)
				in.Status.PreviewBases = []IntentPreviewBase{{Repository: "https://github.com/acme/acme.web_app", SHA: baseC}}
				return in
			}(),
			want: []PreviewComponent{{Name: "web", ImageRepository: previewImageWeb, Revision: baseC, Port: 8080,
				ReadinessPath: "/healthz"}, apiComponent},
		},
		{
			// A recorded base never stands in for a repository's own PR.
			name: "a pull request wins over a base", project: twoAppProject(),
			intent: func() *Intent {
				in := reviewing(apiPR, webPR)
				in.Status.PreviewBases = []IntentPreviewBase{{Repository: webPR.Repository, SHA: baseC}}
				return in
			}(),
			want: []PreviewComponent{webComponent, apiComponent},
		},
		{
			name: "a merged sibling keeps its head while another is open", project: twoAppProject(),
			intent: func() *Intent {
				merged := webPR
				merged.State = "merged"
				return reviewing(merged, apiPR)
			}(),
			want: []PreviewComponent{webComponent, apiComponent},
		},
		{name: "a missing base", project: twoAppProject(), intent: reviewing(apiPR)},
		{
			name: "only bases: no component from a pull request", project: twoAppProject(),
			intent: func() *Intent {
				in := reviewing(openPR("https://github.com/acme/shared-lib", headA))
				in.Status.PreviewBases = []IntentPreviewBase{
					{Repository: webPR.Repository, SHA: baseC}, {Repository: apiPR.Repository, SHA: baseC},
				}
				return in
			}(),
		},
		{
			// A PR's head is never swapped for main's.
			name: "a pull request head that is not a 40-hex sha", project: twoAppProject(),
			intent: func() *Intent {
				in := reviewing(apiPR, openPR(webPR.Repository, strings.Repeat("d", 64)))
				in.Status.PreviewBases = []IntentPreviewBase{{Repository: webPR.Repository, SHA: baseC}}
				return in
			}(),
		},
		{name: "a project with no preview", project: &Project{Spec: ProjectSpec{
			Repositories: []ProjectRepository{{Name: "target", URL: "https://github.com/acme/target"}},
		}}, intent: reviewing(openPR("https://github.com/acme/target", headA))},
		{name: "revising", project: twoAppProject(), intent: func() *Intent {
			in := reviewing(apiPR, webPR)
			in.Status.Phase = IntentRevising
			return in
		}(), want: []PreviewComponent{webComponent, apiComponent}},
		{name: "blocked from review", project: twoAppProject(), intent: blockedFrom(IntentInReview, apiPR, webPR),
			want: []PreviewComponent{webComponent, apiComponent}},
		{name: "blocked from revising", project: twoAppProject(), intent: blockedFrom(IntentRevising, apiPR, webPR),
			want: []PreviewComponent{webComponent, apiComponent}},
		{name: "blocked from building", project: twoAppProject(), intent: blockedFrom(IntentBuilding, apiPR, webPR)},
		{name: "building", project: twoAppProject(), intent: func() *Intent {
			in := reviewing(apiPR, webPR)
			in.Status.Phase = IntentBuilding
			return in
		}()},
		{name: "merged", project: twoAppProject(), intent: func() *Intent {
			in := reviewing(apiPR, webPR)
			in.Status.Phase = IntentMerged
			return in
		}()},
		{name: "every pull request settled", project: twoAppProject(), intent: func() *Intent {
			a, w := apiPR, webPR
			a.State, w.State = "merged", "closed"
			return reviewing(a, w)
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := DesiredPreviewComponents(tt.project, tt.intent)
			if ok != (tt.want != nil) || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DesiredPreviewComponents = %+v, %v; want %+v, %v", got, ok, tt.want, tt.want != nil)
			}
		})
	}
}

// TestEffectivePreviewsFailsClosed: a Project the schema refuses previews
// nothing, so a forged or never-validated Project cannot widen a Preview.
func TestEffectivePreviewsFailsClosed(t *testing.T) {
	repo := func(name, path string) ProjectRepository {
		return ProjectRepository{Name: name, URL: "https://github.com/acme/" + name,
			Preview: &ProjectRepositoryPreview{ProjectPreview: webPreview(), Path: path}}
	}
	tests := []struct {
		name    string
		project ProjectSpec
		want    []string // effective paths, in order
	}{
		{"an omitted path is /", ProjectSpec{Repositories: []ProjectRepository{repo("web", "")}}, []string{"/"}},
		{"four previewed repositories", ProjectSpec{Repositories: []ProjectRepository{
			repo("a", "/"), repo("b", "/b"), repo("c", "/c"), repo("d", "/d"),
		}}, []string{"/", "/b", "/c", "/d"}},
		{"five previewed repositories", ProjectSpec{Repositories: []ProjectRepository{
			repo("a", "/"), repo("b", "/b"), repo("c", "/c"), repo("d", "/d"), repo("e", "/e"),
		}}, nil},
		{"two repositories at one path", ProjectSpec{Repositories: []ProjectRepository{
			repo("a", "/api"), repo("b", "/api"),
		}}, nil},
		{"an omitted path beside /", ProjectSpec{Repositories: []ProjectRepository{
			repo("a", ""), repo("b", "/"),
		}}, nil},
		{"a path outside the grammar", ProjectSpec{Repositories: []ProjectRepository{repo("a", "/API")}}, nil},
		{"a trailing slash", ProjectSpec{Repositories: []ProjectRepository{repo("a", "/api/")}}, nil},
		{"the shorthand beside a per-repository preview", ProjectSpec{
			Repositories: []ProjectRepository{repo("a", "/")}, Preview: &ProjectPreview{},
		}, nil},
		{"the shorthand with two repositories", ProjectSpec{
			Repositories: []ProjectRepository{
				{Name: "a", URL: "https://github.com/acme/a"}, {Name: "b", URL: "https://github.com/acme/b"},
			},
			Preview: &ProjectPreview{},
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, rp := range EffectivePreviews(&Project{Spec: tt.project}) {
				got = append(got, rp.Path)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("effective paths = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSameRepositoryURL(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want bool
	}{
		{"https://github.com/acme/web", "https://github.com/acme/web", true},
		{"https://github.com/Acme/Acme.Web_App", "https://github.com/acme/acme.web_app.git", true},
		{" https://github.com/acme/web/ ", "https://github.com/acme/web", true},
		{"https://github.com/acme/web", "https://github.com/acme/web2", false},
		{"https://github.com/acme/web", "https://github.com/other/web", false},
		{"https://github.com/acme/app.github.io", "https://github.com/acme/app", false},
	} {
		if got := SameRepositoryURL(tt.a, tt.b); got != tt.want {
			t.Errorf("SameRepositoryURL(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestPreviewComponentPath(t *testing.T) {
	if got := PreviewComponentPath(PreviewComponent{}); got != "/" {
		t.Errorf("omitted path = %q, want /", got)
	}
	if got := PreviewComponentPath(PreviewComponent{Path: "/api"}); got != "/api" {
		t.Errorf("path = %q, want /api", got)
	}
}

// previewCase is a Project and an Intent drawn so every branch of the
// derivation is reached often: shorthand and per-repository forms, repeated
// and malformed paths, pull requests in every state and with bad heads, bases
// for some repositories (some recased, some for repositories outside the
// Project), and every phase, Blocked from each.
type previewCase struct {
	Project *Project
	Intent  *Intent
}

var previewPaths = []string{"", "/", "/api", "/web", "/api/v1", "/x", "/API"}

func (previewCase) Generate(r *rand.Rand, _ int) reflect.Value {
	n := 1 + r.Intn(3) // mostly small, so valid Previews are common
	if r.Intn(4) == 0 {
		n = 1 + r.Intn(MaxIntentRunTrees+1)
	}
	p := &Project{}
	for i := range n {
		repo := ProjectRepository{Name: fmt.Sprintf("app%d", i), URL: fmt.Sprintf("https://github.com/acme/App%d", i)}
		if r.Intn(2) == 0 {
			repo.Preview = &ProjectRepositoryPreview{
				ProjectPreview: ProjectPreview{ImageRepository: fmt.Sprintf("registry.example/patchy/previews/app%d", i),
					Port: int32(8000 + i), ReadinessPath: "/healthz"},
				Path: previewPaths[r.Intn(len(previewPaths))],
			}
		}
		p.Spec.Repositories = append(p.Spec.Repositories, repo)
	}
	if r.Intn(4) == 0 {
		p.Spec.Preview = &ProjectPreview{ImageRepository: previewImageWeb, Port: 8080, ReadinessPath: "/healthz"}
		if r.Intn(2) == 0 { // usually the valid shorthand
			p.Spec.Repositories = p.Spec.Repositories[:1]
			p.Spec.Repositories[0].Preview = nil
		}
	}
	sha := func() string {
		switch r.Intn(12) {
		case 0:
			return ""
		case 1:
			return strings.Repeat("e", 64)
		default:
			return strings.Repeat(string("0123456789abcdef"[r.Intn(16)]), 40)
		}
	}
	url := func(i int) string {
		u := fmt.Sprintf("https://github.com/acme/App%d", i)
		switch r.Intn(4) {
		case 0:
			return strings.ToLower(u)
		case 1:
			return u + ".git"
		}
		return u
	}
	in := &Intent{}
	for i := range n + 1 { // one past the Project: a repository outside it
		if r.Intn(5) < 3 {
			in.Status.PullRequests = append(in.Status.PullRequests, IntentPullRequest{
				Repository: url(i), Number: int64(i + 1), HeadSHA: sha(),
				State: []string{"open", "open", "open", "merged", "closed"}[r.Intn(5)],
			})
		}
		if r.Intn(5) < 3 {
			in.Status.PreviewBases = append(in.Status.PreviewBases, IntentPreviewBase{Repository: url(i), SHA: sha()})
		}
	}
	phases := []IntentPhase{IntentPending, IntentPlanning, IntentAwaitingApproval, IntentBuilding,
		IntentInReview, IntentRevising, IntentMerged, IntentClosed, IntentFailed}
	now := metav1.NewTime(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
	phase := phases[r.Intn(len(phases))]
	if r.Intn(2) == 0 { // half the time a phase a Preview can exist in
		phase = []IntentPhase{IntentInReview, IntentRevising}[r.Intn(2)]
	}
	in.Status.Phase = phase
	in.Status.PhaseTimes = []IntentPhaseTime{{Phase: phase, At: now}}
	if r.Intn(3) == 0 && !IntentTerminal(phase) {
		in.Status.Phase = IntentBlocked
		in.Status.PhaseTimes = append(in.Status.PhaseTimes, IntentPhaseTime{Phase: IntentBlocked, At: now})
	}
	return reflect.ValueOf(previewCase{Project: p, Intent: in})
}

// oracleRevision is the derivation's revision rule stated directly: the
// repository's pull request decides when there is one, else its base.
func oracleRevision(in *Intent, url string) (rev string, fromPR, found bool) {
	for _, pr := range in.Status.PullRequests {
		if SameRepositoryURL(pr.Repository, url) {
			return pr.HeadSHA, true, true
		}
	}
	for _, b := range in.Status.PreviewBases {
		if SameRepositoryURL(b.Repository, url) {
			return b.SHA, false, true
		}
	}
	return "", false, false
}

// wantPreview is when the derivation must report a Preview, stated directly:
// the Intent wants one, something is previewed, every previewed repository
// has a valid revision, and at least one is a pull request's.
func wantPreview(c previewCase) bool {
	effective := EffectivePreviews(c.Project)
	allRevised, anyPR := len(effective) > 0, false
	for _, rp := range effective {
		rev, fromPR, _ := oracleRevision(c.Intent, rp.URL)
		allRevised = allRevised && previewRevisionPattern.MatchString(rev)
		anyPR = anyPR || fromPR
	}
	return IntentWantsPreview(c.Intent) && allRevised && anyPR
}

// componentsProblem reports what is wrong with derived components, or "":
// at most MaxPreviewComponents, one per previewed repository, named by its
// key in Project order, each at a distinct, valid, canonical path, running
// its repository's contract at its repository's recorded head, at least one
// of them a pull request's.
func componentsProblem(c previewCase, got []PreviewComponent) string {
	effective := EffectivePreviews(c.Project)
	if len(got) == 0 || len(got) > MaxPreviewComponents || len(got) != len(effective) {
		return fmt.Sprintf("%d components for %d previewed repositories", len(got), len(effective))
	}
	last, paths, fromPR := -1, map[string]bool{}, false
	for i, comp := range got {
		idx := slices.IndexFunc(c.Project.Spec.Repositories, func(r ProjectRepository) bool { return r.Name == comp.Name })
		if idx <= last {
			return fmt.Sprintf("component %q out of Project order or repeated", comp.Name)
		}
		last = idx
		path := PreviewComponentPath(comp)
		if paths[path] || !previewPathPattern.MatchString(path) || comp.Path == "/" {
			return fmt.Sprintf("component %q path %q repeated, malformed or not canonical", comp.Name, comp.Path)
		}
		paths[path] = true
		rev, pr, found := oracleRevision(c.Intent, c.Project.Spec.Repositories[idx].URL)
		if !found || comp.Revision != rev || !previewRevisionPattern.MatchString(comp.Revision) {
			return fmt.Sprintf("component %q revision %q is not its repository's recorded head", comp.Name, comp.Revision)
		}
		fromPR = fromPR || pr
		rp := effective[i]
		if comp.ImageRepository != rp.Preview.ImageRepository || comp.Port != rp.Preview.Port ||
			comp.ReadinessPath != rp.Preview.ReadinessPath {
			return fmt.Sprintf("component %q runtime is not its repository's contract", comp.Name)
		}
	}
	if !fromPR {
		return "no component from a pull request"
	}
	return ""
}

// TestDesiredPreviewComponentsProperty pins the derivation's invariants over
// random Projects and Intents. Seeded, so the gate is deterministic.
func TestDesiredPreviewComponentsProperty(t *testing.T) {
	cfg := &quick.Config{MaxCount: 4000, Rand: rand.New(rand.NewSource(20261003))}
	invariants := func(c previewCase) bool {
		got, ok := DesiredPreviewComponents(c.Project, c.Intent)
		problem := ""
		switch want := wantPreview(c); {
		case ok != want:
			problem = fmt.Sprintf("ok = %v, want %v", ok, want)
		case !ok && got != nil:
			problem = "components returned with ok=false"
		case ok:
			problem = componentsProblem(c, got)
		}
		if problem != "" {
			t.Logf("project %+v intent %+v -> %+v, %v: %s", c.Project.Spec, c.Intent.Status, got, ok, problem)
		}
		return problem == ""
	}
	if err := quick.Check(invariants, cfg); err != nil {
		t.Error(err)
	}
}

// TestPreviewShorthandProperty: the shorthand is repositories[0].preview at
// "/", whether the path is omitted or explicit, for every Intent. Seeded.
func TestPreviewShorthandProperty(t *testing.T) {
	cfg := &quick.Config{MaxCount: 4000, Rand: rand.New(rand.NewSource(20261003))}
	equivalent := func(c previewCase, explicitRoot bool) bool {
		repo := c.Project.Spec.Repositories[0]
		repo.Preview = nil
		short := &Project{Spec: ProjectSpec{Repositories: []ProjectRepository{repo},
			Preview: &ProjectPreview{ImageRepository: previewImageWeb, Port: 8080, ReadinessPath: "/healthz"}}}
		path := ""
		if explicitRoot {
			path = "/"
		}
		repo.Preview = &ProjectRepositoryPreview{ProjectPreview: *short.Spec.Preview, Path: path}
		long := &Project{Spec: ProjectSpec{Repositories: []ProjectRepository{repo}}}
		a, aok := DesiredPreviewComponents(short, c.Intent)
		b, bok := DesiredPreviewComponents(long, c.Intent)
		if aok != bok || !reflect.DeepEqual(a, b) {
			t.Logf("shorthand %+v, %v; per-repository %+v, %v", a, aok, b, bok)
			return false
		}
		return true
	}
	if err := quick.Check(equivalent, cfg); err != nil {
		t.Error(err)
	}
}
