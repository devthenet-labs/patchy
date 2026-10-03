// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/e2e/fakegithub"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// The multi-repository intent e2e (slice 3): one Project over several
// application repositories of the fake GitHub, run by the shipped
// intent-controller with --intent-multi-repo, source-controller,
// preview-controller and the fake kubelet. One plan reads every tree; each
// repository the approved plan names is built in its own image and gets
// its own pull request on patchy-intent/<intent>; rounds run one at a time,
// each on one pull request's repository; the intent ends Merged only when
// every pull request merged. The web repository is named the way patchy
// places no rule on (mixed case, a dot, an underscore): its Project key
// and its preview image leaf are the operator's.
const (
	webRepo    = "Acme.Web_App"
	apiRepo    = "api"
	libRepo    = "lib"
	webRepoURL = "https://127.0.0.1/acme/" + webRepo
	apiRepoURL = "https://127.0.0.1/acme/" + apiRepo
	libRepoURL = "https://127.0.0.1/acme/" + libRepo
	// previewPrefix is the preview image prefix preview-controller holds
	// every component image to.
	previewPrefix = "registry.example/patchy/previews/"
)

// multiRepoArgs turn slice 3 on as the demo's values do: multi-repository
// intents, two intent runs at once (so a fan-out's builds run side by
// side), and the preview projection.
var multiRepoArgs = []string{"--intent-multi-repo", "--intent-max-concurrent-runs", "2", "--intent-previews-enabled"}

// previewAt is a repository's preview: its runtime image under the prefix,
// its port and readiness path, served under path on the intent's host.
func previewAt(leaf, readiness, path string) *v1alpha1.ProjectRepositoryPreview {
	return &v1alpha1.ProjectRepositoryPreview{ProjectPreview: v1alpha1.ProjectPreview{
		ImageRepository: previewPrefix + leaf, Port: 8080, ReadinessPath: readiness,
	}, Path: path}
}

// startMultiRepo runs the intent stack for a Project named projectName over
// repos, each repository declaring the e2e runner image, with
// intent-controller run with extra beside intentArgs; checks are the
// Project's checks.fix. With previews it also runs preview-controller over
// two fixed slots.
func startMultiRepo(t *testing.T, repos []v1alpha1.ProjectRepository, checks []string, previews bool,
	extra ...string) *intentEnv {
	t.Helper()
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		names = append(names, r.URL[strings.LastIndex(r.URL, "/")+1:])
	}
	cl, gh, registry := startSourceStack(t, names...)
	if previews {
		startPreviewController(t, cl)
	}
	e := withIntentsFor(t, cl, gh, v1alpha1.ProjectSpec{
		IntentRepository: intentRepoURL,
		Approvers:        v1alpha1.ProjectApprovers{Logins: []string{approver.Login, reader.Login}},
		Repositories:     repos,
		Checks:           v1alpha1.ProjectChecks{Fix: checks},
	}, extra...)
	e.registry = registry
	return e
}

// startPreviewController runs preview-controller over two fixed slot
// namespaces, as TestPreviewRuntime does.
func startPreviewController(t *testing.T, cl *cluster) {
	t.Helper()
	for _, name := range []string{"patchy-preview-0", "patchy-preview-1"} {
		if err := cl.client.Create(context.Background(),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	cl.controller(t, "preview-controller",
		"--preview-slot-count", "2",
		"--preview-image-prefix", previewPrefix,
		"--preview-host-suffix", "preview.patchy.example.com",
		"--preview-node-pool", "patchy-preview", "--preview-node-class", "patchy-preview",
		"--preview-taint-key", "patchy.devthe.net/preview-only",
		"--preview-poll-interval", "1s")
}

// waitFor waits for the Intent to satisfy cond, and returns it.
func (e *intentEnv) waitFor(t *testing.T, name, why string, cond func(*v1alpha1.Intent) bool) *v1alpha1.Intent {
	t.Helper()
	var in v1alpha1.Intent
	eventually(t, fmt.Sprintf("intent %s: %s", name, why), func() bool {
		if err := e.cl.client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name},
			&in); err != nil {
			return false
		}
		return cond(&in)
	})
	return &in
}

// prOf is the Intent's pull request record in the repository url.
func prOf(t *testing.T, in *v1alpha1.Intent, url string) v1alpha1.IntentPullRequest {
	t.Helper()
	for _, pr := range in.Status.PullRequests {
		if pr.Repository == url {
			return pr
		}
	}
	t.Fatalf("intent %s has no pull request in %s: %+v", in.Name, url, in.Status.PullRequests)
	return v1alpha1.IntentPullRequest{}
}

// repoName is the repository name of an e2e app repository URL.
func repoName(url string) string { return url[strings.LastIndex(url, "/")+1:] }

// tree is one Project repository a plan Job reads beside its own.
type tree struct{ key, url string }

