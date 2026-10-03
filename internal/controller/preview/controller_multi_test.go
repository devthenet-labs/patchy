// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

const (
	webSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	apiSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	// testPodDeployment ties a test Pod to the Deployment that made it, so
	// collectGarbage can model the garbage collector the fake client lacks.
	testPodDeployment = "test.patchy/deployment"
)

// shopProject previews a freely named web repository at / and an API at
// /api, beside a library that is never previewed.
func shopProject() *v1alpha1.Project {
	preview := func(leaf, readiness, path string) *v1alpha1.ProjectRepositoryPreview {
		return &v1alpha1.ProjectRepositoryPreview{ProjectPreview: v1alpha1.ProjectPreview{
			ImageRepository: testSettings().ImagePrefix + leaf, Port: 8080, ReadinessPath: readiness,
		}, Path: path}
	}
	return &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "patchy"},
		Spec: v1alpha1.ProjectSpec{Repositories: []v1alpha1.ProjectRepository{
			{Name: "web", URL: "https://github.com/acme/Acme.Web_App", Preview: preview("acme-web", "/healthz", "/")},
			{Name: "lib", URL: "https://github.com/acme/lib"},
			{Name: "api", URL: "https://github.com/acme/api", Preview: preview("acme-api", "/api/healthz", "/api")},
		}}}
}

// multiPreview is an Intent of shopProject in review with a pull request in
// each previewed repository, and the Preview the writer derives for it.
func multiPreview(t *testing.T, at time.Time) (*v1alpha1.Intent, *v1alpha1.Preview) {
	t.Helper()
	const name = "shop-1"
	uid := types.UID("intent-" + name)
	in := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy", UID: uid},
		Spec: v1alpha1.IntentSpec{Project: "shop"},
		Status: v1alpha1.IntentStatus{Phase: v1alpha1.IntentInReview, PullRequests: []v1alpha1.IntentPullRequest{
			{Repository: "https://github.com/acme/api", Number: 2, State: "open", HeadSHA: apiSHA},
			{Repository: "https://github.com/acme/acme.web_app", Number: 1, State: "open", HeadSHA: webSHA},
		}}}
	components, ok := v1alpha1.DesiredPreviewComponents(shopProject(), in)
	if !ok || len(components) != 2 {
		t.Fatalf("derived %+v, %v; want two components", components, ok)
	}
	p := &v1alpha1.Preview{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy", UID: types.UID("preview-" + name),
			Generation: 1, CreationTimestamp: metav1.NewTime(at)},
		Spec: v1alpha1.PreviewSpec{
			IntentRef: v1alpha1.ObjectReference{Name: name, UID: uid}, HostLabel: name,
			Components: components, TTL: metav1.Duration{Duration: 72 * time.Hour},
		},
	}
	return in, p
}

func (e *testEnv) slotObject(name string, obj client.Object) error {
	return e.c.Get(context.Background(), types.NamespacedName{Namespace: "patchy-preview-0", Name: name}, obj)
}

// markReady plays the Deployment controller and the kubelet for component i:
// its Deployment has rolled out and a Ready Pod runs its exact image.
func (e *testEnv) markReady(p *v1alpha1.Preview, i int) {
	e.t.Helper()
	ctx := context.Background()
	var dep appsv1.Deployment
	if err := e.slotObject(componentName(p, i), &dep); err != nil {
		e.t.Fatalf("component %d Deployment: %v", i, err)
	}
	dep.Status.ObservedGeneration = dep.Generation
	dep.Status.AvailableReplicas = 1
	if err := e.c.Status().Update(ctx, &dep); err != nil {
		e.t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: dep.Name + "-pod", Namespace: dep.Namespace, Labels: maps.Clone(dep.Spec.Template.Labels),
		Annotations: map[string]string{testPodDeployment: dep.Name},
	}, Spec: dep.Spec.Template.Spec}
	if err := e.c.Create(ctx, pod); err != nil {
		e.t.Fatal(err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: p.Spec.Components[i].Name, Ready: true,
		ImageID: "repo@sha256:" + p.Spec.Components[i].Name}}
	if err := e.c.Status().Update(ctx, pod); err != nil {
		e.t.Fatal(err)
	}
}

