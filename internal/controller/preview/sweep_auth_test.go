// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// sweepFixture is a two-slot install with auth required: a live Preview in
// slot 0 whose Ingress is still the pre-auth render, and in slot 1 every
// other kind of Ingress the backstop meets.
type sweepFixture struct {
	t     *testing.T
	c     client.Client
	s     *Sweeper
	rec   *events.FakeRecorder
	now   time.Time
	owned *v1alpha1.Preview
}

func newSweepFixture(t *testing.T, required bool) *sweepFixture {
	t.Helper()
	_, live := testPreview("demo-1", time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
	slot := int32(0)
	live.Status.Slot = &slot
	settings := authSettings()
	settings.Auth.Previous = testAuth(2, 0)
	if !required {
		settings = testSettings()
	}
	class := previewIngressClass
	other := "alb-edge"
	plain := func(ns, name string, ann map[string]string, className *string) *networkingv1.Ingress {
		return &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(ns + "/" + name), Annotations: ann},
			Spec:       networkingv1.IngressSpec{IngressClassName: className},
		}
	}
	withSet := func(set map[string]string) map[string]string {
		out := maps.Clone(set)
		out[annotationHealthcheck] = "/"
		return out
	}
	objects := []client.Object{
		live,
		// The live Preview's own Ingress, rendered before auth.
		testSettings().ingress(live, 0),
		// The chart's placeholder, before the chart renders it with auth.
		plain("patchy-preview-0", placeholderName, map[string]string{"helm.sh/resource-policy": "keep"}, &class),
		// An Ingress no Preview owns, unlabelled, unauthenticated.
		plain("patchy-preview-1", "stray", nil, &class),
		// The same through the legacy class annotation.
		plain("patchy-preview-1", "legacy", map[string]string{"kubernetes.io/ingress.class": class}, nil),
		// Another class: not the preview load balancer's.
		plain("patchy-preview-1", "elsewhere", nil, &other),
		// The current generation, and the previous one during a rotation.
		plain("patchy-preview-1", "current", withSet(testAuth(2, 1)["patchy-preview-1"]), &class),
		plain("patchy-preview-1", "previous", withSet(testAuth(2, 0)["patchy-preview-1"]), &class),
		// Slot 0's set in slot 1: another slot's cookie.
		plain("patchy-preview-1", "slot-0-set", withSet(testAuth(2, 1)["patchy-preview-0"]), &class),
	}
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(objects...).Build()
	f := &sweepFixture{t: t, c: c, rec: events.NewFakeRecorder(100), now: time.Date(2026, 10, 1, 12, 0, 0, 0,
		time.UTC), owned: live}
	f.s = &Sweeper{Client: c, Settings: settings, Events: f.rec, Now: func() time.Time { return f.now }}
	return f
}