// hex64 is a bare sha256, as a tree's digest is written.
var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// checkTreesPlanJob asserts a multi-repository intent's plan Job and its
// run: the default image, no credential, the planning repository (own at
// ownURL) as the working tree and every other Project repository a tree it
// was handed digest-pinned, unpacked read-only beside it, with the
// repositories manifest naming them all; the run's spec lists each tree with
// the Repository it owns for it, and its status records the commit and
// digest each tree was launched with, which outlive those Repositories.
func (e *intentEnv) checkTreesPlanJob(t *testing.T, r agentRun, planRun, own, ownURL string, trees []tree) {
	t.Helper()
	if got := r.Job.Name; got != jobs.NameFor(planRun, intentRunKind, 1) {
		t.Errorf("plan job = %s, want %s", got, jobs.NameFor(planRun, intentRunKind, 1))
	}
	if r.Image != claudeRunnerImage || r.Job.Annotations[runnerImageSourceAnnotation] != "" {
		t.Errorf("plan job image = %s (source %q), want the default %s: trees only ever meet the default image",
			r.Image, r.Job.Annotations[runnerImageSourceAnnotation], claudeRunnerImage)
	}
	if r.Env["PATCHY_PHASE"] != "plan" || r.Env["PATCHY_REPO"] != "acme/"+repoName(ownURL) {
		t.Errorf("plan job phase %q in %q, want plan in acme/%s", r.Env["PATCHY_PHASE"], r.Env["PATCHY_REPO"],
			repoName(ownURL))
	}
	manifest := own + " /workspace/repo " + ownURL + "\n"
	for _, tr := range trees {
		manifest += tr.key + " /workspace/repos/" + tr.key + " " + tr.url + "\n"
	}
	if string(r.Repositories) != manifest {
		t.Errorf("repositories manifest = %q, want %q", r.Repositories, manifest)
	}
	digests := map[string]string{}
	lines := strings.Split(strings.TrimSuffix(string(r.Trees), "\n"), "\n")
	if len(lines) != len(trees) {
		t.Fatalf("trees file = %q, want one line per tree of %+v", r.Trees, trees)
	}
	for i, line := range lines {
		f := strings.Fields(line)
		if len(f) != 3 || f[0] != trees[i].key || !hex64.MatchString(f[1]) || !strings.HasPrefix(f[2], "http://127.0.0.1:") {
			t.Errorf("trees line %q, want %q, a sha256 and the artifact endpoint's URL", line, trees[i].key)
			continue
		}
		digests[f[0]] = f[1]
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(r.Workspace, filepath.FromSlash(rel)))
		if err != nil {
			return "missing: " + err.Error()
		}
		return string(b)
	}
	if got, want := read("repo/README.md"), "# acme/"+repoName(ownURL)+"\n"; got != want {
		t.Errorf("the working tree's README = %q, want the planning repository's %q", got, want)
	}
	for _, tr := range trees {
		if got, want := read("repos/"+tr.key+"/README.md"), "# acme/"+repoName(tr.url)+"\n"; got != want {
			t.Errorf("tree %s's README = %q, want %q", tr.key, got, want)
		}
	}
	for _, u := range append([]string{ownURL}, func() []string {
		var out []string
		for _, tr := range trees {
			out = append(out, tr.url)
		}
		return out
	}()...) {
		if !strings.Contains(string(r.Issue), "- "+u+"\n") {
			t.Errorf("plan handoff does not list %s:\n%s", u, r.Issue)
		}
	}
	e.checkCredentiallessWith(t, r, secretTrees, secretRepositories)

	run := e.run(t, planRun)
	var wantSpec []v1alpha1.IntentRunTree
	var wantStatus []v1alpha1.IntentRunTreeStatus
	for _, tr := range trees {
		wantSpec = append(wantSpec, v1alpha1.IntentRunTree{Name: tr.key, URL: tr.url,
			RepositoryRef: v1alpha1.LocalObjectReference{Name: v1alpha1.IntentRunTreeRepositoryName(planRun, tr.key)}})
		wantStatus = append(wantStatus, v1alpha1.IntentRunTreeStatus{Name: tr.key, BaseSHA: fakegithub.BaseSHA,
			ArtifactDigest: digests[tr.key]})
	}
	if !slices.Equal(run.Spec.Trees, wantSpec) {
		t.Errorf("plan run spec.trees = %+v, want %+v", run.Spec.Trees, wantSpec)
	}
	if !slices.Equal(run.Status.Trees, wantStatus) {
		t.Errorf("plan run status.trees = %+v, want each tree's commit and the digest its Job verified %+v",
			run.Status.Trees, wantStatus)
	}
}

// checkBuildJob asserts one repository's build Job: in that repository's
// own image, pinned by digest on its own Repository, told its repository
// by PATCHY_REPO, handed the approved plan byte-identical and nothing of the
// request, and holding no credential.
func (e *intentEnv) checkBuildJob(t *testing.T, r agentRun, buildRun, url, plan string) {
	t.Helper()
	var repo v1alpha1.Repository
	if err := e.cl.client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: buildRun + "-src"},
		&repo); err != nil {
		t.Fatalf("read the build repository of %s: %v", buildRun, err)
	}
	if repo.Spec.URL != url {
		t.Errorf("build %s's repository = %s, want %s", buildRun, repo.Spec.URL, url)
	}
	pinned := repo.Status.RunnerImage
	if pinned == nil || !strings.HasPrefix(pinned.Image, e.registry+"/"+runnerImageRepository+"@sha256:") ||
		r.Image != pinned.Image ||
		r.Job.Annotations[runnerImageSourceAnnotation] != v1alpha1.RunnerImageSourceRepository {
		t.Errorf("build %s image = %s (source %q), want its repository's pin %+v", buildRun, r.Image,
			r.Job.Annotations[runnerImageSourceAnnotation], pinned)
	}
	if got, want := r.Env["PATCHY_REPO"], "acme/"+repoName(url); got != want {
		t.Errorf("build %s PATCHY_REPO = %q, want %q", buildRun, got, want)
	}
	if len(r.Issue) != 0 || string(r.Investigation) != plan {
		t.Errorf("build %s was handed a request (%d bytes) or a plan other than the approved one", buildRun,
			len(r.Issue))
	}
	if r.Trees != nil || r.Repositories != nil {
		t.Errorf("build %s was handed trees; a build reads its one repository", buildRun)
	}
	e.checkCredentialless(t, r)
}