// collectGarbage deletes the test Pods whose Deployment is gone, as the
// garbage collector would.
func (e *testEnv) collectGarbage() {
	e.t.Helper()
	ctx := context.Background()
	var pods corev1.PodList
	if err := e.c.List(ctx, &pods, client.InNamespace("patchy-preview-0")); err != nil {
		e.t.Fatal(err)
	}
	for i := range pods.Items {
		var dep appsv1.Deployment
		if err := e.slotObject(pods.Items[i].Annotations[testPodDeployment], &dep); kerrors.IsNotFound(err) {
			if err := e.c.Delete(ctx, &pods.Items[i]); err != nil {
				e.t.Fatal(err)
			}
		}
	}
}

func TestMultiComponentRender(t *testing.T) {
	_, p := multiPreview(t, time.Now())
	s := testSettings()
	if err := s.validatePreview(p); err != nil {
		t.Fatal(err)
	}
	web, api := s.deployment(p, 0, 0), s.deployment(p, 1, 0)
	if web.Name != "preview-shop-1" || api.Name != "preview-shop-1-api" {
		t.Fatalf("deployment names = %q, %q", web.Name, api.Name)
	}
	webSvc, apiSvc := s.service(p, 0, 0), s.service(p, 1, 0)
	if webSvc.Name != web.Name || apiSvc.Name != api.Name {
		t.Fatalf("service names = %q, %q", webSvc.Name, apiSvc.Name)
	}
	// No component's Service selects a sibling's Pods.
	for _, tc := range []struct {
		svc  *corev1.Service
		pods *appsv1.Deployment
		want bool
	}{{webSvc, web, true}, {webSvc, api, false}, {apiSvc, api, true}, {apiSvc, web, false}} {
		got := labels.SelectorFromSet(tc.svc.Spec.Selector).Matches(labels.Set(tc.pods.Spec.Template.Labels))
		if got != tc.want {
			t.Errorf("Service %s selects %s's Pods = %v, want %v", tc.svc.Name, tc.pods.Name, got, tc.want)
		}
	}
	for _, d := range []*appsv1.Deployment{web, api} {
		if !maps.Equal(d.Spec.Selector.MatchLabels, d.Spec.Template.Labels) {
			t.Errorf("%s selector %v does not select its own template %v", d.Name, d.Spec.Selector.MatchLabels,
				d.Spec.Template.Labels)
		}
		assertPodIsolation(t, s, d)
	}
	if !maps.Equal(webSvc.Annotations, map[string]string{annotationHealthcheck: "/healthz"}) ||
		!maps.Equal(apiSvc.Annotations, map[string]string{annotationHealthcheck: "/api/healthz"}) {
		t.Errorf("service annotations = %v, %v", webSvc.Annotations, apiSvc.Annotations)
	}
	ing := s.ingress(p, 0)
	paths := ing.Spec.Rules[0].HTTP.Paths
	got := make([]string, 0, len(paths))
	for _, path := range paths {
		got = append(got, path.Path+"="+path.Backend.Service.Name)
	}
	if want := []string{"/api=preview-shop-1-api", "/=preview-shop-1"}; !slices.Equal(got, want) {
		t.Errorf("ingress paths = %q, want %q (longest first)", got, want)
	}
	if ing.Name != "preview-shop-1" || len(ing.Spec.Rules) != 1 || ing.Annotations[annotationHealthcheck] != "/healthz" ||
		len(ing.Annotations) != 1 {
		t.Errorf("ingress = %+v", ing)
	}
}

// assertPodIsolation: every component's Pod keeps the fixed placement and
// isolation of the single-component one.
func assertPodIsolation(t *testing.T, s Settings, d *appsv1.Deployment) {
	t.Helper()
	pod := d.Spec.Template.Spec
	if pod.NodeSelector["karpenter.sh/nodepool"] != s.NodePool || len(pod.Tolerations) != 1 ||
		pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
		len(pod.Containers) != 1 || len(pod.InitContainers) != 0 || len(pod.Volumes) != 0 ||
		!strings.HasPrefix(pod.Containers[0].Image, s.ImagePrefix) {
		t.Errorf("%s pod escapes placement/isolation: %+v", d.Name, pod)
	}
}

