// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package kubeview

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

const ns = "patchy"

func preview(name, label, uid, project string, phase v1alpha1.PreviewPhase, slot *int32) *v1alpha1.Preview {
	return &v1alpha1.Preview{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid)},
		Spec:       v1alpha1.PreviewSpec{HostLabel: label, Project: project},
		Status:     v1alpha1.PreviewStatus{Phase: phase, Slot: slot},
	}
}

func ptr(v int32) *int32 { return &v }

func TestByLabel(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(
		preview("demo-7", "demo-7", "uid-7", "demo", v1alpha1.PreviewReady, ptr(1)),
		preview("demo-8", "other", "uid-8", "demo", v1alpha1.PreviewReady, ptr(0)),
		preview("demo-9", "demo-9", "uid-9", "demo", v1alpha1.PreviewExpired, ptr(0)),
		&v1alpha1.Preview{ObjectMeta: metav1.ObjectMeta{Name: "demo-1", Namespace: "elsewhere", UID: "x"},
			Spec:   v1alpha1.PreviewSpec{HostLabel: "demo-1", Project: "demo"},
			Status: v1alpha1.PreviewStatus{Phase: v1alpha1.PreviewReady, Slot: ptr(0)}},
	).Build()
	l := Lookup{Reader: c, Namespace: ns}
	tests := []struct {
		label string
		live  bool
		uid   string
	}{
		{"demo-7", true, "uid-7"},
		{"demo-8", false, ""},  // name and label differ
		{"demo-9", false, ""},  // expired
		{"demo-1", false, ""},  // another namespace
		{"missing", false, ""}, // no Preview
		{"Demo-7", false, ""},  // not a label
		{"a.b", false, ""},     // not a label
		{"placeholder", false, ""},
	}
	for _, tt := range tests {
		v, err := l.ByLabel(context.Background(), tt.label)
		if err != nil {
			t.Fatalf("%s: %v", tt.label, err)
		}
		if v.Live != tt.live || (tt.live && (v.UID != tt.uid || v.Project != "demo" || v.Slot != 1)) {
			t.Errorf("%s: view %+v", tt.label, v)
		}
	}
	ready, err := l.Ready(context.Background())
	if err != nil || len(ready) != 1 || ready[0].Label != "demo-7" {
		t.Fatalf("Ready = %+v, %v", ready, err)
	}
}

func TestByLabelReadFailureIsAnError(t *testing.T) {
	boom := errors.New("api down")
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return boom },
	}).Build()
	l := Lookup{Reader: c, Namespace: ns}
	if _, err := l.ByLabel(context.Background(), "demo-7"); !errors.Is(err, boom) {
		t.Fatalf("ByLabel err = %v", err)
	}
	if _, err := l.Ready(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Ready err = %v", err)
	}
}