// previewOf reads the Intent's Preview, if it has one.
func (e *intentEnv) previewOf(name string) (*v1alpha1.Preview, bool) {
	var p v1alpha1.Preview
	err := e.cl.client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, &p)
	return &p, err == nil
}

// previewObjects are the slot-0 Deployment names of an Intent's preview
// components, in order: the first keeps the single-component name
// preview-<intent>, each other one preview-<intent>-<component>.
func previewObjects(name string, components ...string) []string {
	out := []string{"preview-" + name}
	for _, c := range components[1:] {
		out = append(out, "preview-"+name+"-"+c)
	}
	return out
}

// waitPreview waits for the Intent's Preview to hold want, component by
// component, and for preview-controller to have rendered a Deployment per
// component running exactly its image (sha-<revision> under the prefix).
func (e *intentEnv) waitPreview(t *testing.T, name, why string, want []v1alpha1.PreviewComponent) {
	t.Helper()
	keys := make([]string, 0, len(want))
	for _, c := range want {
		keys = append(keys, c.Name)
	}
	deployments := previewObjects(name, keys...)
	eventually(t, why, func() bool {
		p, ok := e.previewOf(name)
		if !ok || !slices.Equal(p.Spec.Components, want) {
			return false
		}
		for i, c := range want {
			var dep appsv1.Deployment
			if e.cl.client.Get(context.Background(), types.NamespacedName{Namespace: "patchy-preview-0",
				Name: deployments[i]}, &dep) != nil ||
				dep.Spec.Template.Spec.Containers[0].Image != c.ImageRepository+":sha-"+c.Revision {
				return false
			}
		}
		return true
	})
}

// waitPreviewGone waits for the Intent's Preview and every object of its
// components to be removed.
func (e *intentEnv) waitPreviewGone(t *testing.T, name string, components ...string) {
	t.Helper()
	eventually(t, "the preview and its slot objects removed", func() bool {
		if _, ok := e.previewOf(name); ok {
			return false
		}
		for _, d := range previewObjects(name, components...) {
			key := types.NamespacedName{Namespace: "patchy-preview-0", Name: d}
			if !apierrors.IsNotFound(e.cl.client.Get(context.Background(), key, &appsv1.Deployment{})) ||
				!apierrors.IsNotFound(e.cl.client.Get(context.Background(), key, &corev1.Service{})) {
				return false
			}
		}
		return true
	})
}

// failCheck records a failed run of the named check "test" on head, whose
// Actions job log is log, as GitHub reports one; id numbers the check run,
// its workflow run (id+1) and its job (id+2).
func (e *intentEnv) failCheck(head string, id int64, log string) {
	e.gh.SetCheckRun(head, fakegithub.CheckRun{ID: id, Name: "test", Status: "completed", Conclusion: "failure",
		Title: "go test failed", Summary: "1 test failed", RunID: id + 1,
		Annotations: []fakegithub.CheckAnnotation{{Path: "greeting_test.go", Line: 14, Message: "want hello"}}})
	e.gh.SetWorkflowJob(id+1, fakegithub.WorkflowJob{ID: id + 2, CheckRunID: id, HeadSHA: head,
		Name: "test", Conclusion: "failure", Log: log})
}

// actionsLog is an Actions job log of the one failure, as a run of it at
// second sec on runner prints it: every line stamped with the time it was
// written, the runner's names, a duration. Two runs of it differ in nothing
// else, so they are the same failure.
func actionsLog(sec int, runner string) string {
	stamp := func(frac int) string { return fmt.Sprintf("2026-10-03T09:12:%02d.%07dZ ", sec, frac) }
	lines := []string{
		"\ufeff" + stamp(1) + "##[group]Runner Image Provisioner",
		stamp(2) + "Runner name: '" + runner + "'",
		stamp(3) + "Machine name: 'runnervm" + runner + "'",
		stamp(4) + "##[endgroup]",
		stamp(5) + "##[group]Run go test -race ./...",
		stamp(6) + "go test -race ./...",
		stamp(7) + "##[endgroup]",
		stamp(8) + "--- FAIL: TestGreeting (0.0" + fmt.Sprint(sec%7) + "s)",
		stamp(9) + "    greeting_test.go:14: got \"hi\", want \"hello\"",
		stamp(10) + "FAIL\tgithub.com/acme/api\t0." + fmt.Sprint(400+sec) + "s",
		stamp(11) + "##[error]Process completed with exit code 1.",
		stamp(12) + "Post job cleanup.",
	}
	return strings.Join(lines, "\n") + "\n"
}

