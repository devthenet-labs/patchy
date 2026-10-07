// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"errors"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
)

func preview(mutate func(*v1alpha1.Preview)) *v1alpha1.Preview {
	slot := int32(2)
	p := &v1alpha1.Preview{
		ObjectMeta: metav1.ObjectMeta{Name: "intent-7", UID: types.UID("uid-7")},
		Spec:       v1alpha1.PreviewSpec{HostLabel: "intent-7", Project: "demo"},
		Status:     v1alpha1.PreviewStatus{Phase: v1alpha1.PreviewReady, Slot: &slot},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

func TestViewOf(t *testing.T) {
	now := metav1.Now()
	for _, tc := range []struct {
		name   string
		mutate func(*v1alpha1.Preview)
		live   bool
	}{
		{"ready", nil, true},
		{"deploying", func(p *v1alpha1.Preview) { p.Status.Phase = v1alpha1.PreviewDeploying }, true},
		{"queued with a slot", func(p *v1alpha1.Preview) { p.Status.Phase = v1alpha1.PreviewQueued }, true},
		{"pending", func(p *v1alpha1.Preview) { p.Status.Phase = v1alpha1.PreviewPending }, false},
		{"failed", func(p *v1alpha1.Preview) { p.Status.Phase = v1alpha1.PreviewFailed }, false},
		{"expired", func(p *v1alpha1.Preview) { p.Status.Phase = v1alpha1.PreviewExpired }, false},
		{"no phase", func(p *v1alpha1.Preview) { p.Status.Phase = "" }, false},
		{"no slot", func(p *v1alpha1.Preview) { p.Status.Slot = nil }, false},
		{"slot out of range", func(p *v1alpha1.Preview) { s := int32(MaxSlots); p.Status.Slot = &s }, false},
		{"negative slot", func(p *v1alpha1.Preview) { s := int32(-1); p.Status.Slot = &s }, false},
		{"deleting", func(p *v1alpha1.Preview) { p.DeletionTimestamp = &now }, false},
		{"name is not the label", func(p *v1alpha1.Preview) { p.Spec.HostLabel = "other" }, false},
		{"no uid", func(p *v1alpha1.Preview) { p.UID = "" }, false},
	} {
		v := ViewOf(preview(tc.mutate))
		if v.Live != tc.live {
			t.Errorf("%s: Live = %v, want %v", tc.name, v.Live, tc.live)
		}
	}
	v := ViewOf(preview(nil))
	if want := (View{UID: "uid-7", Label: "intent-7", Project: "demo", Slot: 2, Live: true}); v != want {
		t.Errorf("ViewOf = %+v, want %+v", v, want)
	}
	if v := ViewOf(preview(func(p *v1alpha1.Preview) { p.Status.Slot = nil })); v.Slot != -1 {
		t.Errorf("a Preview with no slot has slot %d, want -1", v.Slot)
	}
}

func TestAdmit(t *testing.T) {
	v := liveView("uid-1", "intent-7", 1)
	for _, tc := range []struct {
		name  string
		label string
		slot  int
		v     View
		ok    bool
	}{
		{"match", "intent-7", 1, v, true},
		{"another slot's client", "intent-7", 0, v, false},
		{"another label", "intent-8", 1, v, false},
		{"not live", "intent-7", 1, View{UID: "uid-1", Label: "intent-7", Slot: 1}, false},
		{"no uid", "intent-7", 1, View{Label: "intent-7", Slot: 1, Live: true}, false},
		{"nothing there", "placeholder", 0, View{Slot: -1}, false},
	} {
		err := Admit(tc.label, tc.slot, tc.v)
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrNoPreview)) {
			t.Errorf("%s: Admit = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
}

// TestBindingProperty: a token bound to (client, slot, UID, label) matches a
// View exactly when the View is live with the same slot, UID and label, and is
// issued to exactly its own client.
func TestBindingProperty(t *testing.T) {
	cfg := quickConfig(5000)
	pick := func(r *rand.Rand, opts ...string) string { return opts[r.Intn(len(opts))] }
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		bs := r.Intn(MaxSlots)
		b := Bound{Client: ClientID(bs), Slot: bs, UID: pick(r, "u1", "u2"), Label: pick(r, "a", "b")}
		v := View{UID: pick(r, "u1", "u2"), Label: pick(r, "a", "b"), Slot: r.Intn(MaxSlots), Live: r.Intn(4) != 0,
			Project: "p"}
		args[0] = reflect.ValueOf(b)
		args[1] = reflect.ValueOf(v)
		args[2] = reflect.ValueOf(ClientID(r.Intn(MaxSlots)))
	}
	prop := func(b Bound, v View, client string) bool {
		want := v.Live && v.UID == b.UID && v.Label == b.Label && v.Slot == b.Slot
		if (b.Matches(v) == nil) != want {
			return false
		}
		return (b.IssuedTo(client) == nil) == (client == b.Client)
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func TestBoundMatchesErrors(t *testing.T) {
	b := Bound{Client: ClientID(1), Slot: 1, UID: "u1", Label: "a"}
	if err := b.Matches(View{UID: "u1", Label: "a", Slot: 1}); !errors.Is(err, ErrNoPreview) {
		t.Errorf("a Preview no longer live: %v, want ErrNoPreview", err)
	}
	if err := b.Matches(liveView("u2", "a", 1)); !errors.Is(err, ErrNotBound) {
		t.Errorf("a new Preview at the same label and slot: %v, want ErrNotBound", err)
	}
	forged := Bound{Client: ClientID(0), Slot: 1, UID: "u1", Label: "a"}
	if err := forged.Matches(liveView("u1", "a", 1)); !errors.Is(err, ErrNotBound) {
		t.Errorf("a binding whose client is another slot's: %v, want ErrNotBound", err)
	}
	if err := forged.IssuedTo(ClientID(0)); !errors.Is(err, ErrNotBound) {
		t.Errorf("IssuedTo on an inconsistent binding: %v, want ErrNotBound", err)
	}
}

func TestReviewFor(t *testing.T) {
	id := Identity{Username: "github:alice", Groups: []string{"github:org:team"}}
	got, err := ReviewFor(id, liveView("u", "a", 0))
	if err != nil {
		t.Fatal(err)
	}
	want := AccessReview{Username: "github:alice", Groups: []string{"github:org:team"}, Project: "demo",
		Subresource: "previews"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReviewFor = %+v, want %+v", got, want)
	}
	got.Groups[0] = "changed"
	if id.Groups[0] != "github:org:team" {
		t.Error("ReviewFor shares the identity's groups slice")
	}
	noProject := liveView("u", "a", 0)
	noProject.Project = ""
	if _, err := ReviewFor(id, noProject); !errors.Is(err, ErrNoProject) {
		t.Errorf("a Preview with no Project: %v, want ErrNoProject", err)
	}
	for _, bad := range []Identity{
		{}, {Username: "system:admin"}, {Username: "github:a", Groups: []string{"system:masters"}},
		{Username: "github:a", Groups: []string{""}},
	} {
		if _, err := ReviewFor(bad, liveView("u", "a", 0)); err == nil {
			t.Errorf("ReviewFor(%+v) built a review", bad)
		}
	}
}
