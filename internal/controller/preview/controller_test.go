// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func testSettings() Settings {
	return Settings{Namespace: "patchy", SlotCount: 2,
		ImagePrefix: "123456789012.dkr.ecr.us-east-1.amazonaws.com/patchy/previews/",
		HostSuffix:  "preview.patchy.example.com", NodePool: "patchy-preview", NodeClass: "patchy-preview",
		TaintKey: "patchy.devthe.net/preview-only", RolloutTimeout: 10 * time.Minute,
		PollInterval: time.Second, MaxRetries: 3}
}

func testPreview(name string, at time.Time) (*v1alpha1.Intent, *v1alpha1.Preview) {
	uid := types.UID("intent-" + name)
	in := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy", UID: uid},
		Spec: v1alpha1.IntentSpec{Project: "demo"},
		Status: v1alpha1.IntentStatus{Phase: v1alpha1.IntentInReview,
			PullRequests: []v1alpha1.IntentPullRequest{{Repository: "https://github.com/acme/demo",
				State: "open", HeadSHA: testSHA}}}}
	p := &v1alpha1.Preview{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy", UID: types.UID("preview-" + name),
			Generation: 1, CreationTimestamp: metav1.NewTime(at)},
		Spec: v1alpha1.PreviewSpec{
			IntentRef: v1alpha1.ObjectReference{Name: name, UID: uid}, HostLabel: name,
			Components: []v1alpha1.PreviewComponent{{Name: "demo",
				ImageRepository: testSettings().ImagePrefix + "demo", Revision: testSHA,
				Port: 8080, ReadinessPath: "/health"}}, TTL: metav1.Duration{Duration: 72 * time.Hour},
		},
	}
	return in, p
}

type testEnv struct {
	r   *Reconciler
	c   client.Client
	now time.Time
	t   *testing.T
}

func newTestEnv(t *testing.T, objects ...client.Object) *testEnv {
	t.Helper()
	scheme := kube.Scheme()
	project := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "patchy"},
		Spec: v1alpha1.ProjectSpec{
			Repositories: []v1alpha1.ProjectRepository{{Name: "demo", URL: "https://github.com/acme/demo"}},
			Preview: &v1alpha1.ProjectPreview{ImageRepository: testSettings().ImagePrefix + "demo",
				Port: 8080, ReadinessPath: "/health"},
		}}
	objects = append(objects, project)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Preview{}, &v1alpha1.Intent{}, &appsv1.Deployment{}, &corev1.Pod{}).
		WithObjects(objects...).Build()
	e := &testEnv{c: c, now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), t: t}
	e.r = &Reconciler{Client: c, Settings: testSettings(), Now: func() time.Time { return e.now }}
	return e
}

func (e *testEnv) step(name string) *v1alpha1.Preview {
	e.t.Helper()
	_, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "patchy", Name: name,
	}})
	if err != nil {
		e.t.Fatalf("reconcile %s: %v", name, err)
	}
	p := &v1alpha1.Preview{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "patchy", Name: name}, p); err != nil {
		e.t.Fatalf("get preview %s: %v", name, err)
	}
	return p
}

func (e *testEnv) untilSlot(name string) *v1alpha1.Preview {
	e.t.Helper()
	for range 6 {
		p := e.step(name)
		if p.Status.Slot != nil {
			return p
		}
	}
	e.t.Fatalf("%s never got a slot", name)
	return nil
}

func TestFixedManifestSecurity(t *testing.T) {
	_, p := testPreview("demo-1", time.Now())
	s := testSettings()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := s.validatePreview(p); err != nil {
		t.Fatal(err)
	}
	assertFixedPodSecurity(t, s, p)
	svc := s.service(p, 0, 0)
	if svc.Spec.Type != corev1.ServiceTypeClusterIP || svc.Spec.Ports[0].Port != 80 {
		t.Errorf("service = %+v", svc.Spec)
	}
	ing := s.ingress(p, 0)
	if *ing.Spec.IngressClassName != "alb-preview" || ing.Spec.Rules[0].Host != "demo-1."+s.HostSuffix ||
		len(ing.Spec.TLS) != 0 || len(ing.Spec.Rules) != 1 || len(ing.Annotations) != 1 {
		t.Errorf("ingress = %+v", ing)
	}
	for _, bad := range []string{"nginx", s.ImagePrefix + "demo:latest", "evil.example/patchy/previews/demo"} {
		copy := p.DeepCopy()
		copy.Spec.Components[0].ImageRepository = bad
		if s.validatePreview(copy) == nil {
			t.Errorf("accepted unsafe image repository %q", bad)
		}
	}
}