func TestMultiComponentValidation(t *testing.T) {
	_, valid := multiPreview(t, time.Now())
	s := testSettings()
	for _, tc := range []struct {
		name   string
		mutate func(*v1alpha1.Preview)
	}{
		{"five components", func(p *v1alpha1.Preview) {
			for _, n := range []string{"c", "d", "e"} {
				c := p.Spec.Components[1]
				c.Name, c.Path = n, "/"+n
				p.Spec.Components = append(p.Spec.Components, c)
			}
		}},
		{"a repeated name", func(p *v1alpha1.Preview) { p.Spec.Components[1].Name = "web" }},
		{"a repeated path", func(p *v1alpha1.Preview) { p.Spec.Components[1].Path = "/" }},
		{"a path outside the grammar", func(p *v1alpha1.Preview) { p.Spec.Components[1].Path = "/API" }},
		{"a component image off the prefix", func(p *v1alpha1.Preview) {
			p.Spec.Components[1].ImageRepository = "evil.example/patchy/previews/acme-api"
		}},
		{"an agent-environment image", func(p *v1alpha1.Preview) {
			p.Spec.Components[1].ImageRepository = "123456789012.dkr.ecr.us-east-1.amazonaws.com/patchy/app-envs/x"
		}},
		// preview-<52 characters>-api is 64 characters, one past a DNS label.
		{"a name past the Service name limit", func(p *v1alpha1.Preview) {
			long := strings.Repeat("p", 52)
			p.Name, p.Spec.IntentRef.Name, p.Spec.HostLabel = long, long, long
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := valid.DeepCopy()
			tc.mutate(p)
			if s.validatePreview(p) == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestMultiComponentIngressWaitsForEveryComponent(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := multiPreview(t, at)
	e := newTestEnv(t, shopProject(), in, p)
	p = e.untilSlot(p.Name)
	e.step(p.Name) // render every component
	e.markReady(p, 0)
	p = e.step(p.Name)
	var ing networkingv1.Ingress
	if err := e.slotObject(resourceName(p), &ing); !kerrors.IsNotFound(err) {
		t.Fatalf("Ingress exists with one of two components Ready: %v", err)
	}
	if p.Status.Phase == v1alpha1.PreviewReady {
		t.Fatal("Preview Ready with one of two components Ready")
	}
	e.markReady(p, 1)
	p = e.step(p.Name)
	if p.Status.Phase != v1alpha1.PreviewReady {
		t.Fatalf("phase = %s, want Ready", p.Status.Phase)
	}
	if err := e.slotObject(resourceName(p), &ing); err != nil {
		t.Fatalf("Ingress after every component is Ready: %v", err)
	}
	want := []v1alpha1.PreviewComponentStatus{
		{Name: "web", Revision: webSHA, ImageID: "repo@sha256:web"},
		{Name: "api", Revision: apiSHA, ImageID: "repo@sha256:api"},
	}
	if !slices.Equal(p.Status.Components, want) || p.Status.ObservedRevision != webSHA {
		t.Errorf("status components %+v, observed %s; want %+v", p.Status.Components, p.Status.ObservedRevision, want)
	}
}

func TestMultiComponentRetryDeletesEveryDeployment(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := multiPreview(t, at)
	e := newTestEnv(t, shopProject(), in, p)
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	e.markReady(p, 0) // one component up, the other never Ready
	e.now = e.now.Add(11 * time.Minute)
	p = e.step(p.Name)
	if p.Status.Retries != 1 {
		t.Fatalf("retries = %d, want 1", p.Status.Retries)
	}
	for i := range p.Spec.Components {
		if err := e.slotObject(componentName(p, i), &appsv1.Deployment{}); !kerrors.IsNotFound(err) {
			t.Errorf("component %d Deployment survived the retry: %v", i, err)
		}
	}
	// The retried web Pod is still terminating: nothing starts beside it.
	p = e.step(p.Name)
	if err := e.slotObject(componentName(p, 0), &appsv1.Deployment{}); !kerrors.IsNotFound(err) {
		t.Errorf("web Deployment recreated while its old Pod remained: %v", err)
	}
	e.collectGarbage()
	e.step(p.Name)
	for i := range p.Spec.Components {
		if err := e.slotObject(componentName(p, i), &appsv1.Deployment{}); err != nil {
			t.Errorf("component %d Deployment not recreated: %v", i, err)
		}
	}
}

// Regression: a finalizer that removed only preview-<p> would leave a
// further component's Deployment, whose Pods then held the slot forever.
func TestMultiComponentCleanupReleasesSlot(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := multiPreview(t, at)
	e := newTestEnv(t, shopProject(), in, p)
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	e.markReady(p, 0)
	e.markReady(p, 1)
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady {
		t.Fatalf("phase = %s, want Ready", p.Status.Phase)
	}
	ctx := context.Background()
	if err := e.c.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	finalize := func() {
		if _, err := e.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)}); err != nil {
			t.Fatal(err)
		}
	}
	finalize()
	for i := range p.Spec.Components {
		for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}} {
			if err := e.slotObject(componentName(p, i), obj); !kerrors.IsNotFound(err) {
				t.Errorf("component %d %T survived cleanup: %v", i, obj, err)
			}
		}
	}
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(p), &v1alpha1.Preview{}); err != nil {
		t.Fatalf("finalizer released before the Pods were gone: %v", err)
	}
	e.collectGarbage()
	finalize()
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(p), &v1alpha1.Preview{}); !kerrors.IsNotFound(err) {
		t.Fatalf("Preview not finalized after every component was removed: %v", err)
	}
	if busy, err := e.r.slotHasManagedObjects(ctx, 0); err != nil || busy {
		t.Fatalf("slot still occupied after cleanup: %v, %v", busy, err)
	}
}