func (f *sweepFixture) sweep() {
	f.t.Helper()
	if err := f.s.SweepOnce(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *sweepFixture) present() map[string]bool {
	f.t.Helper()
	var list networkingv1.IngressList
	if err := f.c.List(context.Background(), &list); err != nil {
		f.t.Fatal(err)
	}
	out := map[string]bool{}
	for _, ing := range list.Items {
		out[ing.Namespace+"/"+ing.Name] = true
	}
	return out
}

func (f *sweepFixture) events() []string {
	var out []string
	for {
		select {
		case ev := <-f.rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// TestSweeperReportsUnauthenticatedIngressesBeforeDeleting pins critique F7.
// The backstop reports first: a slot Ingress of the preview class without
// either generation's pinned set is reported (Event on its live Preview) and
// left alone for unauthenticatedGrace poll intervals, in which the owning
// reconcile, polling at the same interval, patches a live one. Only past
// that is it deleted; never the Helm-owned placeholder, never one on the
// current or previous generation, never another class's.
func TestSweeperReportsUnauthenticatedIngressesBeforeDeleting(t *testing.T) {
	f := newSweepFixture(t, true)
	all := f.present()
	interval := f.s.Settings.PollInterval
	for poll := range unauthenticatedGrace {
		f.sweep()
		if got := f.present(); !maps.Equal(got, all) {
			t.Fatalf("poll %d, within the grace period: Ingresses %v, want all of %v", poll, got, all)
		}
		f.now = f.now.Add(interval)
	}
	evs := f.events()
	if len(evs) != unauthenticatedGrace || !strings.Contains(evs[0], "Warning UnauthenticatedIngress ") ||
		!strings.Contains(evs[0], "patchy-preview-0/preview-demo-1") {
		t.Errorf("events = %q, want one Warning UnauthenticatedIngress on the live Preview per sweep", evs)
	}
	f.sweep() // the grace period is over
	got := f.present()
	for name, want := range map[string]bool{
		"patchy-preview-0/preview-demo-1":     false, // still unauthenticated: deleted
		"patchy-preview-0/" + placeholderName: true,
		"patchy-preview-1/stray":              false,
		"patchy-preview-1/legacy":             false,
		"patchy-preview-1/elsewhere":          true,
		"patchy-preview-1/current":            true,
		"patchy-preview-1/previous":           true,
		"patchy-preview-1/slot-0-set":         false,
	} {
		if got[name] != want {
			t.Errorf("%s present = %v, want %v", name, got[name], want)
		}
	}
	if evs := f.events(); len(evs) != 1 || !strings.Contains(evs[0], "Warning UnauthenticatedIngressDeleted") {
		t.Errorf("events = %q, want one UnauthenticatedIngressDeleted", evs)
	}
	// The placeholder stays reported, never deleted, however long.
	f.now = f.now.Add(10 * interval)
	f.sweep()
	if !f.present()["patchy-preview-0/"+placeholderName] {
		t.Error("the placeholder was deleted")
	}
}

// An Ingress patched to the pinned set within the grace period is never
// deleted, and one that later loses it starts a new grace period.
func TestSweeperGraceRestartsOnceConforming(t *testing.T) {
	f := newSweepFixture(t, true)
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "patchy-preview-0", Name: resourceName(f.owned)}
	setAnnotations := func(ann map[string]string) {
		t.Helper()
		var ing networkingv1.Ingress
		if err := f.c.Get(ctx, key, &ing); err != nil {
			t.Fatal(err)
		}
		ing.Annotations = ann
		if err := f.c.Update(ctx, &ing); err != nil {
			t.Fatal(err)
		}
	}
	interval := f.s.Settings.PollInterval
	f.sweep()
	f.now = f.now.Add(2 * interval)
	// The reconcile patches it to the pinned set: it conforms.
	setAnnotations(f.s.Settings.ingress(f.owned, 0).Annotations)
	f.sweep()
	f.now = f.now.Add(2 * interval)
	f.sweep()
	// It loses the set again: a new grace period, not the old one's end.
	setAnnotations(map[string]string{annotationHealthcheck: "/health"})
	f.sweep()
	f.now = f.now.Add(2 * interval)
	f.sweep()
	if err := f.c.Get(ctx, key, &networkingv1.Ingress{}); err != nil {
		t.Fatalf("deleted before a fresh grace period ran out: %v", err)
	}
	f.now = f.now.Add(interval)
	f.sweep()
	if err := f.c.Get(ctx, key, &networkingv1.Ingress{}); !kerrors.IsNotFound(err) {
		t.Fatalf("still present after the grace period: %v", err)
	}
}

// With auth off the backstop does nothing: the sweep is today's, deleting
// only a managed orphan by its rendered name and labels.
func TestSweeperWithAuthOffLeavesIngressesAlone(t *testing.T) {
	f := newSweepFixture(t, false)
	all := f.present()
	for range 2 * unauthenticatedGrace {
		f.sweep()
		f.now = f.now.Add(f.s.Settings.PollInterval)
	}
	if got := f.present(); !maps.Equal(got, all) {
		t.Errorf("Ingresses %v, want all of %v", got, all)
	}
	if evs := f.events(); len(evs) != 0 {
		t.Errorf("events = %q, want none", evs)
	}
}