func assertFixedPodSecurity(t *testing.T, s Settings, p *v1alpha1.Preview) {
	t.Helper()
	d := s.deployment(p, 0, 0)
	pod := d.Spec.Template.Spec
	if d.Namespace != "patchy-preview-0" || pod.NodeSelector["karpenter.sh/nodepool"] != s.NodePool ||
		pod.NodeSelector["eks.amazonaws.com/nodeclass"] != s.NodeClass ||
		len(pod.Tolerations) != 1 || pod.Tolerations[0].Key != s.TaintKey ||
		pod.Tolerations[0].Effect != corev1.TaintEffectNoExecute ||
		pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
		pod.ServiceAccountName != "default" || len(pod.InitContainers) != 0 || len(pod.Volumes) != 0 {
		t.Fatalf("rendered pod escapes placement/isolation: %+v", pod)
	}
	if got := pod.Containers[0].Image; got != testSettings().ImagePrefix+"demo:sha-"+testSHA {
		t.Errorf("image = %q", got)
	}
	if pod.Containers[0].SecurityContext.AllowPrivilegeEscalation == nil ||
		*pod.Containers[0].SecurityContext.AllowPrivilegeEscalation {
		t.Fatal("container allows privilege escalation")
	}
}

func TestQueueAndOrphanHold(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	i0, p0 := testPreview("demo-1", at)
	i1, p1 := testPreview("demo-2", at.Add(time.Second))
	i2, p2 := testPreview("demo-3", at.Add(2*time.Second))
	e := newTestEnv(t, i0, p0, i1, p1, i2, p2)
	if slot := *e.untilSlot(p0.Name).Status.Slot; slot != 0 {
		t.Fatalf("first slot = %d", slot)
	}
	if slot := *e.untilSlot(p1.Name).Status.Slot; slot != 1 {
		t.Fatalf("second slot = %d", slot)
	}
	for range 4 {
		p2 = e.step(p2.Name)
	}
	if p2.Status.Slot != nil || p2.Status.Phase != v1alpha1.PreviewQueued {
		t.Fatalf("third preview = %+v, want queued", p2.Status)
	}
	// A vanished Preview can leave resources. Its slot is withheld until
	// the periodic sweep actually removes the orphan, then the queue moves.
	orphan := testSettings().deployment(p0, 0, 0)
	if err := e.c.Create(context.Background(), orphan); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(context.Background(), p0); err != nil {
		t.Fatal(err)
	}
	// Model an old controller/forced CR removal that left its workload:
	// bypassing the finalizer is not a normal path, but the orphan sweep
	// must still make this state recoverable.
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(p0), p0); err == nil {
		p0.Finalizers = nil
		if err := e.c.Update(context.Background(), p0); err != nil {
			t.Fatal(err)
		}
	}
	for range 4 {
		p2 = e.step(p2.Name)
	}
	if p2.Status.Slot != nil {
		t.Fatal("queue reused a slot while orphan code remained")
	}
	sweep := &Sweeper{Client: e.c, Settings: testSettings()}
	if err := sweep.SweepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if slot := *e.untilSlot(p2.Name).Status.Slot; slot != 0 {
		t.Errorf("slot after sweep = %d, want 0", slot)
	}
}

func TestHeadUpdateReadinessAndExpiry(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := testPreview("demo-1", at)
	e := newTestEnv(t, in, p)
	p = e.untilSlot(p.Name)
	e.step(p.Name) // create Service and Deployment
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "patchy-preview-0", Name: resourceName(p)}
	var dep appsv1.Deployment
	if err := e.c.Get(ctx, key, &dep); err != nil {
		t.Fatal(err)
	}
	dep.Status.ObservedGeneration = dep.Generation
	dep.Status.AvailableReplicas = 1
	if err := e.c.Status().Update(ctx, &dep); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-1-pod", Namespace: key.Namespace,
		Labels: labelsFor(p)}, Spec: dep.Spec.Template.Spec}
	if err := e.c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "demo", Ready: true, ImageID: "repo@sha256:123"}}
	if err := e.c.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	p = e.step(p.Name)
	if p.Status.Phase != v1alpha1.PreviewReady || p.Status.Components[0].ImageID != "repo@sha256:123" {
		t.Fatalf("ready status = %+v", p.Status)
	}
	var ing networkingv1.Ingress
	if err := e.c.Get(ctx, key, &ing); err != nil {
		t.Fatal(err)
	}
	// A new PR head must withdraw the old Ingress before rendering a new
	// Deployment. The old Pod cannot make the new revision Ready.
	p.Spec.Components[0].Revision = strings.Repeat("b", 40)
	e.updatePRHead(in, strings.Repeat("b", 40))
	p.Generation++
	if err := e.c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	p = e.step(p.Name)
	if p.Status.Phase != v1alpha1.PreviewPending || p.Status.URL != "" ||
		p.Status.ObservedRevision != strings.Repeat("b", 40) {
		t.Fatalf("head update status = %+v", p.Status)
	}
	if err := e.c.Get(ctx, key, &ing); err == nil {
		t.Fatal("old Ingress remained exposed during a PR-head update")
	}
	p = e.step(p.Name)
	if p.Status.Phase == v1alpha1.PreviewReady {
		t.Fatal("old Pod made new revision Ready")
	}
	e.now = at.Add(73 * time.Hour)
	p = e.step(p.Name)
	if p.Status.Phase != v1alpha1.PreviewExpired {
		t.Fatalf("phase = %s, want Expired", p.Status.Phase)
	}
	// The fake client does not run Kubernetes's foreground garbage
	// collector; model the ReplicaSet's Pod disappearing after deletion.
	if err := e.c.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		p = e.step(p.Name)
		if p.Status.Slot == nil {
			break
		}
	}
	if p.Status.Slot != nil {
		t.Fatal("expired preview held a slot after cleanup")
	}
}