// TestMultiRepoIntentLifecycle drives one intent over two repositories from
// the issue to the merge: one plan Job reading both trees, both builds (one
// per repository, each in its own image), two pull requests on
// patchy-intent/<intent> cross-linked by one comment each, a two-component
// preview at both pull requests' heads, a revision round on the web pull
// request and a CI-fix round on the API's (each pushed to its own branch
// only, never forced), and Merged only once both have merged.
func TestMultiRepoIntentLifecycle(t *testing.T) {
	e := startMultiRepo(t, []v1alpha1.ProjectRepository{
		{Name: "web", URL: webRepoURL, Preview: previewAt("acme-web", "/healthz", "/")},
		{Name: "api", URL: apiRepoURL, Preview: previewAt("acme-api", "/api/healthz", "/api")},
	}, []string{"test"}, true, multiRepoArgs...)
	ctx := context.Background()

	// 1. One plan over both trees, read-only on the default image.
	number, name := e.fileIntent(t)
	planRun := v1alpha1.IntentRunName(name, v1alpha1.IntentStagePlan, 1, "", 1)
	planJob := e.kubelet.waitRun(t, "the plan job to run", phaseIs("plan"))
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	e.checkTreesPlanJob(t, planJob, planRun, "web", webRepoURL, []tree{{"api", apiRepoURL}})
	pl := in.Status.Plan
	if !slices.Equal(pl.Repositories, []string{webRepoURL, apiRepoURL}) {
		t.Errorf("plan repositories = %v, want both", pl.Repositories)
	}
	planComments := e.own(number, templates.PlanMarker(namespace, name, 1, pl.Digest))
	if len(planComments) != 1 || !strings.Contains(planComments[0].Body,
		"patchy opens one pull request in each of: `acme/"+webRepo+"`, `acme/api`.") {
		t.Errorf("plan comments = %+v, want one naming both repositories in its header", planComments)
	}
	eventually(t, "the plan's repositories, its trees' included, to be deleted once collected", func() bool {
		for _, n := range []string{planRun + "-src", v1alpha1.IntentRunTreeRepositoryName(planRun, "api")} {
			if !apierrors.IsNotFound(e.cl.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: n},
				&v1alpha1.Repository{})) {
				return false
			}
		}
		return true
	})
	// The run keeps what the planner saw after its Repositories are gone.
	if run := e.run(t, planRun); len(run.Status.Trees) != 1 || run.Status.Trees[0].BaseSHA != fakegithub.BaseSHA {
		t.Errorf("plan run status.trees after collection = %+v, want the api tree recorded", run.Status.Trees)
	}
	var planCM corev1.ConfigMap
	if err := e.cl.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: pl.ConfigMap}, &planCM); err != nil {
		t.Fatalf("read the plan configmap: %v", err)
	}
	plan := planCM.Data["plan.md"]

	// 2. The approval builds each repository, in its own image.
	afterSecond(pl.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)
	branch := v1alpha1.IntentBranchPrefix + name
	heads := map[string]string{}
	for _, c := range []struct{ key, url string }{{"web", webRepoURL}, {"api", apiRepoURL}} {
		buildRun := v1alpha1.IntentRunName(name, v1alpha1.IntentStageBuild, 1, c.key, 1)
		job := e.kubelet.waitRun(t, "the "+c.key+" build job to run", func(r agentRun) bool {
			return r.Job.Name == jobs.NameFor(buildRun, intentRunKind, 1)
		})
		e.checkBuildJob(t, job, buildRun, c.url, plan)
	}

	// 3. Both pushed, then both pull requests opened: in review.
	in = e.waitPhase(t, name, v1alpha1.IntentInReview)
	if len(in.Status.PullRequests) != 2 {
		t.Fatalf("pull requests = %+v, want one per repository", in.Status.PullRequests)
	}
	prs := map[string]v1alpha1.IntentPullRequest{}
	for _, c := range []struct{ key, url string }{{"web", webRepoURL}, {"api", apiRepoURL}} {
		run := e.run(t, v1alpha1.IntentRunName(name, v1alpha1.IntentStageBuild, 1, c.key, 1))
		pushed := run.Status.PushedCommit
		rec := prOf(t, in, c.url)
		prs[c.key], heads[c.key] = rec, pushed
		if run.Status.Phase != v1alpha1.RunComplete || run.Status.BaseSHA != fakegithub.BaseSHA || rec.HeadSHA != pushed {
			t.Errorf("%s build = %s on %s pushed %s, PR head %s; want Complete on the base, the PR at its push",
				c.key, run.Status.Phase, run.Status.BaseSHA, pushed, rec.HeadSHA)
		}
		want := []fakegithub.RefWrite{{Op: "create", Ref: "heads/" + branch, SHA: pushed, Status: 201}}
		if got := e.gh.RepoRefWrites(intentOwner, repoName(c.url)); !slices.Equal(got, want) {
			t.Errorf("%s ref writes = %+v, want its own branch created once %+v", c.key, got, want)
		}
		if commit, ok := e.gh.CommitOf(pushed); !ok || !slices.Equal(commit.Parents, []string{fakegithub.BaseSHA}) ||
			string(commit.Files["VERSION"]) != "0.1.0\n" {
			t.Errorf("%s commit = %+v, want the build's VERSION on the base", c.key, commit)
		}
		pr, ok := e.gh.Pull(int(rec.Number))
		if !ok || pr.Repository != intentOwner+"/"+repoName(c.url) || pr.Head != branch || pr.Base != "main" {
			t.Fatalf("%s pull request = %+v, want %s into main of its own repository", c.key, pr, branch)
		}
		if !strings.HasPrefix(pr.Body, fmt.Sprintf("Part of %s/%s#%d", intentOwner, intentRepo, number)) ||
			!strings.Contains(pr.Body, "one of 2 pull requests patchy opened for the intent, one in each of: `acme/"+
				webRepo+"`, `acme/api`") {
			t.Errorf("%s pull request body does not say it is one of two:\n%s", c.key, pr.Body)
		}
		if m := closingKeyword.FindString(pr.Title + "\n" + pr.Body); m != "" {
			t.Errorf("%s pull request carries the closing keyword %q", c.key, m)
		}
	}

	// 4. Each pull request carries one comment linking the other, posted
	//    after the InReview write and gating nothing.
	siblings := templates.NoticeMarker(namespace, name, templates.SiblingsKey)
	in = e.waitFor(t, name, "the pull requests cross-linked", func(in *v1alpha1.Intent) bool {
		return meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionSiblingsLinked)
	})
	for key, other := range map[string]string{"web": "api", "api": "web"} {
		got := e.own(int(prs[key].Number), siblings)
		if len(got) != 1 || !strings.Contains(got[0].Body, prs[other].URL) ||
			strings.Contains(got[0].Body, prs[key].URL+")") {
			t.Errorf("%s sibling comments = %+v, want one linking the %s pull request %s", key, got, other,
				prs[other].URL)
		}
	}

	// 5. One preview host, a component per repository at its pull
	//    request's head, the web at / and the API at /api.
	webPreview := v1alpha1.PreviewComponent{Name: "web", ImageRepository: previewPrefix + "acme-web",
		Revision: heads["web"], Port: 8080, ReadinessPath: "/healthz"}
	apiPreview := v1alpha1.PreviewComponent{Name: "api", ImageRepository: previewPrefix + "acme-api",
		Revision: heads["api"], Port: 8080, ReadinessPath: "/api/healthz", Path: "/api"}
	e.waitPreview(t, name, "a two-component preview at both heads", []v1alpha1.PreviewComponent{webPreview, apiPreview})
	if len(in.Status.PreviewBases) != 0 {
		t.Errorf("preview bases = %+v, want none: both repositories have a pull request", in.Status.PreviewBases)
	}

	// 6. A revision round on the web pull request: the web repository
	//    alone, in its own image, fast-forwarded.
	e.gh.SetComparePatch(fakegithub.BaseSHA, heads["web"], "diff --git a/VERSION b/VERSION\n+0.1.0\n")
	e.gh.CommentAs(int(prs["web"].Number), "/patchy revise Please also show the version in the footer.", approver)
	e.waitPhase(t, name, v1alpha1.IntentRevising)
	reviseRun := v1alpha1.IntentRunName(name, v1alpha1.IntentStageRevise, 1, "web", 1)
	reviseJob := e.kubelet.waitRun(t, "the web revision job to run", func(r agentRun) bool {
		return r.Job.Name == jobs.NameFor(reviseRun, intentRunKind, 1)
	})
	if reviseJob.Env["PATCHY_REPO"] != "acme/"+webRepo ||
		reviseJob.Job.Annotations[runnerImageSourceAnnotation] != v1alpha1.RunnerImageSourceRepository ||
		!strings.Contains(string(reviseJob.Investigation), "Please also show the version in the footer.") {
		t.Errorf("revision job in %q (source %q), handoff %q; want web's own image and the approver's feedback",
			reviseJob.Env["PATCHY_REPO"], reviseJob.Job.Annotations[runnerImageSourceAnnotation], reviseJob.Investigation)
	}
	e.checkCredentialless(t, reviseJob)
	in = e.waitPhase(t, name, v1alpha1.IntentInReview)
	revise := e.run(t, reviseRun)
	if revise.Status.Phase != v1alpha1.RunComplete || revise.Status.BaseSHA != heads["web"] ||
		revise.Status.PushedCommit == "" || in.Status.Revisions != 1 || in.Status.CheckFixes != 0 {
		t.Fatalf("revision = %+v, revisions %d, check fixes %d", revise.Status, in.Status.Revisions,
			in.Status.CheckFixes)
	}
	heads["web"] = revise.Status.PushedCommit
	if got := e.own(int(prs["web"].Number), fmt.Sprintf("<!-- patchy:intent-pr-round:%s:1 -->", name)); len(got) != 1 ||
		!strings.Contains(got[0].Body, "Revision round pushed commit `"+heads["web"]+"`") {
		t.Errorf("web round comments = %+v, want one saying the revision round pushed %s", got, heads["web"])
	}
	if got := e.own(int(prs["api"].Number), fmt.Sprintf("<!-- patchy:intent-pr-round:%s:1 -->", name)); len(got) != 0 {
		t.Errorf("the api pull request carries the web round's comment: %+v", got)
	}
	webPreview.Revision = heads["web"]
	e.waitPreview(t, name, "the web component moved to the revised head", []v1alpha1.PreviewComponent{webPreview, apiPreview})

	// 7. A CI-fix round on the API pull request, started by its failed
	//    named check, labelled as one.
	e.gh.SetComparePatch(fakegithub.BaseSHA, heads["api"], "diff --git a/VERSION b/VERSION\n+0.1.0\n")
	e.failCheck(heads["api"], 81, actionsLog(31, "GitHub Actions 1000001234"))
	e.waitPhase(t, name, v1alpha1.IntentRevising)
	fixRun := v1alpha1.IntentRunName(name, v1alpha1.IntentStageRevise, 2, "api", 1)
	fixJob := e.kubelet.waitRun(t, "the api CI-fix job to run", func(r agentRun) bool {
		return r.Job.Name == jobs.NameFor(fixRun, intentRunKind, 1)
	})
	if fixJob.Env["PATCHY_REPO"] != "acme/api" || !strings.Contains(string(fixJob.Investigation), "--- FAIL: TestGreeting") {
		t.Errorf("CI-fix job in %q, handoff %q; want the api repository and the failing check's log",
			fixJob.Env["PATCHY_REPO"], fixJob.Investigation)
	}
	e.checkCredentialless(t, fixJob)
	in = e.waitPhase(t, name, v1alpha1.IntentInReview)
	fix := e.run(t, fixRun)
	if fix.Status.Phase != v1alpha1.RunComplete || fix.Status.BaseSHA != heads["api"] || fix.Status.PushedCommit == "" ||
		in.Status.CheckFixes != 1 || in.Status.Revisions != 1 {
		t.Fatalf("CI fix = %+v, revisions %d, check fixes %d", fix.Status, in.Status.Revisions, in.Status.CheckFixes)
	}
	heads["api"] = fix.Status.PushedCommit
	if got := e.own(int(prs["api"].Number), fmt.Sprintf("<!-- patchy:intent-pr-round:%s:2 -->", name)); len(got) != 1 ||
		!strings.Contains(got[0].Body, "CI-fix round for `test` pushed commit `"+heads["api"]+"`") {
		t.Errorf("api round comments = %+v, want one saying the CI-fix round for test pushed %s", got, heads["api"])
	}
	apiPreview.Revision = heads["api"]
	e.waitPreview(t, name, "the api component moved to the fixed head", []v1alpha1.PreviewComponent{webPreview, apiPreview})

	// Each branch was created once, then only fast-forwarded by its own
	// repository's round.
	for key, url := range map[string]string{"web": webRepoURL, "api": apiRepoURL} {
		writes := e.gh.RepoRefWrites(intentOwner, repoName(url))
		if len(writes) != 2 || writes[1].Op != "update" || writes[1].Force || writes[1].SHA != heads[key] ||
			writes[1].Status != 200 {
			t.Errorf("%s ref writes = %+v, want the create then one fast-forward to %s", key, writes, heads[key])
		}
	}

	// 8. The web pull request merges: the intent stays in review, its
	//    preview up, while the API's is open.
	e.gh.MergePull(int(prs["web"].Number), branch, strings.Repeat("1", 40))
	e.waitFor(t, name, "the web merge recorded", func(in *v1alpha1.Intent) bool {
		return prOf(t, in, webRepoURL).State == "merged"
	})
	consistently(t, "the intent to stay in review while a pull request is open", func() bool {
		_, previewed := e.previewOf(name)
		return e.intent(t, name).Status.Phase == v1alpha1.IntentInReview && previewed
	})

	// 9. The API's merges too: Merged, the issue closed as completed, the
	//    preview torn down.
	e.gh.MergePull(int(prs["api"].Number), branch, strings.Repeat("2", 40))
	in = e.waitPhase(t, name, v1alpha1.IntentMerged)
	if in.Status.CompletedAt == nil || prOf(t, in, apiRepoURL).MergeCommitSHA != strings.Repeat("2", 40) {
		t.Errorf("merged status = %+v, want completed with both merges recorded", in.Status)
	}
	if issue := e.issue(t, number); issue.State != "closed" || issue.StateReason != "completed" {
		t.Errorf("intent issue = %s (%s), want closed as completed", issue.State, issue.StateReason)
	}
	summary := e.own(number, notice(name, templates.SummaryKey))
	if len(summary) != 1 || !strings.Contains(summary[0].Body, prs["web"].URL) ||
		!strings.Contains(summary[0].Body, prs["api"].URL) || !strings.Contains(summary[0].Body, "**Revisions:** 1\n") ||
		!strings.Contains(summary[0].Body, "**CI-fix rounds:** 1\n") {
		t.Errorf("summary = %+v, want both merged and the rounds counted apart", summary)
	}
	e.waitPreviewGone(t, name, "web", "api")

	// Every comment patchy wrote, it wrote once.
	wantIssue := map[string]int{
		templates.IntentStatusMarker(namespace, name):       1,
		templates.PlanMarker(namespace, name, 1, pl.Digest): 1,
		notice(name, templates.SummaryKey):                  1,
	}
	consistently(t, "every comment patchy wrote to be written once", func() bool {
		if got := e.botMarkers(number); !maps.Equal(got, wantIssue) {
			t.Logf("issue comments = %v, want %v", got, wantIssue)
			return false
		}
		for key, round := range map[string]int{"web": 1, "api": 2} {
			n := int(prs[key].Number)
			if len(e.own(n, siblings)) != 1 ||
				len(e.own(n, fmt.Sprintf("<!-- patchy:intent-pr-round:%s:%d -->", name, round))) != 1 {
				return false
			}
		}
		return true
	})
}