func TestSweepDeletesComponentOrphan(t *testing.T) {
	_, p := multiPreview(t, time.Now())
	e := newTestEnv(t)
	ctx := context.Background()
	orphan := testSettings().deployment(p, 1, 0)
	// A copied managed-by label and a name the labels do not render.
	decoy := testSettings().deployment(p, 1, 0)
	decoy.Name = "preview-shop-1-other"
	for _, obj := range []client.Object{orphan, decoy} {
		if err := e.c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	sweep := &Sweeper{Client: e.c, Settings: testSettings()}
	if err := sweep.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.slotObject(orphan.Name, &appsv1.Deployment{}); !kerrors.IsNotFound(err) {
		t.Errorf("component orphan %s survived the sweep: %v", orphan.Name, err)
	}
	if err := e.slotObject(decoy.Name, &appsv1.Deployment{}); err != nil {
		t.Errorf("sweep deleted %s, a name its labels do not render: %v", decoy.Name, err)
	}
}

// A component the Project stops previewing is pruned, and the remaining one
// becomes a single-component Preview again: the selector it loses the
// component label from is immutable, so its Deployment is replaced.
func TestDroppedComponentIsPruned(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := multiPreview(t, at)
	project := shopProject()
	e := newTestEnv(t, project, in, p)
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	e.markReady(p, 0)
	e.markReady(p, 1)
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady {
		t.Fatalf("phase = %s, want Ready", p.Status.Phase)
	}
	ctx := context.Background()
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(project), project); err != nil {
		t.Fatal(err)
	}
	project.Spec.Repositories[2].Preview = nil
	if err := e.c.Update(ctx, project); err != nil {
		t.Fatal(err)
	}
	p.Spec.Components = p.Spec.Components[:1]
	p.Generation++
	if err := e.c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		p = e.step(p.Name)
		e.collectGarbage()
	}
	for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}} {
		if err := e.slotObject("preview-shop-1-api", obj); !kerrors.IsNotFound(err) {
			t.Errorf("dropped component's %T survived: %v", obj, err)
		}
	}
	var web appsv1.Deployment
	if err := e.slotObject("preview-shop-1", &web); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(web.Spec.Selector.MatchLabels, labelsFor(p)) {
		t.Errorf("remaining component selector = %v, want the single-component %v", web.Spec.Selector.MatchLabels,
			labelsFor(p))
	}
	var svc corev1.Service
	if err := e.slotObject("preview-shop-1", &svc); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(svc.Spec.Selector, labelsFor(p)) || len(svc.Annotations) != 0 {
		t.Errorf("remaining Service selector %v, annotations %v", svc.Spec.Selector, svc.Annotations)
	}
}

