// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package evalresults

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
)

var (
	unitGVK = metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "EvaluationUnit"}
	errBoom = errors.New("boom")
)

func unitNamed(name string) *v1alpha1.EvaluationUnit {
	return &v1alpha1.EvaluationUnit{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy", UID: "uid-u"}}
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPersistOwnerReferenceAndLabels(t *testing.T) {
	c := newClient(t).Build()
	ctx := context.Background()
	labels := map[string]string{v1alpha1.LabelEvaluation: "eval-9"}
	ref, err := Persist(ctx, c, "patchy", labels, unitNamed("eval-9-u001"), unitGVK, []byte(`{}`))
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: "patchy", Name: ref.Name}, &cm); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if cm.Labels[v1alpha1.LabelEvaluation] != "eval-9" {
		t.Errorf("labels = %v", cm.Labels)
	}
	if len(cm.OwnerReferences) != 1 {
		t.Fatalf("owner refs = %+v", cm.OwnerReferences)
	}
	o := cm.OwnerReferences[0]
	if o.Kind != "EvaluationUnit" || o.Name != "eval-9-u001" || o.UID != "uid-u" ||
		o.APIVersion != v1alpha1.GroupVersion.String() || o.Controller == nil || !*o.Controller {
		t.Errorf("owner ref = %+v", o)
	}
	if _, ok := cm.BinaryData[DataKey]; !ok {
		t.Errorf("binaryData missing %q: %v", DataKey, cm.BinaryData)
	}
}

func TestPersistClientErrors(t *testing.T) {
	alreadyExists := apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, "x")
	tests := []struct {
		name     string
		create   error
		update   error
		wantErr  string
		wantCall bool // whether Update must be attempted
	}{
		{name: "create fails", create: errBoom, wantErr: "evalresults: write u-results"},
		{
			name: "rewrite fails", create: alreadyExists, update: errBoom,
			wantErr: "evalresults: rewrite u-results", wantCall: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updated := false
			c := newClient(t).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					return tt.create
				},
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					updated = true
					return tt.update
				},
			}).Build()
			ref, err := Persist(context.Background(), c, "patchy", nil, unitNamed("u"), unitGVK, []byte("x"))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !errors.Is(err, errBoom) {
				t.Fatalf("Persist err = %v, want %q wrapping boom", err, tt.wantErr)
			}
			if ref != nil {
				t.Errorf("ref = %+v, want nil on error", ref)
			}
			if updated != tt.wantCall {
				t.Errorf("Update called = %v, want %v", updated, tt.wantCall)
			}
		})
	}
}

func TestLoadGetError(t *testing.T) {
	c := newClient(t).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errBoom
		},
	}).Build()
	entry, err := Load(context.Background(), c, "patchy", "x-results")
	if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "evalresults: read x-results") {
		t.Errorf("Load err = %v, want wrapped read error", err)
	}
	if entry != nil {
		t.Errorf("entry = %q, want nil", entry)
	}
}

func TestLoadObjectShapes(t *testing.T) {
	full := gz(t, []byte(`{"ok":true}`))
	tests := []struct {
		name      string
		data      map[string][]byte
		want      string
		wantNil   bool
		wantErrIn string
	}{
		{name: "missing key", data: nil, wantNil: true},
		{name: "empty value", data: map[string][]byte{DataKey: {}}, wantNil: true},
		{name: "other key only", data: map[string][]byte{"other": full}, wantNil: true},
		{name: "not gzip", data: map[string][]byte{DataKey: []byte("plain text")}, wantErrIn: "evalresults: decompress r"},
		{
			name: "truncated gzip", data: map[string][]byte{DataKey: full[:len(full)-6]},
			wantErrIn: "evalresults: decompress r",
		},
		{name: "valid", data: map[string][]byte{DataKey: full}, want: `{"ok":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t).WithObjects(&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "patchy"},
				BinaryData: tt.data,
			}).Build()
			got, err := Load(context.Background(), c, "patchy", "r")
			if tt.wantErrIn != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrIn) {
					t.Fatalf("Load err = %v, want containing %q", err, tt.wantErrIn)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if tt.wantNil {
				if got != nil {
					t.Errorf("Load = %q, want nil", got)
				}
				return
			}
			if string(got) != tt.want {
				t.Errorf("Load = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLoadBoundsDecompression: a hand-edited object that inflates past the
// cap is read only up to maxDecodedBytes.
func TestLoadBoundsDecompression(t *testing.T) {
	big := bytes.Repeat([]byte("a"), maxDecodedBytes+1024)
	c := newClient(t).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "big", Namespace: "patchy"},
		BinaryData: map[string][]byte{DataKey: gz(t, big)},
	}).Build()
	got, err := Load(context.Background(), c, "patchy", "big")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != maxDecodedBytes {
		t.Errorf("Load returned %d bytes, want the %d cap", len(got), maxDecodedBytes)
	}
}