// TestMultiRepoIntentPartialEnding: a plan that changes two of a Project's
// three repositories. The unchanged web repository's preview runs its main
// head, read once at review start; the API's at its pull request; the
// library has no runtime and no component. A CI failure the API's CI-fix
// round did not fix, seen again with only its log's times, runner and ids
// changed, stops automatic fixing (RepeatedFailure, naming the repository)
// rather than spending another round. Then a human closes the library pull
// request unmerged and merges the API's: once the operator lifts the block,
// the intent ends Closed, with the notice of what merged and what did not,
// and the issue closed as not planned.
func TestMultiRepoIntentPartialEnding(t *testing.T) {
	e := startMultiRepo(t, []v1alpha1.ProjectRepository{
		{Name: "web", URL: webRepoURL, Preview: previewAt("acme-web", "/healthz", "/")},
		{Name: "api", URL: apiRepoURL, Preview: previewAt("acme-api", "/api/healthz", "/api")},
		{Name: "lib", URL: libRepoURL},
	}, []string{"test"}, true, multiRepoArgs...)
	ctx := context.Background()
	// The planner decides only the API and the library must change.
	e.kubelet.setAgentEnv("PATCHY_FAKE_PLAN_REPOSITORIES", apiRepoURL+" "+libRepoURL)

	number, name := e.fileIntent(t)
	planRun := v1alpha1.IntentRunName(name, v1alpha1.IntentStagePlan, 1, "", 1)
	planJob := e.kubelet.waitRun(t, "the plan job to run", phaseIs("plan"))
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	e.checkTreesPlanJob(t, planJob, planRun, "web", webRepoURL, []tree{{"api", apiRepoURL}, {"lib", libRepoURL}})
	pl := in.Status.Plan
	if !slices.Equal(pl.Repositories, []string{apiRepoURL, libRepoURL}) {
		t.Fatalf("plan repositories = %v, want the two that change", pl.Repositories)
	}
	afterSecond(pl.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)

	// Two builds, two pull requests; nothing built in the web repository.
	in = e.waitPhase(t, name, v1alpha1.IntentInReview)
	if len(in.Status.PullRequests) != 2 {
		t.Fatalf("pull requests = %+v, want the API's and the library's", in.Status.PullRequests)
	}
	if _, ok := e.runs(t, name)[v1alpha1.IntentRunName(name, v1alpha1.IntentStageBuild, 1, "web", 1)]; ok {
		t.Error("the web repository, which the plan does not change, was built")
	}
	if got := e.gh.RepoRefWrites(intentOwner, webRepo); len(got) != 0 {
		t.Errorf("web ref writes = %+v, want none", got)
	}
	api, lib := prOf(t, in, apiRepoURL), prOf(t, in, libRepoURL)

	// The web component runs main as of review start, recorded once.
	in = e.waitFor(t, name, "the web preview base recorded", func(in *v1alpha1.Intent) bool {
		return len(in.Status.PreviewBases) > 0
	})
	wantBases := []v1alpha1.IntentPreviewBase{{Repository: webRepoURL, SHA: fakegithub.BaseSHA}}
	if !slices.Equal(in.Status.PreviewBases, wantBases) {
		t.Errorf("preview bases = %+v, want the web repository's main head %+v", in.Status.PreviewBases, wantBases)
	}
	webPreview := v1alpha1.PreviewComponent{Name: "web", ImageRepository: previewPrefix + "acme-web",
		Revision: fakegithub.BaseSHA, Port: 8080, ReadinessPath: "/healthz"}
	apiPreview := v1alpha1.PreviewComponent{Name: "api", ImageRepository: previewPrefix + "acme-api",
		Revision: api.HeadSHA, Port: 8080, ReadinessPath: "/api/healthz", Path: "/api"}
	e.waitPreview(t, name, "web at main and the API at its pull request", []v1alpha1.PreviewComponent{webPreview, apiPreview})

	// The API's check fails; its CI-fix round pushes a fix.
	e.gh.SetComparePatch(fakegithub.BaseSHA, api.HeadSHA, "diff --git a/VERSION b/VERSION\n+0.1.0\n")
	e.failCheck(api.HeadSHA, 81, actionsLog(31, "GitHub Actions 1000001234"))
	e.waitPhase(t, name, v1alpha1.IntentRevising)
	fixRun := v1alpha1.IntentRunName(name, v1alpha1.IntentStageRevise, 1, "api", 1)
	in = e.waitPhase(t, name, v1alpha1.IntentInReview)
	fix := e.run(t, fixRun)
	if fix.Status.Phase != v1alpha1.RunComplete || in.Status.CheckFixes != 1 {
		t.Fatalf("CI fix = %+v, check fixes %d", fix.Status, in.Status.CheckFixes)
	}
	apiHead := fix.Status.PushedCommit
	apiPreview.Revision = apiHead
	e.waitPreview(t, name, "the API component at the fixed head", []v1alpha1.PreviewComponent{webPreview, apiPreview})

	// The same failure again, on another runner at another time: stopped,
	// naming the repository, with no second round.
	e.failCheck(apiHead, 91, actionsLog(58, "GitHub Actions 1000009876"))
	in = e.waitFor(t, name, "the repeated failure to block", func(in *v1alpha1.Intent) bool {
		return in.Status.Phase == v1alpha1.IntentBlocked
	})
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionChecksFailing)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "RepeatedFailure" ||
		!strings.Contains(c.Message, "in acme/api") {
		t.Fatalf("checks condition = %+v, want RepeatedFailure naming acme/api", c)
	}
	consistently(t, "no second CI-fix round", func() bool {
		_, again := e.runs(t, name)[v1alpha1.IntentRunName(name, v1alpha1.IntentStageRevise, 2, "api", 1)]
		cur := e.intent(t, name)
		return !again && cur.Status.CheckFixes == 1 && cur.Status.Phase == v1alpha1.IntentBlocked
	})

	// A human closes the library pull request and merges the API's.
	if !e.gh.ClosePull(int(lib.Number)) {
		t.Fatal("close the library pull request")
	}
	e.gh.MergePull(int(api.Number), v1alpha1.IntentBranchPrefix+name, strings.Repeat("3", 40))

	// The operator lifts the block by changing the Project: every pull
	// request has settled, not all merged, so the intent ends Closed.
	p := e.project(t)
	more := int32(3)
	p.Spec.Limits.MaxCheckFixes = &more
	if err := e.cl.client.Update(ctx, p); err != nil {
		t.Fatalf("update the project: %v", err)
	}
	in = e.waitFor(t, name, "the intent to end Closed", func(in *v1alpha1.Intent) bool {
		return in.Status.Phase == v1alpha1.IntentClosed
	})
	if got := prOf(t, in, libRepoURL).State; got != "closed" {
		t.Errorf("library pull request = %s, want closed", got)
	}
	if issue := e.issue(t, number); issue.State != "closed" || issue.StateReason != "not_planned" {
		t.Errorf("intent issue = %s (%s), want closed as not planned", issue.State, issue.StateReason)
	}
	partial := e.own(number, notice(name, templates.PartialKey))
	if len(partial) != 1 {
		t.Fatalf("partial notices = %+v, want one", partial)
	}
	merged, closed, _ := strings.Cut(partial[0].Body, "**Closed without merging:**")
	if !strings.Contains(merged, api.URL) || strings.Contains(merged, lib.URL) || !strings.Contains(closed, lib.URL) ||
		!strings.Contains(closed, "**CI-fix rounds:** 1\n") {
		t.Errorf("partial notice does not list the API merged and the library closed:\n%s", partial[0].Body)
	}
	if len(e.own(number, notice(name, templates.SummaryKey))) != 0 {
		t.Error("a partly merged intent posted the all-merged summary")
	}
	e.waitPreviewGone(t, name, "web", "api")
	// The base was recorded once and never rewritten.
	if got := e.intent(t, name).Status.PreviewBases; !slices.Equal(got, wantBases) {
		t.Errorf("preview bases at the end = %+v, want %+v", got, wantBases)
	}
}