// A single-component Preview the slice-2 controller deployed is left exactly
// as it is: the same objects under the same names, none of them written.
func TestLegacySinglePreviewIsNotRerendered(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := testPreview("demo-1", at)
	slot := int32(0)
	p.Finalizers = []string{finalizer}
	p.Status = v1alpha1.PreviewStatus{Phase: v1alpha1.PreviewReady, ObservedGeneration: p.Generation, Slot: &slot,
		URL: "https://demo-1." + testSettings().HostSuffix, ObservedRevision: testSHA,
		AttemptStartedAt: &metav1.Time{Time: at}, LastDeployedAt: &metav1.Time{Time: at},
		Components: []v1alpha1.PreviewComponentStatus{{Name: "demo", ImageID: "repo@sha256:demo"}}}
	live := renderSingle(testSettings(), p, slot) // exactly what origin/main rendered (the golden)
	e := newTestEnv(t, append([]client.Object{in, p}, live...)...)
	e.markReady(p, 0)
	before := map[string]string{}
	for _, obj := range live {
		if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatal(err)
		}
		before[objectKind(obj)] = obj.GetResourceVersion()
	}
	for range 3 {
		p = e.step(p.Name)
	}
	if p.Status.Phase != v1alpha1.PreviewReady {
		t.Fatalf("phase = %s, want Ready", p.Status.Phase)
	}
	for _, obj := range live {
		if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatal(err)
		}
		if rv := obj.GetResourceVersion(); rv != before[objectKind(obj)] {
			t.Errorf("%s %s was written (resourceVersion %s -> %s)", objectKind(obj), obj.GetName(),
				before[objectKind(obj)], rv)
		}
	}
	want := []v1alpha1.PreviewComponentStatus{{Name: "demo", Revision: testSHA, ImageID: "repo@sha256:demo"}}
	if !slices.Equal(p.Status.Components, want) {
		t.Errorf("status components = %+v, want %+v", p.Status.Components, want)
	}
}

func TestForgedMultiComponentPreviewFails(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		mutate func(*v1alpha1.Preview)
	}{
		{"components out of Project order", func(p *v1alpha1.Preview) {
			p.Spec.Components[0], p.Spec.Components[1] = p.Spec.Components[1], p.Spec.Components[0]
		}},
		{"a sibling at an unrecorded head", func(p *v1alpha1.Preview) {
			p.Spec.Components[1].Revision = strings.Repeat("c", 40)
		}},
		{"a sibling moved to another path", func(p *v1alpha1.Preview) { p.Spec.Components[1].Path = "/v2" }},
		{"a component dropped", func(p *v1alpha1.Preview) { p.Spec.Components = p.Spec.Components[:1] }},
		{"a component's readiness path changed", func(p *v1alpha1.Preview) {
			p.Spec.Components[1].ReadinessPath = "/other"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, p := multiPreview(t, at)
			tc.mutate(p)
			e := newTestEnv(t, shopProject(), in, p)
			e.step(p.Name) // finalizer
			p = e.step(p.Name)
			if p.Status.Phase != v1alpha1.PreviewFailed || p.Status.Slot != nil {
				t.Fatalf("forged Preview not refused: %+v", p.Status)
			}
		})
	}
}

// A Preview is deleted, not failed, once its Intent stops wanting one: no
// open pull request, or a block from before review began.
func TestPreviewDeletedWhenIntentNoLongerWantsIt(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		mutate func(*v1alpha1.Intent)
	}{
		{"every pull request merged", func(in *v1alpha1.Intent) {
			for i := range in.Status.PullRequests {
				in.Status.PullRequests[i].State = "merged"
			}
		}},
		{"blocked from building", func(in *v1alpha1.Intent) {
			in.Status.Phase = v1alpha1.IntentBlocked
			in.Status.PhaseTimes = []v1alpha1.IntentPhaseTime{
				{Phase: v1alpha1.IntentBuilding, At: metav1.NewTime(at)},
				{Phase: v1alpha1.IntentBlocked, At: metav1.NewTime(at)},
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, p := multiPreview(t, at)
			tc.mutate(in)
			e := newTestEnv(t, shopProject(), in, p)
			e.step(p.Name) // finalizer
			if _, err := e.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)}); err != nil {
				t.Fatal(err)
			}
			got := &v1alpha1.Preview{}
			if err := e.c.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
				t.Fatal(err)
			}
			if got.DeletionTimestamp.IsZero() {
				t.Errorf("Preview not deleted: %+v", got.Status)
			}
		})
	}
}
