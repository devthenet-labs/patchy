// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/controller/intent"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// projectionRepos are the repositories a random Project lists from: one
// spelled freely, so a pull request recorded in another case still matches.
var projectionRepos = []struct{ key, url, recorded string }{
	{"app", "https://github.com/acme/app", "https://github.com/acme/app"},
	{"web", "https://github.com/acme/Acme.Web_App", "https://github.com/acme/acme.web_app.git"},
	{"api", "https://github.com/acme/api", "https://github.com/acme/api/"},
}

// randomProjection is a random Project and Intent state: one to three
// repositories, previewed in the shorthand, per repository (with valid,
// invalid and repeated paths) or not at all; any phase, Blocked from any;
// pull requests in any state at a 40-hex head or not; bases or none.
func randomProjection(rng *rand.Rand) (*v1alpha1.Project, *v1alpha1.Intent) {
	n := 1 + rng.Intn(len(projectionRepos))
	p := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "patchy"}}
	contract := func(i int) v1alpha1.ProjectPreview {
		return v1alpha1.ProjectPreview{ImageRepository: "registry.example/patchy/previews/" + projectionRepos[i].key,
			Port: int32(8080 + i), ReadinessPath: "/healthz"}
	}
	// Mostly each repository's own valid path; sometimes a repeated, an
	// explicit root or a malformed one.
	paths := []string{"", "/", "/api", "/web", "/x/y", "/API", "api"}
	path := func(i int) string {
		if rng.Intn(5) == 0 {
			return paths[rng.Intn(len(paths))]
		}
		return []string{"", "/web", "/api"}[i]
	}
	for i := range n {
		r := v1alpha1.ProjectRepository{Name: projectionRepos[i].key, URL: projectionRepos[i].url}
		p.Spec.Repositories = append(p.Spec.Repositories, r)
	}
	switch rng.Intn(4) {
	case 0:
		c := contract(0)
		p.Spec.Preview = &c
	case 1, 2:
		for i := range p.Spec.Repositories {
			if rng.Intn(3) > 0 {
				p.Spec.Repositories[i].Preview = &v1alpha1.ProjectRepositoryPreview{ProjectPreview: contract(i),
					Path: path(i)}
			}
		}
	}
	phases := []v1alpha1.IntentPhase{v1alpha1.IntentPending, v1alpha1.IntentBuilding, v1alpha1.IntentInReview,
		v1alpha1.IntentRevising, v1alpha1.IntentBlocked, v1alpha1.IntentMerged, v1alpha1.IntentClosed}
	in := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "target-1", Namespace: "patchy", UID: "intent-uid"},
		Spec: v1alpha1.IntentSpec{Project: "target"}}
	in.Status.Phase = phases[rng.Intn(len(phases))]
	if rng.Intn(2) == 0 {
		in.Status.Phase = phases[2+rng.Intn(2)] // in review, or revising
	}
	if in.Status.Phase == v1alpha1.IntentBlocked {
		from := phases[rng.Intn(4)]
		in.Status.PhaseTimes = []v1alpha1.IntentPhaseTime{{Phase: from}, {Phase: v1alpha1.IntentBlocked}}
	}
	states := []string{"open", "open", "merged", "closed"}
	for i := range n {
		sha := fmt.Sprintf("%040x", rng.Int63())
		if rng.Intn(10) == 0 {
			sha = "short"
		}
		switch rng.Intn(5) {
		case 0, 1, 2:
			in.Status.PullRequests = append(in.Status.PullRequests, v1alpha1.IntentPullRequest{
				Repository: projectionRepos[i].recorded, Number: int64(i + 1), HeadSHA: sha,
				State: states[rng.Intn(len(states))]})
		case 3:
			in.Status.PreviewBases = append(in.Status.PreviewBases, v1alpha1.IntentPreviewBase{
				Repository: projectionRepos[i].url, SHA: fmt.Sprintf("%040x", rng.Int63())})
		}
	}
	return p, in
}

// TestProjectionIsWhatTheControllerAccepts is the writer-to-check cross
// property over random seeded states: intent-controller's preview projection
// (intent.PreviewSourceReconciler, run as deployed) writes a Preview exactly
// when the derivation has one, and preview-controller's re-check
// (matchesApprovedPreview) accepts every Preview it writes, as written. One
// component changed in any field, or one dropped, is refused, so the check
// is not merely the writer's echo.
func TestProjectionIsWhatTheControllerAccepts(t *testing.T) {
	rng := rand.New(rand.NewSource(20261003))
	written := 0
	for i := range 1500 {
		project, in := randomProjection(rng)
		c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithStatusSubresource(in).
			WithObjects(project, in).Build()
		w := &intent.PreviewSourceReconciler{Client: c, APIReader: c, Scheme: kube.Scheme()}
		key := types.NamespacedName{Namespace: in.Namespace, Name: in.Name}
		if _, err := w.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		var p v1alpha1.Preview
		err := c.Get(context.Background(), key, &p)
		_, want := v1alpha1.DesiredPreviewComponents(project, in)
		switch {
		case kerrors.IsNotFound(err):
			if want {
				t.Fatalf("case %d: no Preview written for %+v / %+v", i, project.Spec, in.Status)
			}
			continue
		case err != nil:
			t.Fatal(err)
		case !want:
			t.Fatalf("case %d: a Preview written the derivation has none of: %+v", i, p.Spec)
		case !matchesApprovedPreview(&p, in, project):
			t.Fatalf("case %d: the controller refuses the projection's Preview %+v", i, p.Spec)
		}
		written++
		for k := range 6 {
			forged := p.DeepCopy()
			comp := &forged.Spec.Components[rng.Intn(len(forged.Spec.Components))]
			switch k {
			case 0:
				comp.Revision = fmt.Sprintf("%040x", rng.Int63())
			case 1:
				comp.ImageRepository += "-other"
			case 2:
				comp.Port++
			case 3:
				comp.Path = "/elsewhere"
			case 4:
				comp.Name += "x"
			default:
				forged.Spec.Components = forged.Spec.Components[1:]
			}
			if matchesApprovedPreview(forged, in, project) {
				t.Fatalf("case %d: a forged Preview (change %d) was accepted: %+v", i, k, forged.Spec)
			}
		}
	}
	if written < 100 {
		t.Errorf("only %d of the random states had a Preview; the property tests too little", written)
	}
}