// TestMultiRepoIntentFlagOff: --intent-multi-repo is a real rollback. An
// intent planned over two repositories while it was on is held where it
// stands once intent-controller runs without it: its Project is not Ready,
// the intent is Blocked (UnsupportedRepositories), and an approval given
// meanwhile builds nothing. Turned on again, the Project is Ready, the
// intent resumes, and the approval that waited builds both repositories.
func TestMultiRepoIntentFlagOff(t *testing.T) {
	e := startMultiRepo(t, []v1alpha1.ProjectRepository{
		{Name: "web", URL: webRepoURL},
		{Name: "api", URL: apiRepoURL},
	}, nil, false, multiRepoArgs...)
	number, name := e.fileIntent(t)
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	pl := in.Status.Plan

	// The operator turns multi-repository intents off.
	e.restartIntents(t)
	in = e.waitFor(t, name, "the intent to be held", func(in *v1alpha1.Intent) bool {
		return in.Status.Phase == v1alpha1.IntentBlocked &&
			meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionUnsupportedRepositories)
	})
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionUnsupportedRepositories); c.Reason !=
		"MultiRepositoryOff" || !strings.Contains(c.Message, "--intent-multi-repo") {
		t.Errorf("held condition = %+v, want MultiRepositoryOff saying which flag lifts it", c)
	}
	if v1alpha1.IntentBlockedFrom(in) != v1alpha1.IntentAwaitingApproval {
		t.Errorf("held from %s, want AwaitingApproval", v1alpha1.IntentBlockedFrom(in))
	}
	eventually(t, "the project to be not Ready", func() bool {
		c := meta.FindStatusCondition(e.project(t).Status.Conditions, v1alpha1.ConditionReady)
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == "UnsupportedRepositories"
	})

	// An approval meanwhile builds nothing.
	afterSecond(pl.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)
	consistently(t, "the held intent to build nothing", func() bool {
		cur := e.intent(t, name)
		return cur.Status.Phase == v1alpha1.IntentBlocked && cur.Status.Approval == nil &&
			len(e.runs(t, name)) == 1 && len(e.kubelet.Runs()) == 1
	})

	// On again: the project is Ready, and the approval that waited builds
	// both repositories.
	e.restartIntents(t, multiRepoArgs...)
	eventually(t, "the project to be Ready again", func() bool {
		return meta.IsStatusConditionTrue(e.project(t).Status.Conditions, v1alpha1.ConditionReady)
	})
	in = e.waitFor(t, name, "both pull requests opened", func(in *v1alpha1.Intent) bool {
		return in.Status.Phase == v1alpha1.IntentInReview && len(in.Status.PullRequests) == 2
	})
	if ap := in.Status.Approval; ap == nil || ap.By != approver.Login || ap.PlanDigest != pl.Digest {
		t.Errorf("approval = %+v, want the approver's waiting approval of %s", ap, pl.Digest)
	}
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionUnsupportedRepositories); c == nil ||
		c.Status != metav1.ConditionFalse {
		t.Errorf("held condition = %+v, want it resolved", c)
	}
}

// multiRepoAddsNothing is the rest of TestIntentLifecycleMultiRepoOn's
// claim: with slice 3 on, a one-repository intent's runs had no trees, it
// recorded no preview base and no cross-link, and no Preview was ever made
// for its Project, which previews nothing.
func multiRepoAddsNothing(t *testing.T, e *intentEnv, name string) {
	t.Helper()
	in := e.intent(t, name)
	if len(in.Status.PreviewBases) != 0 || meta.FindStatusCondition(in.Status.Conditions,
		v1alpha1.ConditionSiblingsLinked) != nil {
		t.Errorf("one-repository intent status = %+v, want no preview base and no cross-link", in.Status)
	}
	for runName, run := range e.runs(t, name) {
		if len(run.Spec.Trees) != 0 || len(run.Status.Trees) != 0 {
			t.Errorf("run %s has trees %+v / %+v", runName, run.Spec.Trees, run.Status.Trees)
		}
	}
	var previews v1alpha1.PreviewList
	if err := e.cl.client.List(context.Background(), &previews); err != nil || len(previews.Items) != 0 {
		t.Errorf("previews = %d (%v), want none", len(previews.Items), err)
	}
}