func (e *testEnv) updatePRHead(in *v1alpha1.Intent, sha string) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(in), in); err != nil {
		e.t.Fatal(err)
	}
	in.Status.PullRequests[0].HeadSHA = sha
	if err := e.c.Status().Update(ctx, in); err != nil {
		e.t.Fatal(err)
	}
}

func TestRolloutRetriesAreBounded(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := testPreview("demo-1", at)
	e := newTestEnv(t, in, p)
	e.untilSlot(p.Name)
	for attempt := int32(1); attempt <= 3; attempt++ {
		e.step(p.Name)
		e.now = e.now.Add(11 * time.Minute)
		p = e.step(p.Name)
		if p.Status.Retries != attempt {
			t.Fatalf("attempt %d retries = %d", attempt, p.Status.Retries)
		}
	}
	if p.Status.Phase != v1alpha1.PreviewFailed {
		t.Fatalf("phase = %s, want Failed", p.Status.Phase)
	}
	for range 4 {
		p = e.step(p.Name)
		if p.Status.Slot == nil {
			break
		}
	}
	if p.Status.Slot != nil {
		t.Fatal("failed preview did not release its slot")
	}
	for range 3 {
		p = e.step(p.Name)
		if p.Status.Retries != 3 || p.Status.Slot != nil {
			t.Fatal("failed preview restarted without a new PR head")
		}
	}
}

func TestInvalidSpecFailsOnceAndReleasesSlot(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := testPreview("demo-1", at)
	e := newTestEnv(t, in, p)
	p = e.untilSlot(p.Name)
	e.step(p.Name) // create the slot resources
	p.Spec.Components[0].Revision = "not-a-sha"
	p.Generation++
	if err := e.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p = e.step(p.Name)
	if p.Status.Phase != v1alpha1.PreviewFailed {
		t.Fatalf("invalid spec phase = %s", p.Status.Phase)
	}
	for range 5 {
		p = e.step(p.Name)
		if p.Status.Slot == nil {
			break
		}
	}
	if p.Status.Slot != nil {
		t.Fatal("invalid preview retained its slot")
	}
	rv := p.ResourceVersion
	p = e.step(p.Name)
	if p.ResourceVersion != rv {
		t.Fatal("stable invalid preview updated status again, causing a hot loop")
	}
}

func TestRetiredSlotBlocksFinalizerCleanup(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := testPreview("demo-1", at)
	e := newTestEnv(t, in, p)
	p = e.untilSlot(p.Name)
	retired := int32(2)
	p.Status.Slot = &retired
	if err := e.c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	_, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)})
	if err == nil || !strings.Contains(err.Error(), "retired slot") {
		t.Fatalf("retired slot finalizer error = %v", err)
	}
}

func TestForgedPreviewCannotDeployUnapprovedImageOrHead(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := testPreview("demo-1", at)
	p.Spec.Components[0].ImageRepository = testSettings().ImagePrefix + "other"
	e := newTestEnv(t, in, p)
	e.step(p.Name) // finalizer
	p = e.step(p.Name)
	if p.Status.Phase != v1alpha1.PreviewFailed || p.Status.Slot != nil {
		t.Fatalf("forged image deployed: %+v", p.Status)
	}
	p.Spec.Components[0].ImageRepository = testSettings().ImagePrefix + "demo"
	p.Spec.Components[0].Revision = strings.Repeat("b", 40)
	p.Generation++
	if err := e.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p = e.step(p.Name)
	if p.Status.Phase != v1alpha1.PreviewFailed || p.Status.Slot != nil {
		t.Fatalf("unrecorded head deployed: %+v", p.Status)
	}
}

func TestForgedUpdateWithdrawsExistingIngressBeforeFailure(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := testPreview("demo-1", at)
	e := newTestEnv(t, in, p)
	p = e.untilSlot(p.Name)
	ing := testSettings().ingress(p, *p.Status.Slot)
	if err := e.c.Create(context.Background(), ing); err != nil {
		t.Fatal(err)
	}
	p.Spec.Components[0].ImageRepository = testSettings().ImagePrefix + "other"
	p.Generation++
	if err := e.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p = e.step(p.Name)
	if p.Status.Phase != v1alpha1.PreviewFailed {
		t.Fatalf("forged update phase = %s", p.Status.Phase)
	}
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(ing), ing); err == nil {
		t.Fatal("Ingress still exposed after unapproved Preview update")
	}
}
